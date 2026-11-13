/*
 * sr_wg4_input.c - SR-WG Decap/Input Graph Node
 *
 * 处理流程：
 *   udp4-local dispatch -> sr_wg4_input (current_data 指向 SR Header)
 *   验证 SR Header -> HMAC 校验 -> 检查 seg_left==0 -> 剥离 SR Header -> wg4-input
 *
 * 注册在端口 51820（替代 wg4-input 的原始注册），处理完后交给 wg4-input。
 *
 * 向后兼容：本节点接管了 51820 的全部流量，包括未启用 SR-WG 的
 * 普通 WireGuard 包。对这类包（载荷首字节不是 SR_MAGIC）直接旁路
 * 到 wg4-input，保证 WG 直连模式不受影响。
 *
 * vBOND 观测：本节点同时是**外层源观测**的采集点。凡是我们接受并交给
 * wg4-input 的包（旁路的普通 WG 包 + 解封装成功的 SR 包），都记下它的
 * 外层源 (ip:port) 与本机目的端口 —— 这就是"NAT 把那台 CPE 的隧道 socket
 * 改成了什么"的唯一现场证据。丢包不记（见 Step 6 的注释）。
 */
#include "sr_wg.h"
#include "sr_qos.h"
#include "sr_fec.h"

/* ==================== Trace 定义 ==================== */

typedef struct {
  u32 next_index;
  u32 sw_if_index;
  u16 hmac_id;
  u16 path_id;
  u8  seg_left;
  u8  seg_count;
  u8  error;
  u8  bypass;   /* 1 = 非 SR 包，直通 wg4-input（WG 直连模式） */
} sr_wg4_input_trace_t;

static u8 *
format_sr_wg4_input_trace (u8 *s, va_list *args)
{
  CLIB_UNUSED (vlib_main_t * vm) = va_arg (*args, vlib_main_t *);
  CLIB_UNUSED (vlib_node_t * node) = va_arg (*args, vlib_node_t *);
  sr_wg4_input_trace_t *t = va_arg (*args, sr_wg4_input_trace_t *);

  if (t->bypass)
    s = format (s, "SR-WG-INPUT: plain-wireguard (no SR header) next=%u",
                t->next_index);
  else
    s = format (s, "SR-WG-INPUT: hmac_id=%u path_id=%u seg_left=%u/%u "
                   "err=%u next=%u",
                t->hmac_id, t->path_id, t->seg_left, t->seg_count,
                t->error, t->next_index);
  return s;
}

/* ==================== 错误枚举 ==================== */

#define foreach_sr_wg4_input_error                                       \
  _ (NONE, "no error")                                                   \
  _ (BAD_MAGIC, "bad magic byte")                                        \
  _ (BAD_VER, "unsupported version")                                     \
  _ (BAD_LEN, "header length mismatch")                                  \
  _ (HMAC_FAIL, "hmac verification failed")                              \
  _ (SEG_NOT_ZERO, "seg_left not zero at endpoint")                      \
  _ (NO_KEY, "no key found for hmac_id")                                 \
  _ (FEC_BAD, "fec parity unusable")

typedef enum {
#define _(sym, str) SR_WG4_INPUT_ERROR_##sym,
  foreach_sr_wg4_input_error
#undef _
    SR_WG4_INPUT_N_ERROR,
} sr_wg4_input_error_t;

static char *sr_wg4_input_error_strings[] = {
#define _(sym, str) str,
  foreach_sr_wg4_input_error
#undef _
};

/* ==================== Next nodes ==================== */

typedef enum {
  SR_WG4_INPUT_NEXT_DROP,
  SR_WG4_INPUT_NEXT_WG4_INPUT,
  SR_WG4_INPUT_N_NEXT,
} sr_wg4_input_next_t;

/* ==================== Node 注册 ==================== */

VLIB_REGISTER_NODE (sr_wg4_input_node) = {
  .name = "sr-wg4-input",
  .vector_size = sizeof (u32),
  .format_trace = format_sr_wg4_input_trace,
  .type = VLIB_NODE_TYPE_INTERNAL,
  /* 不置这个 flag 时 `trace add sr-wg4-input N` 直接报不支持 */
  .flags = VLIB_NODE_FLAG_TRACE_SUPPORTED,
  .n_errors = SR_WG4_INPUT_N_ERROR,
  .error_strings = sr_wg4_input_error_strings,
  .n_next_nodes = SR_WG4_INPUT_N_NEXT,
  .next_nodes = {
    [SR_WG4_INPUT_NEXT_DROP] = "error-drop",
    [SR_WG4_INPUT_NEXT_WG4_INPUT] = "wg4-input",
  },
};

/* ==================== Node Function ==================== */

VLIB_NODE_FN (sr_wg4_input_node) (vlib_main_t *vm, vlib_node_runtime_t *node,
                                  vlib_frame_t *frame)
{
  u32 n_left = frame->n_vectors;
  u32 *from = vlib_frame_vector_args (frame);
  vlib_buffer_t *bufs[VLIB_FRAME_SIZE];
  u16 nexts[VLIB_FRAME_SIZE];
  /* bufs/nexts 是数组，不可自增；用指针游标遍历 */
  vlib_buffer_t **buf_ptr = bufs;
  u16 *next_ptr = nexts;
  sr_wg_main_t *sm = &sr_wg_main;
  /*
   * 每批取一次时钟：观测表只需要秒级粒度（keepalive 几秒一轮），
   * 逐包读 rdtsc 是白给的热路径开销。
   * 注意 vlib_time_now 是**相对 VPP 启动**的单调秒数，不是墙钟；
   * dump 出去的时间戳由 agent 换算成 UTC 再上报控制器。
   */
  const u64 now_sec = (u64) vlib_time_now (vm);

  vlib_get_buffers (vm, from, bufs, n_left);

  while (n_left > 0)
    {
      vlib_buffer_t *b = buf_ptr[0];
      sr_header_t *srh;
      ip4_header_t *outer;
      udp_header_t *udp;
      sr_wg_key_entry_t *key_entry;
      u16 hmac_id;
      u16 sr_hdr_len;
      u32 pay_len;
      /*
       * trace 要展示的四个字段必须在 memmove **之前**取快照。
       *
       * Step 5 用 clib_memmove 把 WG 报文整体左移覆盖掉 SR 头（那是本节点的
       * 设计目标，不是缺陷：wg4-input 从 current_data 往回取 UDP 头，只 advance
       * 会留空洞）。而 trace 块在 done: 之后才跑，那时 srh 指向的已经是 WG
       * transport 报文 —— 实测解出来的 hmac_id 是 5120/1024/1280（=0x1400/0x0400/
       * 0x0500，WG 报文小端的 type/receiver 被当大端读）、seg_count 是 87/101
       * （=nonce 的随机字节），而同一批包的真值是 hmac_id=1 path_id=1 seg_count=2。
       * err/next/bypass 一直是好的，因为它们来自局部变量，不看包 —— 这正是这个
       * 缺陷能长期藏住的原因。
       */
      u16 tr_path_id = 0;
      u8 tr_seg_left = 0, tr_seg_count = 0, hdr_seen = 0;
      u32 next_index = SR_WG4_INPUT_NEXT_DROP;
      u8 error = SR_WG4_INPUT_ERROR_NONE;
      u8 bypass = 0;
      /* 还原帧标志：它填的是上一组的洞，不属于当前配对窗口（Step 5.6） */
      u8 fec_recovered = 0;

      /* current_data 指向 SR Header（udp4-local 已剥掉 eth/ip4/udp） */
      srh = (sr_header_t *) vlib_buffer_get_current (b);

      /*
       * 外层头反推：udp4-local 只是推进 current_data，缓冲区里的 IP/UDP 字节
       * 还在，所以从 SR Header 往回退 SR_OUTER_HDR_LEN 就取得到外层头。
       * Step 5 的长度修正与 done: 的观测记录都要用它，故在此一次算好。
       * 前提与 sr-wg4-transit 一致：外层 IPv4 头固定 20 字节（无选项）。
       * 旁路包同样成立 —— 它的 current_data 也是 UDP payload 起点。
       */
      outer = (ip4_header_t *) ((u8 *) srh - SR_OUTER_HDR_LEN);
      udp = (udp_header_t *) ((u8 *) outer + sizeof (ip4_header_t));

      /*
       * Step 0: WG 直连兼容旁路。
       *
       * 普通 WireGuard 包的 current 指向 WG transport header，其首字节
       * 是消息类型（handshake-init=1 / handshake-response=2 / cookie=3 /
       * transport-data=4），永不等于 SR_MAGIC(0x53)，可安全区分。
       * 不是 SR 包就直接交给 wg4-input，不做任何修改。
       */
      if (PREDICT_FALSE (b->current_length < SR_FIXED_HDR_LEN ||
                         srh->magic != SR_MAGIC))
        {
          bypass = 1;
          sm->counters.decap_bypass++;
          next_index = SR_WG4_INPUT_NEXT_WG4_INPUT;
          goto done;
        }

      /* Step 1: 验证 SR Header 基础字段 */
      if (sr_hdr_validate (srh, b->current_length) != 0)
        {
          if (b->current_length < SR_FIXED_HDR_LEN || srh->magic != SR_MAGIC)
            error = SR_WG4_INPUT_ERROR_BAD_MAGIC;
          else if ((srh->ver_flags & SR_VER_MASK) != (SR_VERSION << 4))
            error = SR_WG4_INPUT_ERROR_BAD_VER;
          else
            error = SR_WG4_INPUT_ERROR_BAD_LEN;
          sm->counters.drop_total++;
          goto done;
        }

      /* Step 2: 查找 P-Key */
      hmac_id = sr_hdr_get_hmac_id (srh);
      tr_path_id = sr_hdr_get_path_id (srh);
      tr_seg_left = srh->seg_left;
      tr_seg_count = srh->seg_count;
      hdr_seen = 1;
      key_entry = sr_wg_key_lookup (sm, hmac_id);
      if (!key_entry)
        {
          error = SR_WG4_INPUT_ERROR_NO_KEY;
          sm->counters.drop_total++;
          goto done;
        }

      /* Step 3: HMAC 验证 */
      if (sr_hmac_verify (srh, key_entry->key) != 0)
        {
          error = SR_WG4_INPUT_ERROR_HMAC_FAIL;
          sm->counters.decap_hmac_fail++;
          sm->counters.drop_total++;
          goto done;
        }

      /* Step 4: 检查 seg_left == 0（到达终点） */
      if (srh->seg_left != 0)
        {
          error = SR_WG4_INPUT_ERROR_SEG_NOT_ZERO;
          sm->counters.decap_seg_not_zero++;
          sm->counters.drop_total++;
          goto done;
        }

      /*
       * Step 4.5: 从 action 高 3bit 恢复 QoS 类别（owner 2026-11-13：必须步骤）。
       *
       * action 3bit 是端到端权威类别（运营商可能重标记外层 ToS，载荷不会动）。
       * 解密后的内层包带着这份元数据（VPP 标准 qos opaque2）走完本地转发，
       * LAN 出向物理口的 interface-output 调度直接用它分类 —— 接收端**不**
       * 重新从内层 DSCP 分类："DSCP→类别"的映射决策只在发送端入口做一次，
       * 链路两端映射表版本不一致也不会类别漂移。
       * 必须在 Step 5 剥 SR 头之前读（之后 srh 指向的内存被 WG 报文覆盖）。
       */
      vnet_buffer2 (b)->qos.bits = sr_qos_action_to_class (srh->action);
      b->flags |= VNET_BUFFER_F_QOS_DATA_VALID;

      /*
       * Step 5: 剥离 SR Header。
       *
       * 这里绝不能用 vlib_buffer_advance()。advance 只移动指针，SR Header 留下的
       * 空洞还在 UDP 头和 WG 报文之间，而 wg4-input 是从 current_data 往回取
       * 8 字节 UDP 头来定位本端接口的，取到的会是 SR Header 的尾部，于是整包
       * 报 "Interface error"（实测：encap/transit/decap 计数都正常涨了，
       * 但 wg4-input 全拒）。正确做法是把 WG 报文整体左移覆盖掉 SR Header，
       * 让它紧贴在 UDP 头之后，current_data 保持原位。
       *
       * 位移之后 datagram 真的变短了，所以外层 ip4->length / udp->length 必须
       * 同步减回 sr_hdr_len（encap Step 13 加过），否则 wg4-input 会按虚高的
       * 长度去解析越界数据。outer/udp 已在循环开头反推，memmove 不动
       * current_data，所以那两个指针此刻依然有效。
       */
      sr_hdr_len = sr_hdr_total_len (srh->seg_count);
      pay_len = b->current_length - sr_hdr_len;

      udp->length =
        clib_host_to_net_u16 (clib_net_to_host_u16 (udp->length) - sr_hdr_len);
      outer->length =
        clib_host_to_net_u16 (clib_net_to_host_u16 (outer->length) -
                              sr_hdr_len);
      outer->checksum = ip4_header_checksum (outer);

      clib_memmove (srh, (u8 *) srh + sr_hdr_len, pay_len);
      b->current_length = pay_len;

      /* current_data 仍指向原 SR Header 位置 = 现在的 WG transport header */

      sm->counters.decap_ok++;
      next_index = SR_WG4_INPUT_NEXT_WG4_INPUT;

      /*
       * Step 5.5: FEC 分流（插件 1.2.0，sr_fec.h 设计 2）。
       *
       * 冗余包复制成员的 SR 头、载荷首字节 = SR_FEC_MAGIC(0xF0)：HMAC 天然
       * 通过（action 类别位是排除字段）、中转照常转发，到这里 SR 头已剥、
       * current 指向 fec 载荷。还原成功时 buffer 已被原地换成完整 WG 帧
       * （长度/校验和都已修好），照常交给 wg4-input；还原不了就丢冗余包
       * 本身 —— 只丢冗余机会，不碰数据面。
       */
      if (PREDICT_FALSE (((u8 *) srh)[0] == SR_FEC_MAGIC))
	{
	  sr_wg_tunnel_bind_t *fbind =
	    sr_wg_bind_lookup (sm, clib_net_to_host_u16 (udp->dst_port));

	  if (fbind && fbind->fec_idx &&
	      sr_fec_rx_parity (sm, fbind, b, outer, udp) == 0)
	    {
	      fec_recovered = 1;
	      goto done;
	    }
	  error = SR_WG4_INPUT_ERROR_FEC_BAD;
	  sm->counters.drop_total++;
	  /* next_index 在上面的 Step 5 已被置成 WG4_INPUT：不显式改回 DROP，
	     fec 载荷会被当成 WG 帧送进 wg4-input。 */
	  next_index = SR_WG4_INPUT_NEXT_DROP;
	  goto done;
	}

    done:
      /*
       * Step 5.6: FEC 接收侧配对（sr_fec.h 设计 4）。
       *
       * 解封装成功的 SR 数据包把 WG 帧拷进所属隧道的配对环（计数法：
       * 冗余包到达时"自上个冗余包以来收到的帧数"决定能不能还原）。
       * 有 FEC 状态的隧道才拷 —— 每包一次 dst_port 哈希查表 + 最多
       * 1.5KB memcpy，是这项 feature 的主要收包成本；没开 FEC 的盒子
       * 由 sr_fec_active_count 的总开关短路。
       */
      if (PREDICT_FALSE (!bypass && !fec_recovered &&
			 next_index == SR_WG4_INPUT_NEXT_WG4_INPUT &&
			 sr_fec_active_count (sm) > 0))
	{
	  sr_wg_tunnel_bind_t *rbind =
	    sr_wg_bind_lookup (sm, clib_net_to_host_u16 (udp->dst_port));
	  if (rbind && rbind->fec_idx)
	    sr_fec_rx_data (rbind, b);
	}

      b->error = error ? node->errors[error] : 0;

      /*
       * Step 6: 外层源观测（vBOND 地址发现）。
       *
       * 只在**接受**（交给 wg4-input）时记录：HMAC 失败/无密钥/seg_left 非 0
       * 的包不是"某个 peer 的 socket 到了"的证据，记进表只会让陌生人挤掉
       * 真条目（表是 SR_OBS_SLOTS 槽的开放寻址，窗口占满才覆盖 home 槽；
       * evictions 会暴露这种干扰）。
       *
       * verified 的取值必须如实：**只有走完 SR HMAC 校验的解封装包**才算被
       * 认证（持有 p-key 的一方才签得出来；签名不覆盖外层 IP 头，认的是发送者
       * 而不是地址）。旁路的普通 WG 包此刻没有任何本地可验证的凭据 —— initiator
       * 的 static key 在密文里，wg4-input 要到我们之后才解密 —— 所以记 verified=0，
       * 控制器只能把它当候选映射，靠该节点自己的上报或"用它建隧道后握手真成功"
       * 来追认。
       */
      if (PREDICT_TRUE (next_index == SR_WG4_INPUT_NEXT_WG4_INPUT))
        sr_obs_record (&sm->obs,
                       clib_net_to_host_u32 (outer->src_address.as_u32),
                       clib_net_to_host_u16 (udp->src_port),
                       clib_net_to_host_u16 (udp->dst_port),
                       (u16) vnet_buffer (b)->sw_if_index[VLIB_RX], now_sec,
                       bypass ? 0 : 1);

      if (PREDICT_FALSE (b->flags & VLIB_BUFFER_IS_TRACED))
        {
          sr_wg4_input_trace_t *tr =
            vlib_add_trace (vm, node, b, sizeof (*tr));
          clib_memset (tr, 0, sizeof (*tr));
          tr->next_index = next_index;
          tr->sw_if_index = vnet_buffer (b)->sw_if_index[VLIB_RX];
          tr->error = error;
          tr->bypass = bypass;
          /*
           * 读上面的快照，绝不回读 srh。旁路包（bypass）与 Step 1 就掉的包
           * （magic/ver/len 不对）压根没读到过合法 SR 头，那三行保持全 0，
           * format 函数按 bypass 打 "plain-wireguard"。
           */
          if (hdr_seen)
            {
              tr->hmac_id = hmac_id;
              tr->path_id = tr_path_id;
              tr->seg_left = tr_seg_left;
              tr->seg_count = tr_seg_count;
            }
        }

      next_ptr[0] = next_index;

      n_left--;
      buf_ptr++;
      next_ptr++;
    }

  vlib_buffer_enqueue_to_next (vm, node, from, nexts, frame->n_vectors);
  return frame->n_vectors;
}
