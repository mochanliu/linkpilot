/*
 * sr_fec.c - 隧道内前向纠错（FEC）：XOR 块奇偶校验
 *
 * 设计与线格式见 sr_fec.h 顶部注释；决策记录在 docs/fec-and-lb-plan.md §9。
 *
 * 数据面入口三个：
 *   sr_fec_tx        —— sr-wg4-encap Step 14.5（封装成功后顺带异或，组满造冗余包）
 *   sr_fec_rx_data   —— sr-wg4-input 解封装成功路径（WG 帧拷进配对环）
 *   sr_fec_rx_parity —— sr-wg4-input Step 5.5（冗余包分流，原地还原或丢弃）
 *
 * 数据包永远直通：FEC 的一切失败只丢"冗余机会"，不碰数据面。
 */
#include "sr_fec.h"

#include <vnet/ip/ip4_packet.h>
#include <vnet/feature/feature.h>

/* ==================== 状态池 ==================== */

typedef struct
{
  sr_fec_state_t *states;	/* pool，bind->fec_idx = 池下标 + 1 */
  u32 active_count;		/* ratio > 0 的状态数（热路径总开关） */
} sr_fec_main_t;

static sr_fec_main_t sr_fec_main;

static sr_fec_state_t *
sr_fec_state_of (sr_wg_tunnel_bind_t *bind)
{
  if (bind->fec_idx == 0)
    return NULL;
  return pool_elt_at_index (sr_fec_main.states, bind->fec_idx - 1);
}

/* ==================== 管理面 ==================== */

int
sr_fec_set (sr_wg_main_t *sm, u32 sw_if_index, u8 ratio)
{
  uword *p;
  sr_wg_tunnel_bind_t *bind;
  sr_fec_state_t *st;
  u8 was;

  if (ratio > SR_FEC_MAX_MEMBERS)
    return VNET_API_ERROR_INVALID_VALUE;

  sr_wg_encap_init (sm);
  p = hash_get (sm->tunnel_path, (uword) sw_if_index);
  if (!p || p[0] == 0)
    return VNET_API_ERROR_NO_SUCH_ENTRY;
  bind = pool_elt_at_index (sm->binds, p[0] - 1);

  if (ratio == bind->fec_ratio)
    return 0;			/* 幂等 */

  was = bind->fec_ratio;

  if (ratio == 0)
    {
      /* 关闭：回收状态。半组直接丢 —— 数据不等 FEC（sr_fec.h 设计 3）。 */
      st = sr_fec_state_of (bind);
      if (st)
	{
	  u32 idx = bind->fec_idx - 1;
	  clib_memset (st, 0, sizeof (*st));
	  pool_put_index (sr_fec_main.states, idx);
	}
      bind->fec_idx = 0;
      bind->fec_ratio = 0;
      if (was)
	sr_fec_main.active_count--;
      return 0;
    }

  /* 开启 / 改比例：状态必须存在（首次创建），ratio 只改个数字 */
  st = sr_fec_state_of (bind);
  if (st == NULL)
    {
      pool_get_zero (sr_fec_main.states, st);
      bind->fec_idx = (u32) (st - sr_fec_main.states) + 1;
      st->sw_if_index = sw_if_index;
      /*
       * 组序号的基址取墙上时间（秒），不是 0。
       *
       * 理由：进程重启 / 换件重放之后，如果 tx_seq 从 1 重来，对端的
       * rx_last_seq 还停在上一代那个大数上，于是**每一个冗余包**都被判
       * "陈旧"丢掉 —— FEC 在谁也没察觉的情况下整段失效，直到对端也重启
       * 为止（2026-11-15 换件实测：发端重启后 530 个冗余包全 bad、
       * complete 0）。取时钟当基址，重启那一侧的序号直接跳到当下，
       * 对端要么把它当新组边界收下（seq 前进），要么走 rx 侧的大跨度
       * 回退重新同步（SR_FEC_RESEQ_SLACK）。
       */
      st->tx_seq = (u32) unix_time_now ();
      sr_fec_main.active_count++;
    }
  st->ratio = ratio;
  bind->fec_ratio = ratio;
  return 0;
}

u32
sr_fec_active_count (sr_wg_main_t *sm)
{
  (void) sm;
  return sr_fec_main.active_count;
}

/* ==================== 发送侧 ==================== */

/*
 * 造冗余包并返回 buffer index。失败返回 ~0（只丢冗余机会）。
 *
 * 冗余包布局（current 与成员一致，headroom 同宽）：
 *   [IP4|UDP|SR]（逐字拷贝成员，IP ToS=0、SR action 类别位=0 —— 两者都在
 *   HMAC 排除集/免签名字段，不破坏校验）+ fec 头(10B) + 异或体(tx_pad B)
 */
static u32
sr_fec_build_parity (vlib_main_t *vm, sr_fec_state_t *st,
		     vlib_buffer_t *member, u8 *pkt, u32 outer_len,
		     u16 sr_len, u32 fib_index)
{
  vlib_buffer_t *pb;
  u32 pi = (u32) ~0;
  u8 *p;
  u32 fec_total;
  u16 len_xor = 0;
  int i;

  if (vlib_buffer_alloc (vm, &pi, 1) != 1)
    return (u32) ~0;
  pb = vlib_get_buffer (vm, pi);

  pb->current_data = member->current_data;
  pb->current_length = outer_len + sr_len;
  p = (u8 *) vlib_buffer_get_current (pb);

  /* 外层头 + SR 头逐字拷贝（含 ip4_len 可能的选项字节） */
  clib_memcpy_fast (p, pkt, outer_len + sr_len);

  fec_total = SR_FEC_HDR_LEN + st->tx_pad;
  pb->current_length += fec_total;

  /* fec 头（网络序） */
  {
    ip4_header_t *ip4 = (ip4_header_t *) p;
    udp_header_t *udp = (udp_header_t *) (p + (outer_len - sizeof
					       (udp_header_t)));
    u8 *fh = p + outer_len + sr_len;
    u32 seq_net = clib_host_to_net_u32 (st->tx_seq);
    u16 pad_net = clib_host_to_net_u16 (st->tx_pad);
    u16 lx_net;

    fh[0] = SR_FEC_MAGIC;
    fh[1] = st->tx_n;
    clib_memcpy_fast (fh + 2, &seq_net, 4);
    for (i = 0; i < st->tx_n; i++)
      len_xor ^= st->tx_len[i];
    lx_net = clib_host_to_net_u16 (len_xor);
    clib_memcpy_fast (fh + 6, &lx_net, 2);
    clib_memcpy_fast (fh + 8, &pad_net, 2);

    /* 异或体 */
    clib_memcpy_fast (fh + SR_FEC_HDR_LEN, st->tx_acc, st->tx_pad);

    /*
     * 头部修整：ToS 与 action 类别位清零（冗余包按 BE 走 QoS 调度，见
     * sr_fec.h 设计 2；action 在 HMAC 排除集、ToS 是外层字段，都免重签）。
     */
    ip4->tos = 0;
    {
      u8 *srh = p + outer_len;
      srh[5] &= (u8) ~ (0x7 << 5);	/* action 高 3bit = 类别位 */
    }
    /* 长度与校验和 */
    udp->length =
      clib_host_to_net_u16 ((u16) (8 + sr_len + fec_total));
    udp->checksum = 0;
    /* 外层 IP 总长 = IP4+UDP + SR 头 + fec 头 + 异或体（step 12 给成员加过
     * sr_len，冗余包同样要算进去，漏掉它下一跳会按截断长度解析） */
    ip4->length =
      clib_host_to_net_u16 ((u16) (outer_len + sr_len + fec_total));
    ip4->checksum = ip4_header_checksum (ip4);
  }

  /*
   * 与成员包 Step 14 同款：VLIB_TX = FIB index（26.06 的 ip4-lookup 把它当
   * FIB 下标用），旧邻接清掉让重查表写回正确的第一跳邻接。
   */
  vnet_buffer (pb)->sw_if_index[VLIB_TX] = fib_index;
  vnet_buffer (pb)->ip.adj_index[VLIB_TX] = ~0;
  vnet_buffer (pb)->ip.fib_index = fib_index;
  pb->flags &= ~(VNET_BUFFER_F_L2_HDR_OFFSET_VALID |
		 VNET_BUFFER_F_L3_HDR_OFFSET_VALID |
		 VNET_BUFFER_F_L4_HDR_OFFSET_VALID);

  st->parity_sent++;
  st->parity_bytes += pb->current_length;
  return pi;
}

u32
sr_fec_tx (vlib_main_t *vm, sr_wg_main_t *sm, sr_wg_tunnel_bind_t *bind,
	   vlib_buffer_t *member, u8 *pkt, u32 outer_len, u16 sr_len,
	   u16 wg_len, u32 fib_index)
{
  sr_fec_state_t *st;
  u8 *frame;
  int i;

  (void) sm;
  st = sr_fec_state_of (bind);
  if (st == NULL || st->ratio == 0)
    return (u32) ~0;

  /*
   * ratio 1 = 整包复制：冗余包就是成员包的完整克隆（外层 + WG 帧原样），
   * 对端的 WG 反回放按计数器天然去重 —— 走普通 WG 包形态，不进 fec 头格式。
   */
  if (st->ratio == 1)
    {
      u32 pi = (u32) ~0;
      vlib_buffer_t *pb;

      if (vlib_buffer_alloc (vm, &pi, 1) != 1)
	return (u32) ~0;
      pb = vlib_get_buffer (vm, pi);
      pb->current_data = member->current_data;
      pb->current_length = member->current_length;
      clib_memcpy_fast (vlib_buffer_get_current (pb),
			vlib_buffer_get_current (member),
			member->current_length);
      vnet_buffer (pb)->sw_if_index[VLIB_TX] = fib_index;
      vnet_buffer (pb)->ip.adj_index[VLIB_TX] = ~0;
      vnet_buffer (pb)->ip.fib_index = fib_index;
      pb->flags &= ~(VNET_BUFFER_F_L2_HDR_OFFSET_VALID |
		     VNET_BUFFER_F_L3_HDR_OFFSET_VALID |
		     VNET_BUFFER_F_L4_HDR_OFFSET_VALID);
      st->parity_sent++;
      st->parity_bytes += pb->current_length;
      return pi;
    }

  if (wg_len == 0 || wg_len > SR_FEC_MAX_FRAME)
    {
      st->too_big++;
      return (u32) ~0;
    }

  /* 成员帧异或进组（帧尾超 pad 的历史残留不影响：XOR 按 wg_len 字节进行） */
  frame = pkt + outer_len + sr_len;
  for (i = 0; i < wg_len; i++)
    st->tx_acc[i] ^= frame[i];
  if (wg_len > st->tx_pad)
    st->tx_pad = wg_len;
  st->tx_len[st->tx_n] = wg_len;
  st->tx_n++;

  if (st->tx_n < st->ratio)
    return (u32) ~0;

  /* 组满：补冗余包，然后重置组（acc 必须清零 —— 它是组内异或，不是累计） */
  {
    u32 pi = sr_fec_build_parity (vm, st, member, pkt, outer_len,
				  sr_len, fib_index);
    st->tx_seq++;
    st->tx_n = 0;
    st->tx_pad = 0;
    clib_memset (st->tx_acc, 0, sizeof (st->tx_acc));
    return pi;
  }
}

/* ==================== 接收侧 ==================== */

void
sr_fec_rx_data (sr_wg_tunnel_bind_t *bind, vlib_buffer_t *b)
{
  sr_fec_state_t *st = sr_fec_state_of (bind);
  u16 len = b->current_length;

  if (st == NULL || st->rx_j >= SR_FEC_MAX_MEMBERS)
    return;			/* 环满：这组本来也恢复不了 */
  if (len == 0 || len > SR_FEC_MAX_FRAME)
    return;
  clib_memcpy_fast (st->rx_ring[st->rx_j],
		    (u8 *) vlib_buffer_get_current (b), len);
  st->rx_len[st->rx_j] = len;
  st->rx_j++;
}

int
sr_fec_rx_parity (sr_wg_main_t *sm, sr_wg_tunnel_bind_t *bind,
		  vlib_buffer_t *b, ip4_header_t *outer, udp_header_t *udp)
{
  sr_fec_state_t *st = sr_fec_state_of (bind);
  u8 *cur = (u8 *) vlib_buffer_get_current (b);
  u32 plen = b->current_length;
  u8 k;
  u32 seq;
  u16 len_xor, pad;
  u32 j;
  int i;

  (void) sm;
  if (st == NULL || plen < SR_FEC_HDR_LEN)
    {
      if (st)
	st->bad_parity++;
      return -1;
    }

  k = cur[1];
  clib_memcpy_fast (&seq, cur + 2, 4);
  clib_memcpy_fast (&len_xor, cur + 6, 2);
  clib_memcpy_fast (&pad, cur + 8, 2);
  seq = clib_net_to_host_u32 (seq);
  len_xor = clib_net_to_host_u16 (len_xor);
  pad = clib_net_to_host_u16 (pad);

  if (k < 1 || k > SR_FEC_MAX_MEMBERS || pad == 0 ||
      pad > SR_FEC_MAX_FRAME || plen < (u32) (SR_FEC_HDR_LEN + pad))
    {
      /* 头不合法：配对窗口作废重新同步（数据包永远没被动过，无需处理） */
      st->bad_parity++;
      st->rx_j = 0;
      return -1;
    }

  /*
   * 陈旧冗余包（seq 没有前进）：不做任何配对动作，也不动计数窗口 ——
   * 它是重复投递，不是新组的边界。
   *
   * 两个例外都写在 SR_FEC_RESEQ_SLACK 的注释里：
   *   - d == 0        ：同一个包又到了一遍（重复投递/被镜像），丢。
   *   - d > -SLACK    ：轻微乱序（后一组的冗余包先到），丢，窗口不动。
   *   - d <= -SLACK   ：对端重启过、序号换了基址 —— 重新同步（rx_j 归零，
   *                     这个包当新组边界）。死守旧序号会让 FEC 永久失效。
   */
  if (st->rx_last_seq != 0)
    {
      i32 d = (i32) (seq - st->rx_last_seq);

      if (d == 0 || (d < 0 && d > -SR_FEC_RESEQ_SLACK))
	{
	  st->bad_parity++;
	  return -1;
	}
      if (d < 0)
	st->rx_j = 0;
    }

  j = st->rx_j;
  if (j == k)
    {
      /* 全到：这个冗余包白发了（对端按数发的，正常现象） */
      st->complete++;
      st->rx_j = 0;
      st->rx_last_seq = seq;
      return -1;		/* buffer 是冗余包本身，丢弃 */
    }
  else if (j == (u32) (k - 1) && j >= 1)
    {
      /*
       * 恰好缺 1：异或还原。缺失帧长度 = len_xor ^ 各在场帧长度（异或
       * 自反）。还原体直接在 buffer 里算：body 逐帧 ^= rx_ring 成员
       * （按各自长度，pad 之外的尾字节是 0 补齐，无需处理）。
       */
      u16 missing = len_xor;
      u8 *body = cur + SR_FEC_HDR_LEN;

      for (i = 0; i < (int) j; i++)
	{
	  u16 l = st->rx_len[i];
	  int n;
	  missing ^= st->rx_len[i];
	  for (n = 0; n < l; n++)
	    body[n] ^= st->rx_ring[i][n];
	}
      if (missing == 0 || missing > pad)
	{
	  /* 配对错位（乱序/半组跨周期）：还原出的是垃圾，WG 标签会拦，
	     但长度都不像话就没必要再走一趟解密 —— 丢弃并重新同步 */
	  st->unrecovered++;
	  st->rx_j = 0;
	  st->rx_last_seq = seq;
	  return -1;
	}

      /*
       * 把还原帧移到 current（wg4-input 从 current 往回取 UDP 头定位）。
       * current_length 只算 SR 头之后的部分 —— 与解封装成功的数据包一致
       * （Step 5 之后 current_length 就是 WG 帧长）：外层 IP4/UDP 不在
       * current 之前，多报 SR_OUTER_HDR_LEN 会让 wg4-input 按虚高长度
       * 解析到垃圾，还原帧全被 WG 标签拒掉。
       */
      clib_memmove (cur, cur + SR_FEC_HDR_LEN, missing);
      b->current_length = missing;
      udp->length = clib_host_to_net_u16 ((u16) (8 + missing));
      outer->length = clib_host_to_net_u16 ((u16) (SR_OUTER_HDR_LEN + missing));
      outer->checksum = ip4_header_checksum (outer);
      st->recovered++;
      st->rx_j = 0;
      st->rx_last_seq = seq;
      return 0;			/* buffer 已变成还原帧，送 wg4-input */
    }
  else
    {
      /* 组内多丢：放弃。窗口照常前移，别让下一组背这一组的锅 */
      st->unrecovered++;
      st->rx_j = 0;
      st->rx_last_seq = seq;
      return -1;
    }
}

/* ==================== CLI ==================== */

static clib_error_t *
sr_wg_set_fec_command_fn (vlib_main_t *vm, unformat_input_t *input,
			  vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;
  vnet_main_t *vnm = vnet_get_main ();
  u32 sw_if_index = (u32) ~0;
  u32 ratio = (u32) ~0;
  int rv;

  while (unformat_check_input (input) != UNFORMAT_END_OF_INPUT)
    {
      if (unformat (input, "off"))
	ratio = 0;
      else if (unformat (input, "%U", unformat_vnet_sw_interface, vnm,
			 &sw_if_index))
	;
      else if (unformat (input, "%u", &ratio))
	;
      else
	break;
    }

  if (sw_if_index == (u32) ~0)
    return clib_error_return (0, "usage: set sr-wg fec <隧道接口> <1-4 | off>");
  if (ratio == (u32) ~0)
    return clib_error_return (0, "missing <1-4 | off>");

  rv = sr_fec_set (sm, sw_if_index, (u8) ratio);
  if (rv != 0)
    return clib_error_return (0, "failed (rv=%d) —— 该接口可能还没有 SR 隧道绑定",
			      rv);

  if (ratio == 0)
    vlib_cli_output (vm, "%U: FEC off", format_vnet_sw_if_index_name, vnm,
		     sw_if_index);
  else
    vlib_cli_output (vm,
		     "%U: FEC ratio %u（每 %u 个数据包 1 个异或冗余包）",
		     format_vnet_sw_if_index_name, vnm, sw_if_index,
		     (u32) ratio, (u32) ratio);
  return 0;
}

/* *INDENT-OFF* */
VLIB_CLI_COMMAND (sr_wg_set_fec_command, static) = {
  .path = "set sr-wg fec",
  .short_help = "set sr-wg fec <隧道接口> <1-4 | off>",
  .function = sr_wg_set_fec_command_fn,
};
/* *INDENT-ON* */

static clib_error_t *
sr_wg_show_fec_command_fn (vlib_main_t *vm, unformat_input_t *input,
			   vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;
  vnet_main_t *vnm = vnet_get_main ();
  u32 bi;
  int any = 0;

  /* *INDENT-OFF* */
  pool_foreach_index (bi, sm->binds)
    {
      sr_wg_tunnel_bind_t *bind = pool_elt_at_index (sm->binds, bi);
      sr_fec_state_t *st = sr_fec_state_of (bind);
      u8 *ifname = format (0, "%U", format_vnet_sw_if_index_name, vnm,
			   bind->sw_if_index);

      if (st == NULL)
	{
	  vlib_cli_output (vm, "%v: FEC off", ifname);
	}
      else
	{
	  vlib_cli_output (vm,
			   "%v: ratio %u  seq %u  tx组内 %u/%u  rx窗口 %u  |  "
			   "parity %llu (%llu B)  recovered %llu  "
			   "complete %llu  unrecovered %llu  bad %llu  too_big %llu",
			   ifname, st->ratio, st->tx_seq, st->tx_n, st->ratio,
			   st->rx_j, st->parity_sent, st->parity_bytes,
			   st->recovered, st->complete, st->unrecovered,
			   st->bad_parity, st->too_big);
	}
      vec_free (ifname);
      any = 1;
    }
  /* *INDENT-ON* */

  if (!any)
    vlib_cli_output (vm, "（还没有 SR 隧道绑定）");
  return 0;
}

/* *INDENT-OFF* */
VLIB_CLI_COMMAND (sr_wg_show_fec_command, static) = {
  .path = "show sr-wg fec",
  .short_help = "show sr-wg fec",
  .function = sr_wg_show_fec_command_fn,
};
/* *INDENT-ON* */
