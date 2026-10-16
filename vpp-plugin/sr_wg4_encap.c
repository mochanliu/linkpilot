/*
 * sr_wg4_encap.c - SR-WG Encap Graph Node + 隧道-路径绑定管理
 *
 * ==================== 集成方式 ====================
 *
 * WireGuard 插件导出的符号全部是 hidden（无法 dlsym 直接调用），因此
 * SR-WG 与 WG 的集成只能通过 VPP 的 feature arc / graph node 串联完成。
 *
 * 实测（VPP 26.06-release，`show features` / `show interface features wg0`）：
 *   - 不存在名为 "ip4-output" 的 graph node，只存在同名的 feature **arc**；
 *   - 该 arc 的 start_nodes 是 ip4-midchain / ip4-mcast-midchain，
 *     arc 末尾是 adj-midchain-tx（负责父邻接的 L2 rewrite）；
 *   - WireGuard 插件自己把 "wg4-output-tun" 注册为该 arc 上的 feature，
 *     并在每个 wg 接口上启用：
 *         ip4-output:
 *           wg4-output-tun
 *
 * 所以 sr-wg4-encap 的正确挂点是同一个 arc、同一个（wg）接口，且必须
 * runs_after wg4-output-tun。完整数据流：
 *
 *   内网包 → ip4-lookup → FIB(vpn route) → ip4-midchain(tx=wg0, features)
 *          → wg4-output-tun（WG 加密 + 外层 IP4/UDP/WG-transport）
 *          → sr-wg4-encap（插入 SR Header + 改写外层 dst 为第一跳）
 *          → ip4-lookup（外层重新查表，得到第一跳对应的父邻接）
 *          → ip4-rewrite → interface-output → 物理网卡
 *
 * 未绑定 SR 路径时 sr-wg4-encap 用 vnet_feature_next() 直通，包继续沿
 * arc 走 adj-midchain-tx → ip4-rewrite，WG 直连模式完全不受影响。
 *
 * ==================== 节点处的缓冲区布局 ====================
 *
 * 由于 arc 起点 ip4-midchain 传下来的是纯 L3 包（L2 rewrite 由 arc 末尾
 * 的 adj-midchain-tx 负责），在 sr-wg4-encap 处 vlib_buffer_get_current(b)
 * 指向 **外层 IP4 头**，其后紧跟 UDP 头和 WG transport header：
 *
 *   current → [ outer IP4 | outer UDP | WG transport hdr | 加密载荷 ]
 *
 * 目标线格式（SR Header 插在 UDP 头与 WG 载荷之间）：
 *
 *   current → [ outer IP4 | outer UDP | SR Header | WG transport hdr | 载荷 ]
 *
 * 因此只需把 current 前移 sr_len 字节，再把原来的 outer_len(=ip4_len+8)
 * 字节整体前移，腾出的空间写入 SR Header；WG 载荷原地不动。
 *
 * ==================== 段列表 / seg_left 语义 ====================
 *
 * 与 sr_header.h、docs/sr-header-format.md 一致：
 *   segments[0]            = 最终目的节点（VPN 端口 51820）
 *   segments[seg_count-1]  = 第一跳中转（"首跳免费"，由外层 IP/UDP 承载）
 *   sr_hdr_get_next_hop()  = &segments[seg_left - 1]
 *   sr_hdr_pop_segment()   = 取走 segments[seg_left-1] 后 seg_left--
 *
 * 故 encap 必须：
 *   外层 IP4 dst      = segments[seg_count-1].ip
 *   外层 UDP dst_port = segments[seg_count-1].port
 *   seg_left          = seg_count - 1     （首跳已在外层，剩余待处理段数）
 *
 * 注意 sr_hdr_init()/sr_path_fill_header() 把 seg_left 初始化为 seg_count，
 * 必须在其之后覆盖为 seg_count-1；seg_left 不在 HMAC 覆盖范围内，
 * 覆盖后无需重新签名。
 *
 * 验证（seg_count=2: segments[0]=node2:51820, segments[1]=transit:51821）：
 *   encap   → seg_left=1，外层 dst=transit:51821
 *   transit → next_hop=segments[0]=node2:51820，seg_left 1→0，改写外层
 *   node2   → sr-wg4-input 见 seg_left==0 → 剥 SR 头 → wg4-input 解密
 */
#include "sr_wg.h"

#include <vnet/udp/udp_packet.h>
#include <vnet/ip/format.h>

/* ==================== Trace 定义 ==================== */

typedef struct {
  u32 next_index;
  u32 tx_sw_if_index;
  u16 path_id;
  u8  seg_count;
  u8  seg_left;
  u16 sr_len;
  ip4_address_t orig_dst;      /* 原外层 dst（WG peer endpoint） */
  ip4_address_t first_hop_ip;  /* 改写后的外层 dst */
  u16 first_hop_port;
  i16 current_data;            /* 用于观测 headroom 余量 */
  u8  error;
  u8  bypass;
} sr_wg4_encap_trace_t;

static u8 *
format_sr_wg4_encap_trace (u8 *s, va_list *args)
{
  CLIB_UNUSED (vlib_main_t * vm) = va_arg (*args, vlib_main_t *);
  CLIB_UNUSED (vlib_node_t * node) = va_arg (*args, vlib_node_t *);
  sr_wg4_encap_trace_t *t = va_arg (*args, sr_wg4_encap_trace_t *);

  if (t->bypass)
    s = format (s, "SR-WG-ENCAP: bypass sw_if_index=%u orig_dst=%U "
                   "err=%u next=%u",
                t->tx_sw_if_index, format_ip4_address, &t->orig_dst,
                t->error, t->next_index);
  else
    s = format (s, "SR-WG-ENCAP: path_id=%u seg_count=%u seg_left=%u "
                   "sr_len=%u orig_dst=%U -> first_hop=%U:%u "
                   "cd=%d err=%u next=%u",
                t->path_id, t->seg_count, t->seg_left, t->sr_len,
                format_ip4_address, &t->orig_dst,
                format_ip4_address, &t->first_hop_ip, t->first_hop_port,
                t->current_data, t->error, t->next_index);
  return s;
}

/* ==================== 错误枚举 ==================== */

#define foreach_sr_wg4_encap_error                                       \
  _ (NONE, "no error")                                                   \
  _ (NO_PATH, "bound path missing or inactive")                          \
  _ (HMAC_FAIL, "hmac sign failed")                                      \
  _ (NO_HEADROOM, "no headroom for sr header")                           \
  _ (BAD_PACKET, "malformed outer packet")

typedef enum {
#define _(sym, str) SR_WG4_ENCAP_ERROR_##sym,
  foreach_sr_wg4_encap_error
#undef _
    SR_WG4_ENCAP_N_ERROR,
} sr_wg4_encap_error_t;

static char *sr_wg4_encap_error_strings[] = {
#define _(sym, str) str,
  foreach_sr_wg4_encap_error
#undef _
};

/* ==================== Next nodes ==================== */

/*
 * 静态 next 只有 DROP / IP4_LOOKUP 两个；feature arc 的后续节点索引由
 * vnet_feature_enable_disable() 在运行时通过 vlib_node_add_next() 追加，
 * 由 vnet_feature_next_u16() 取回。
 */
typedef enum {
  SR_WG4_ENCAP_NEXT_DROP,
  SR_WG4_ENCAP_NEXT_IP4_LOOKUP,
  SR_WG4_ENCAP_N_NEXT,
} sr_wg4_encap_next_t;

/* ==================== Node 注册 ==================== */

VLIB_REGISTER_NODE (sr_wg4_encap_node) = {
  .name = "sr-wg4-encap",
  .vector_size = sizeof (u32),
  .format_trace = format_sr_wg4_encap_trace,
  .type = VLIB_NODE_TYPE_INTERNAL,
  .n_errors = SR_WG4_ENCAP_N_ERROR,
  .error_strings = sr_wg4_encap_error_strings,
  .n_next_nodes = SR_WG4_ENCAP_N_NEXT,
  .next_nodes = {
    [SR_WG4_ENCAP_NEXT_DROP] = "error-drop",
    [SR_WG4_ENCAP_NEXT_IP4_LOOKUP] = "ip4-lookup",
  },
};

/*
 * 注册为 "ip4-output" arc 上的 feature，排在 wg4-output-tun 之后。
 *
 * 不要在这里声明 VNET_FEATURE_ARC_INIT —— "ip4-output" arc 已由 libvnet
 * 注册，重复注册会在 vnet_feature_arc_init() 报 duplicate arc。
 *
 * 若 wireguard 插件未加载（"wg4-output-tun" 未注册为 feature），
 * runs_after 约束不会匹配到任何对象，排序时被忽略，不会报错。
 */
VNET_FEATURE_INIT (sr_wg4_encap_feat, static) = {
  .arc_name = SR_WG_ENCAP_ARC,
  .node_name = SR_WG_ENCAP_NODE,
  .runs_after = VNET_FEATURES ("wg4-output-tun"),
};

/* ==================== Node Function ==================== */

VLIB_NODE_FN (sr_wg4_encap_node) (vlib_main_t *vm, vlib_node_runtime_t *node,
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
  u16 vpn_port_net = clib_host_to_net_u16 (sm->vpn_port);
  u32 fib_index = sm->encap_fib_index;
  /* SR Header 时间戳（秒级）。transit/decap 不做时效校验，取 VPP 时钟即可 */
  u32 now = (u32) vlib_time_now (vm);

  vlib_get_buffers (vm, from, bufs, n_left);

  while (n_left > 0)
    {
      vlib_buffer_t *b = buf_ptr[0];
      ip4_header_t *ip4;
      udp_header_t *udp;
      u8 *payload;
      sr_wg_tunnel_bind_t *bind;
      sr_path_config_t *path = NULL;
      sr_segment_t *first_hop;
      /*
       * SR Header 在栈上构建：sr_path_fill_header() 内部 memset 整个
       * sr_header_t（固定 72B，含 segments[8] 数组），直接写进包里会
       * 覆盖 72 - sr_len 字节的 WG 载荷。构建完再按实际 sr_len 拷贝。
       */
      sr_header_t srh;
      ip4_address_t orig_dst;
      u8 *p;
      u32 tx_sw_if_index = vnet_buffer (b)->sw_if_index[VLIB_TX];
      u32 next_index = SR_WG4_ENCAP_NEXT_DROP;
      u32 ip4_len = 0, outer_len = 0, sr_len = 0;
      u16 path_id = 0;
      u8 seg_count = 0, seg_left = 0;
      u8 error = SR_WG4_ENCAP_ERROR_NONE;
      u8 bypass = 1;   /* 缺省直通：保证 WG 直连模式不受影响 */

      clib_memset (&orig_dst, 0, sizeof (orig_dst));
      clib_memset (&srh, 0, sizeof (srh));

      ip4 = (ip4_header_t *) vlib_buffer_get_current (b);

      /* Step 1: 长度下限——至少要能放下 outer IP4 + UDP + 1 字节载荷 */
      if (PREDICT_FALSE (b->current_length <
                         sizeof (ip4_header_t) + sizeof (udp_header_t) + 1))
        goto done;

      /* Step 2: 外层必须是 IPv4 + UDP（WG 隧道包） */
      if (PREDICT_FALSE (ip4->protocol != IP_PROTOCOL_UDP))
        goto done;

      ip4_len = ip4_header_bytes (ip4);
      /* 防御：header length 字段被破坏时不能算出小于 20 的 ip4_len，
       * 否则后面的 udp / payload 指针会指到 IP4 头内部。 */
      if (PREDICT_FALSE (ip4_len < sizeof (ip4_header_t)))
        goto done;
      outer_len = ip4_len + sizeof (udp_header_t);
      if (PREDICT_FALSE (b->current_length < outer_len + 1))
        goto done;

      udp = (udp_header_t *) ((u8 *) ip4 + ip4_len);
      payload = (u8 *) udp + sizeof (udp_header_t);
      orig_dst = ip4->dst_address;

      /*
       * Step 3: 环路守卫。载荷首字节已是 SR_MAGIC 说明本包已经封装过
       * SR Header（例如误把 feature 同时启用在 underlay 物理接口上），
       * 直接直通，避免二次封装。
       * 该判据安全：WG transport header 首字节是消息类型（1/2/3/4），
       * 永不等于 0x53。
       */
      if (PREDICT_FALSE (payload[0] == SR_MAGIC))
        goto done;

      /*
       * Step 4: 只处理 WG VPN 流量。SR-WG 约定 peer 端口固定为 51820
       * （SR_PORT_VPN），非该端口的包直通。
       */
      if (PREDICT_FALSE (udp->dst_port != vpn_port_net))
        goto done;

      /* Step 5: 查绑定（sw_if_index 优先，外层 dst IP 回退）。
       * 注意 ip4-output arc 上的 sw_if_index[VLIB_TX] 对隔道而言可能
       * 是父接口而不是 wg0，故绑定时建议同时下发 endpoint，让
       * endpoint_path 回退索引兼底。 */
      bind = sr_wg_bind_lookup (sm, tx_sw_if_index, orig_dst);
      if (PREDICT_FALSE (bind == NULL))
        goto done;   /* 未绑定 → 普通 WG 隧道，直通 */

      /* Step 6: 查路径。已显式绑定却查不到/未激活属于配置错误，丢包并计数，
       * 不做静默回退——否则流量会绕开 SR 路径直发 peer，破坏源路由语义。 */
      path = sr_path_lookup (&sm->path_table, bind->path_id);
      if (PREDICT_FALSE (path == NULL || !path->active))
        {
          error = SR_WG4_ENCAP_ERROR_NO_PATH;
          bypass = 0;
          sm->counters.encap_no_path++;
          sm->counters.drop_total++;
          goto done;
        }
      path_id = path->path_id;
      seg_count = path->seg_count;

      /* Step 7: 构造 SR Header（含 HMAC 签名） */
      if (PREDICT_FALSE (sr_path_fill_header (path, &srh, now) != 0))
        {
          error = SR_WG4_ENCAP_ERROR_HMAC_FAIL;
          bypass = 0;
          sm->counters.encap_hmac_fail++;
          sm->counters.drop_total++;
          goto done;
        }

      /* Step 8: 首跳免费——seg_left 覆盖为 seg_count-1（不在 HMAC 覆盖内） */
      seg_left = (u8) (seg_count - 1);
      srh.seg_left = seg_left;

      first_hop = &srh.segments[seg_count - 1];
      sr_len = sr_hdr_total_len (seg_count);

      /*
       * Step 9: headroom 守卫。
       * vlib_buffer_get_current() = b->data + b->current_data，
       * 可用 headroom = b->current_data + VLIB_BUFFER_PRE_DATA_SIZE。
       * wg4-output-tun 已消耗 44B（IP4+UDP+WG transport）rewrite 空间，
       * 26.06 的 VLIB_BUFFER_PRE_DATA_SIZE = 128，剩余约 84B，
       * 足以容纳最大 SR Header（72B），但仍需运行时校验。
       */
      if (PREDICT_FALSE ((i32) b->current_data +
                           (i32) VLIB_BUFFER_PRE_DATA_SIZE <
                         (i32) sr_len))
        {
          error = SR_WG4_ENCAP_ERROR_NO_HEADROOM;
          bypass = 0;
          sm->counters.encap_no_headroom++;
          sm->counters.drop_total++;
          goto done;
        }

      /* Step 10: 腾出 SR Header 空间，并把外层 IP4/UDP 整体前移 */
      vlib_buffer_advance (b, -(int) sr_len);
      p = (u8 *) vlib_buffer_get_current (b);
      clib_memmove (p, p + sr_len, outer_len);

      ip4 = (ip4_header_t *) p;
      udp = (udp_header_t *) (p + ip4_len);

      /* Step 11: 写入 SR Header（仅 sr_len 字节，不越界覆盖 WG 载荷） */
      clib_memcpy_fast (p + outer_len, &srh, sr_len);

      /* Step 12: 改写外层 IP4 dst / UDP dst_port 为第一跳 */
      clib_memcpy_fast (&ip4->dst_address, first_hop->ip, 4);
      clib_memcpy_fast (&udp->dst_port, first_hop->port, 2);

      /* Step 13: 长度增加 sr_len */
      ip4->length =
        clib_host_to_net_u16 (clib_net_to_host_u16 (ip4->length) + sr_len);
      udp->length =
        clib_host_to_net_u16 (clib_net_to_host_u16 (udp->length) + sr_len);

      /* Step 14: 校验和。UDP checksum 置 0（与 sr-wg4-transit 保持一致，
       * 也是 WireGuard 的惯例）；同时清掉所有 offload 标志，避免下游按
       * 过期的 hdr offset 重算。 */
      vnet_buffer_offload_flags_clear (b, vnet_buffer (b)->oflags);
      b->flags &= ~(VNET_BUFFER_F_L2_HDR_OFFSET_VALID |
                    VNET_BUFFER_F_L3_HDR_OFFSET_VALID |
                    VNET_BUFFER_F_L4_HDR_OFFSET_VALID);
      udp->checksum = 0;
      ip4->checksum = ip4_header_checksum (ip4);

      /* Step 15: 外层 dst 已变，必须重新查 FIB 才能拿到第一跳对应的
       * 父邻接 L2 rewrite（arc 默认的 adj-midchain-tx 用的是原 peer
       * endpoint 的父邻接，其 dst MAC 与第一跳不匹配）。 */
      vnet_buffer (b)->ip.fib_index = fib_index;
      next_index = SR_WG4_ENCAP_NEXT_IP4_LOOKUP;
      bypass = 0;
      sm->counters.encap_ok++;

    done:
      if (bypass)
        {
          /* 继续沿 ip4-output arc → adj-midchain-tx → ip4-rewrite */
          vnet_feature_next (&next_index, b);
          sm->counters.encap_bypass++;
        }

      b->error = error ? node->errors[error] : 0;

      if (PREDICT_FALSE (b->flags & VLIB_BUFFER_IS_TRACED))
        {
          sr_wg4_encap_trace_t *tr =
            vlib_add_trace (vm, node, b, sizeof (*tr));
          clib_memset (tr, 0, sizeof (*tr));
          tr->next_index = next_index;
          tr->tx_sw_if_index = tx_sw_if_index;
          tr->path_id = path_id;
          tr->seg_count = seg_count;
          tr->seg_left = seg_left;
          tr->sr_len = (u16) sr_len;
          tr->orig_dst = orig_dst;
          tr->current_data = b->current_data;
          tr->error = error;
          tr->bypass = bypass;
          if (!bypass && seg_count)
            {
              clib_memcpy_fast (&tr->first_hop_ip, srh.segments[seg_count - 1].ip,
                                4);
              u16 port_net;
              clib_memcpy_fast (&port_net, srh.segments[seg_count - 1].port, 2);
              tr->first_hop_port = clib_net_to_host_u16 (port_net);
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

/* ====================================================================
 * 控制面：隧道-路径绑定表
 * ==================================================================== */

void
sr_wg_encap_init (sr_wg_main_t *sm)
{
  if (!sm->tunnel_path)
    sm->tunnel_path = hash_create (0, sizeof (uword));
  if (!sm->endpoint_path)
    sm->endpoint_path = hash_create (0, sizeof (uword));
  if (!sm->binds)
    pool_alloc (sm->binds, 64);
}

sr_wg_tunnel_bind_t *
sr_wg_bind_lookup (sr_wg_main_t *sm, u32 sw_if_index, ip4_address_t dst)
{
  uword *p;

  p = hash_get (sm->tunnel_path, (uword) sw_if_index);
  if (p && p[0] != 0)
    return pool_elt_at_index (sm->binds, p[0] - 1);

  p = hash_get (sm->endpoint_path,
                (uword) clib_net_to_host_u32 (dst.as_u32));
  if (p && p[0] != 0)
    return pool_elt_at_index (sm->binds, p[0] - 1);

  return NULL;
}

/* 内部：把 bind 池索引 bi+1 登记到 endpoint hash（endpoint 非 0 时） */
static void
sr_wg_bind_set_endpoint (sr_wg_main_t *sm, sr_wg_tunnel_bind_t *bind, u32 bi)
{
  if (bind->endpoint.as_u32 != 0)
    hash_set (sm->endpoint_path,
              (uword) clib_net_to_host_u32 (bind->endpoint.as_u32), bi + 1);
}

/* 内部：从 endpoint hash 摘除 */
static void
sr_wg_bind_unset_endpoint (sr_wg_main_t *sm, ip4_address_t endpoint)
{
  if (endpoint.as_u32 != 0)
    hash_unset (sm->endpoint_path,
                (uword) clib_net_to_host_u32 (endpoint.as_u32));
}

int
sr_wg_tunnel_set_path (sr_wg_main_t *sm, u32 sw_if_index, u16 path_id,
                       const ip4_address_t *endpoint, int is_add)
{
  vnet_main_t *vnm = vnet_get_main ();
  sr_wg_tunnel_bind_t *bind;
  uword *p;
  u32 bi;
  int rv;

  sr_wg_encap_init (sm);

  if (!vnet_get_sw_interface_or_null (vnm, sw_if_index))
    return VNET_API_ERROR_INVALID_SW_IF_INDEX;

  /* ---------- 解绑 ---------- */
  if (!is_add)
    {
      p = hash_get (sm->tunnel_path, (uword) sw_if_index);
      if (!p || p[0] == 0)
        return VNET_API_ERROR_NO_SUCH_ENTRY;

      bi = p[0] - 1;
      bind = pool_elt_at_index (sm->binds, bi);

      /* 先关掉 feature，再摘表项，避免数据面看到悬空绑定 */
      sr_wg_encap_enable_disable (sm, sw_if_index, 0);

      sr_wg_bind_unset_endpoint (sm, bind->endpoint);
      hash_unset (sm->tunnel_path, (uword) sw_if_index);
      pool_put_index (sm->binds, bi);
      return 0;
    }

  /* ---------- 绑定 ---------- */
  if (!sr_path_lookup (&sm->path_table, path_id))
    return VNET_API_ERROR_NO_SUCH_ENTRY;

  p = hash_get (sm->tunnel_path, (uword) sw_if_index);
  if (p && p[0] != 0)
    {
      /* 已存在 → 更新 */
      bi = p[0] - 1;
      bind = pool_elt_at_index (sm->binds, bi);
      if (endpoint && endpoint->as_u32 != bind->endpoint.as_u32)
        sr_wg_bind_unset_endpoint (sm, bind->endpoint);
    }
  else
    {
      pool_get_zero (sm->binds, bind);
      bi = bind - sm->binds;
      hash_set (sm->tunnel_path, (uword) sw_if_index, bi + 1);
    }

  bind->sw_if_index = sw_if_index;
  bind->path_id = path_id;
  if (endpoint)
    bind->endpoint = *endpoint;
  sr_wg_bind_set_endpoint (sm, bind, bi);

  /* 绑定时自动在 ip4-output arc 上启用 sr-wg4-encap（与 wg4-output-tun
   * 同接口同 arc）。也可用 `set sr-wg encap <sw_if_index> disable` 单独关闭。 */
  rv = sr_wg_encap_enable_disable (sm, sw_if_index, 1);
  if (rv != 0)
    return rv;

  return 0;
}

int
sr_wg_encap_enable_disable (sr_wg_main_t *sm, u32 sw_if_index, int enable)
{
  vnet_main_t *vnm = vnet_get_main ();
  uword *p;
  int rv;

  sr_wg_encap_init (sm);

  if (!vnet_get_sw_interface_or_null (vnm, sw_if_index))
    return VNET_API_ERROR_INVALID_SW_IF_INDEX;

  rv = vnet_feature_enable_disable (SR_WG_ENCAP_ARC, SR_WG_ENCAP_NODE,
                                    sw_if_index, enable ? 1 : 0, 0, 0);
  if (rv != 0)
    return rv;

  p = hash_get (sm->tunnel_path, (uword) sw_if_index);
  if (p && p[0] != 0)
    pool_elt_at_index (sm->binds, p[0] - 1)->feature_on = enable ? 1 : 0;

  return 0;
}
