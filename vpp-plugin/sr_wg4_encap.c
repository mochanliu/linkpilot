/*
 * sr_wg4_encap.c - SR-WG Encap Graph Node + 隧道-路径绑定管理
 *
 * ==================== 集成方式 ====================
 *
 * WireGuard 插件导出的符号全部是 hidden（无法 dlsym 直接调用），因此
 * SR-WG 与 WG 的集成只能通过 VPP 的 feature arc / graph node 串联完成。
 *
 * 实测（VPP 26.06-release）结论：**wg4-output-tun 不参与 feature 链**。
 *   - `show interface features wg0` 显示 ip4-output: wg4-output-tun,
 *     sr-wg4-encap —— arc 顺序是对的，feature 也确实注册成了 wg4-output-tun
 *     的 next-index 5；
 *   - 但 `show node wg4-output-tun` 显示 adj-midchain-tx=54 vectors、
 *     sr-wg4-encap=0 vectors。原因在 wireguard_output_tun.c 里
 *     `next_out0 = WG_OUTPUT_NEXT_INTERFACE_OUTPUT` 被硬编码为
 *     adj-midchain-tx，且整个文件 0 次调用 vnet_feature_next()。
 *     所以挂在 wg0 的 ip4-output arc 上是结构性死挂点，加密后的包永远
 *     不会经过我们（这不是配置问题，改配置无解）。
 *
 * 可用的挂点是 **underlay 出口接口**（本例 host-eth0）的同名 arc：
 * libvnet 在 ip4_forward.c 把 "ip4-output" arc 声明为
 *     .start_nodes = VNET_FEATURES ("ip4-rewrite", "ip4-midchain", "ip4-dvr-dpo")
 *     .last_in_arc = "interface-output"
 * 即 ip4-rewrite 也是该 arc 的起点。WG 加密后的外层包必然走：
 *
 *   内网包 → ip4-lookup(vpn route) → ip4-midchain(tx=wg0)
 *          → wg4-output-tun（WG 加密 + 外层 IP4/UDP/WG-transport）
 *          → adj-midchain-tx → ip4-load-balance → ip4-rewrite（外层 L2）
 *          → sr-wg4-encap（插 SR Header + 改写外层 dst 为第一跳）   ← 挂点
 *          → ip4-lookup（外层重新查表，得到第一跳对应的父邻接）
 *          → ip4-rewrite → interface-output → 物理网卡
 *
 * 已由 `set sr-wg encap enable <underlay>` 实测验证：4 个 overlay ping
 * 精确产生 4 vectors 进入 sr-wg4-encap，arc 直通路径（bypass）不破坏原包。
 *
 * 未绑定 SR 路径时 sr-wg4-encap 用 vnet_feature_next() 直通，包继续沿 arc
 * 走 interface-output → 网卡，WG 直连模式完全不受影响。
 *
 * ==================== 节点处的缓冲区布局 ====================
 *
 * 挂点在 ip4-rewrite **之后**，L2 头已经写好，所以 vlib_buffer_get_current(b)
 * 指向以太网头：
 *
 *   current → [ ETH | outer IP4 | outer UDP | WG transport hdr | 加密载荷 ]
 *
 * 目标线格式（SR Header 插在 UDP 头与 WG 载荷之间），且交给 ip4-lookup 的
 * 必须是纯 L3 包：
 *
 *   current → [ outer IP4 | outer UDP | SR Header | WG transport hdr | 载荷 ]
 *
 * 因此三步走，且**全部放在"决定封装"之后**（bypass 分支绝不改包）：
 *   1) advance(+l2_len)  —— 剥掉 L2，current 落到外层 IP4 头
 *   2) advance(-sr_len)  —— 抢 headroom，[IP][UDP] 逻辑位置后移 sr_len
 *   3) memmove(p, p+sr_len, ip4_len+8) —— 把 [IP][UDP] 拉回包首，
 *      腾出的 [ip4_len+8, ip4_len+8+sr_len) 正好落在 WG transport 头之前，
 *      WG 载荷一个字节都不用动。
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
#include <vnet/ethernet/packet.h>

/* ==================== Trace 定义 ==================== */

typedef struct {
  u32 next_index;
  u32 tx_sw_if_index;
  u16 path_id;
  u8  seg_count;
  u8  seg_left;
  u16 sr_len;
  u8  l2_len;              /* 入口时嗅到的 L2 头长度（0=无 L2） */
  ip4_address_t orig_dst;      /* 原外层 dst（WG peer endpoint） */
  u16 in_port;                 /* 封装前的外层 UDP 目的端口 = 隧道身份的另一半 */
  ip4_address_t first_hop_ip;  /* 改写后的外层 dst */
  u16 first_hop_port;
  i16 current_data;            /* 封装后 current_data，用于观测 headroom */
  u8  error;
  u8  bypass;
} sr_wg4_encap_trace_t;

static u8 *
format_sr_wg4_encap_trace (u8 *s, va_list *args)
{
  CLIB_UNUSED (vlib_main_t * vm) = va_arg (*args, vlib_main_t *);
  CLIB_UNUSED (vlib_node_t * node) = va_arg (*args, vlib_node_t *);
  sr_wg4_encap_trace_t *t = va_arg (*args, sr_wg4_encap_trace_t *);

  /*
   * 两条分支都带 in_port：同一对 CPE 开多条隧道时 (orig_dst, in_port) 才是
   * "这条流属于哪条隧道"的完整身份，只看 dst IP 会把它错认成一条。
   */
  if (t->bypass)
    s = format (s, "SR-WG-ENCAP: bypass sw_if_index=%u orig_dst=%U:%u "
                   "err=%u next=%u",
                t->tx_sw_if_index, format_ip4_address, &t->orig_dst,
                t->in_port, t->error, t->next_index);
  else
    s = format (s, "SR-WG-ENCAP: path_id=%u seg_count=%u seg_left=%u "
                   "sr_len=%u l2_len=%u orig_dst=%U:%u -> first_hop=%U:%u "
                   "cd=%d err=%u next=%u",
                t->path_id, t->seg_count, t->seg_left, t->sr_len, t->l2_len,
                format_ip4_address, &t->orig_dst, t->in_port,
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
  /* 打开 per-node trace，`trace add sr-wg4-encap 10` 才可用（排障全靠它） */
  .flags = VLIB_NODE_FLAG_TRACE_SUPPORTED,
  .n_errors = SR_WG4_ENCAP_N_ERROR,
  .error_strings = sr_wg4_encap_error_strings,
  .n_next_nodes = SR_WG4_ENCAP_N_NEXT,
  .next_nodes = {
    [SR_WG4_ENCAP_NEXT_DROP] = "error-drop",
    [SR_WG4_ENCAP_NEXT_IP4_LOOKUP] = "ip4-lookup",
  },
};

/*
 * 注册为 "ip4-output" arc 上的 feature，显式排在 arc 终点 interface-output
 * 之前（与 ipsec4-output-feature 的写法一致）。挂点在 underlay 接口上，
 * 所以这里排在 wg4-output-tun 之后毫无意义（WG 根本不参与 arc 链），
 * 用 runs_before interface-output 才表达真实意图：包在离开本机前最后
 * 一次可改写外层的机会。
 *
 * 不要在这里声明 VNET_FEATURE_ARC_INIT —— "ip4-output" arc 已由 libvnet
 * 注册，重复注册会在 vnet_feature_arc_init() 报 duplicate arc。
 */
VNET_FEATURE_INIT (sr_wg4_encap_feat, static) = {
  .arc_name = SR_WG_ENCAP_ARC,
  .node_name = SR_WG_ENCAP_NODE,
  .runs_before = VNET_FEATURES ("interface-output"),
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
      u8 *cur;
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
      u32 ip4_len = 0, outer_len = 0, sr_len = 0, l2_len = 0;
      u16 in_port = 0;   /* 封装前的外层 UDP 目的端口（隧道身份的一半） */
      u16 path_id = 0;
      u8 seg_count = 0, seg_left = 0;
      u8 error = SR_WG4_ENCAP_ERROR_NONE;
      u8 bypass = 1;   /* 缺省直通：保证 WG 直连模式不受影响 */

      clib_memset (&orig_dst, 0, sizeof (orig_dst));
      clib_memset (&srh, 0, sizeof (srh));

      cur = vlib_buffer_get_current (b);

      /* Step 1: 长度下限——至少要能放下外层 IP4 + UDP + 1 字节载荷，
       * 也保证下面嗅探 ethertype / 版本 nibble 不会读越界 */
      if (PREDICT_FALSE (b->current_length <
                         sizeof (ip4_header_t) + sizeof (udp_header_t) + 1))
        goto done;

      /*
       * Step 2: 挂点在 ip4-rewrite 之后，L2 头已经写好，所以 current 指向
       * 以太网头。判据用「ethertype==0x0800 且紧随其后的版本 nibble==4」，
       * 单嗅一个字节会被 MAC 首字节撞出假阳性。判据不成立时退回按
       * 「无 L2」解析（万一 arc 挂点或 underlay 形态变了仍可用）。
       * 带 VLAN tag(0x8100) 的 underlay 不在 v1 范围内：会走"无 L2"分支、
       * 版本 nibble 检查失败、安全 bypass。
       */
      if (clib_net_to_host_u16 (((ethernet_header_t *) cur)->type) ==
          ETHERNET_TYPE_IP4 &&
          (((ip4_header_t *) (cur +
                sizeof (ethernet_header_t)))->ip_version_and_header_length >>
           4) == 4)
        l2_len = sizeof (ethernet_header_t);

      ip4 = (ip4_header_t *) (cur + l2_len);

      /* 外层必须是 IPv4 */
      if (PREDICT_FALSE ((ip4->ip_version_and_header_length >> 4) != 4))
        goto done;

      /* 外层必须是 UDP（WG 隧道包） */
      if (PREDICT_FALSE (ip4->protocol != IP_PROTOCOL_UDP))
        goto done;

      ip4_len = ip4_header_bytes (ip4);
      /* 防御：header length 字段被破坏时不能算出小于 20 的 ip4_len，
       * 否则后面的 udp / payload 指针会指到 IP4 头内部。 */
      if (PREDICT_FALSE (ip4_len < sizeof (ip4_header_t)))
        goto done;
      outer_len = ip4_len + sizeof (udp_header_t);
      if (PREDICT_FALSE (b->current_length < l2_len + outer_len + 1))
        goto done;

      udp = (udp_header_t *) ((u8 *) ip4 + ip4_len);
      payload = (u8 *) udp + sizeof (udp_header_t);
      orig_dst = ip4->dst_address;

      /*
       * Step 3: 环路守卫。载荷首字节已是 SR_MAGIC 说明本包已经封装过
       * SR Header。本节点改写外层 dst 后会交回 ip4-lookup，重查表的包
       * 会**第二次**经过本节点（同一张 underlay 网卡），这条判据 + Step 4
       * 的端口判据一起保证第二次只走直通，不会二次封装。
       * 该判据安全：WG transport header 首字节是消息类型（1/2/3/4），
       * 永不等于 0x53。
       */
      if (PREDICT_FALSE (payload[0] == SR_MAGIC))
        goto done;

      /*
       * Step 4: 只处理打在本机**某条** WG 隧道监听端口上的包（vpn_ports 集合，
       * 定案 2「每隧道一个端口」）。集合外的端口一律直通。
       * 封装后外层端口被改成 SR 段里的第一跳端口，正常不在集合内，所以这一条
       * 同时仍是"第二次经过本节点时直通"的第二重保险（第一重是 Step 3 的 magic）。
       */
      in_port = clib_net_to_host_u16 (udp->dst_port);
      if (PREDICT_FALSE (!clib_bitmap_get (sm->vpn_ports, in_port)))
        goto done;

      /* Step 5: 查绑定（外层 (dst IP, 封装前 dst 端口) 优先，sw_if_index 兜底）。
       * 挂点在 underlay 接口，tx_sw_if_index 是物理/宿主接口而不是 wg0，
       * 所以实际生效的是 endpoint_path（外层 dst = WG peer endpoint）。
       * 端口是这个键的另一半：同一对 CPE 开多条隧道时，只有端口能把
       * "这条流属于哪条隧道"分清楚。绑定时必须下发 endpoint，否则本步查不到
       * → 静默直通 → 流量绕过 SR 路径直发 peer。`show sr-wg tunnels` 会把
       * 这种配错显式标出来。 */
      bind = sr_wg_bind_lookup (sm, tx_sw_if_index, orig_dst, in_port);
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
       * 此处 current 仍指向 L2 头，Step 10 会先剥掉 l2_len 再抢 sr_len，
       * 所以 l2_len 要计入可用余量。26.06 的 VLIB_BUFFER_PRE_DATA_SIZE=128，
       * ip4-rewrite 掉 14B 后仍余 >100B，容得下最大 SR Header（72B），
       * 但外层带 IP options 时余量会变，故保留运行时校验。
       */
      if (PREDICT_FALSE ((i32) b->current_data + (i32) l2_len +
                           (i32) VLIB_BUFFER_PRE_DATA_SIZE <
                         (i32) sr_len))
        {
          error = SR_WG4_ENCAP_ERROR_NO_HEADROOM;
          bypass = 0;
          sm->counters.encap_no_headroom++;
          sm->counters.drop_total++;
          goto done;
        }

      /*
       * Step 10: 剥 L2 → 抢 headroom → 把外层 IP4/UDP 拉回包首。
       * 顺序不能颠倒：先剥 L2 才有 l2_len 字节的余量给 sr_len，
       * 且交给 ip4-lookup 的包必须是纯 L3。
       */
      if (l2_len)
        vlib_buffer_advance (b, (int) l2_len);
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

      /*
       * Step 15: 交回 ip4-lookup 重新查表。
       *
       * ip4-lookup 的入参约定（vnet/ip/ip4_forward.c 的 @node ip4-lookup 文档）
       * 是 **复用 sw_if_index[VLIB_TX] 当 FIB index**：
       *   - 该字段 == ~0    → 在"收包接口所挂的 FIB"里做 LPM；
       *   - 该字段 != ~0    → 直接把它当作 FIB index（0/1/2…），不是接口索引。
       * 进到本节点时，VLIB_TX 已被 ip4-rewrite/adj-midchain-tx 写成 underlay
       * 出口**接口索引**（本实验里是 1），若不覆盖就会被当成"FIB 1"去 LPM，
       * 查不到 10.10.1.3 → Drop 邻接 → error-drop。这正是之前黑洞的根因，
       * 也是"改了 fib_index 却仍然丢包"的原因：本版本 ip4-lookup 读的是
       * VLIB_TX，不是 ip.fib_index。
       *
       * 因此这里显式把 VLIB_TX 写成 encap_fib_index，让 `set sr-wg encap
       * fib-index <n>` 真正生效；adj_index[VLIB_TX] 是本节点进入前留下的
       * 旧父邻接，一并清掉，避免下游（trace/错误路径）读到过期邻接。
       * ip4-lookup 会用查表结果重写这两个字段，ip4-rewrite 随后把 VLIB_TX
       * 覆盖成真正的出口接口索引，interface-output 拿到的是正确值。
       */
      vnet_buffer (b)->sw_if_index[VLIB_TX] = fib_index;
      vnet_buffer (b)->ip.adj_index[VLIB_TX] = ~0;
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
          tr->l2_len = (u8) l2_len;
          tr->orig_dst = orig_dst;
          tr->in_port = in_port;
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
  if (!sm->encap_enabled_ifs)
    sm->encap_enabled_ifs = hash_create (0, sizeof (uword));
  if (!sm->binds)
    pool_alloc (sm->binds, 64);
}

/*
 * endpoint_path 的键：外层 dst IP + **封装前**的外层 UDP 目的端口。
 * 端口是键的另一半，因为同一对 CPE 可以开多条 WG 隧道（每隧道一个监听端口，
 * 定案 2），只按 IP 会把第二条隧道也拽进第一条的路径。
 * dst_port = 0 是通配键：命中"这个 IP 上不分行数全走本路径"的旧配法。
 */
static uword
sr_wg_bind_key (ip4_address_t endpoint, u16 dst_port)
{
  return ((uword) clib_net_to_host_u32 (endpoint.as_u32) << 16) |
         (uword) dst_port;
}

sr_wg_tunnel_bind_t *
sr_wg_bind_lookup (sr_wg_main_t *sm, u32 sw_if_index, ip4_address_t dst,
                   u16 dst_port)
{
  uword *p;

  /*
   * 精确键 (dst, port) 最优先，其次 (dst, 0) 通配，最后 sw_if_index。
   * 挂点在 underlay 接口，sw_if_index 命中的只能是"整张 underlay 网卡
   * 一条默认路径"这种粗粒度配法，所以更具体的 endpoint 必须优先，
   * 否则一张 underlay 网卡上的所有 WG 隧道会被默认路径全部接管。
   */
  p = hash_get (sm->endpoint_path, sr_wg_bind_key (dst, dst_port));
  if (p && p[0] != 0)
    return pool_elt_at_index (sm->binds, p[0] - 1);

  if (dst_port != 0)
    {
      p = hash_get (sm->endpoint_path, sr_wg_bind_key (dst, 0));
      if (p && p[0] != 0)
        return pool_elt_at_index (sm->binds, p[0] - 1);
    }

  p = hash_get (sm->tunnel_path, (uword) sw_if_index);
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
              sr_wg_bind_key (bind->endpoint, bind->dst_port), bi + 1);
}

/* 内部：按 bind 自己的 (endpoint, dst_port) 从 endpoint hash 摘除 */
static void
sr_wg_bind_unset_endpoint (sr_wg_main_t *sm, const sr_wg_tunnel_bind_t *bind)
{
  if (bind->endpoint.as_u32 != 0)
    hash_unset (sm->endpoint_path,
                sr_wg_bind_key (bind->endpoint, bind->dst_port));
}

int
sr_wg_tunnel_set_path (sr_wg_main_t *sm, u32 sw_if_index, u16 path_id,
                       const ip4_address_t *endpoint, u16 dst_port,
                       int is_add)
{
  vnet_main_t *vnm = vnet_get_main ();
  sr_wg_tunnel_bind_t *bind;
  ip4_address_t new_endpoint;
  uword old_key = 0, new_key;
  u8 have_old = 0;
  uword *p;
  u32 bi;

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

      sr_wg_bind_unset_endpoint (sm, bind);
      hash_unset (sm->tunnel_path, (uword) sw_if_index);
      pool_put_index (sm->binds, bi);
      /*
       * 不动 feature：underlay 接口上的 sr-wg4-encap 是全局开关，可能还有
       * 别的绑定依赖它；没绑定时节点本身走 bypass，不改变任何包。
       */
      return 0;
    }

  /* ---------- 绑定 ---------- */
  if (!sr_path_lookup (&sm->path_table, path_id))
    return VNET_API_ERROR_NO_SUCH_ENTRY;

  clib_memset (&new_endpoint, 0, sizeof (new_endpoint));
  if (endpoint)
    new_endpoint = *endpoint;

  p = hash_get (sm->tunnel_path, (uword) sw_if_index);
  if (p && p[0] != 0)
    bi = p[0] - 1;    /* 已存在 → 原地更新 */
  else
    bi = (u32) ~0;    /* 新建 */

  /*
   * (endpoint, dst_port) 就是"哪条隧道"的身份：同一对 CPE 开多条 WG 隧道时
   * 只有端口能把它们分开。两张接口抢同一个身份一定是配错了，而后果是前一条
   * 隧道静默旁路（encap_bypass 上涨、隧道仍是通的），所以这里显式拒绝，
   * 让控制面看见 —— 具体是谁占的，`show sr-wg tunnels` 全表可查。
   */
  new_key = sr_wg_bind_key (new_endpoint, dst_port);
  if (new_endpoint.as_u32 != 0)
    {
      uword *owner = hash_get (sm->endpoint_path, new_key);
      if (owner && owner[0] != 0 && (bi == (u32) ~0 || owner[0] != bi + 1))
        return VNET_API_ERROR_ENTRY_ALREADY_EXISTS;
    }

  if (bi == (u32) ~0)
    {
      pool_get_zero (sm->binds, bind);
      bi = bind - sm->binds;
      hash_set (sm->tunnel_path, (uword) sw_if_index, bi + 1);
    }
  else
    bind = pool_elt_at_index (sm->binds, bi);

  old_key = sr_wg_bind_key (bind->endpoint, bind->dst_port);
  have_old = bind->endpoint.as_u32 != 0;

  bind->sw_if_index = sw_if_index;
  bind->path_id = path_id;
  bind->endpoint = new_endpoint;
  bind->dst_port = dst_port;

  /* endpoint 或端口变了要摘掉旧键，否则留下一条指向本 bind 的僵尸键，
   * 会把别的隧道的流量拽进这条路径。 */
  if (have_old && old_key != new_key)
    hash_unset (sm->endpoint_path, old_key);

  sr_wg_bind_set_endpoint (sm, bind, bi);

  return 0;
}

u32
sr_wg_binds_drop_for_path (sr_wg_main_t *sm, u16 path_id)
{
  u32 n = 0, bi;

  sr_wg_encap_init (sm);

  /*
   * pool_put_index 只置自由位、不缩 vec_len，所以边遍历边摘是安全的。
   * 悬空绑定不会立刻报错，只会在收包时按 encap_no_path 静默丢包 ——
   * 删路径必须把指向它的绑定一起摘掉，控制面才不会看到"路径没了但流量
   * 还在悄悄旁路"这种状态。
   */
  for (bi = 0; bi < vec_len (sm->binds); bi++)
    {
      sr_wg_tunnel_bind_t *bind;
      uword *p;

      if (pool_is_free_index (sm->binds, bi))
        continue;

      bind = pool_elt_at_index (sm->binds, bi);
      if (bind->path_id != path_id)
        continue;

      /* 新签名的 unset：摘的是 (endpoint, dst_port) 组合键，传整个 bind */
      sr_wg_bind_unset_endpoint (sm, bind);
      p = hash_get (sm->tunnel_path, (uword) bind->sw_if_index);
      if (p && p[0] == bi + 1)
        hash_unset (sm->tunnel_path, (uword) bind->sw_if_index);
      pool_put_index (sm->binds, bi);
      n++;
    }

  return n;
}

int
sr_wg_encap_enable_disable (sr_wg_main_t *sm, u32 sw_if_index, int enable)
{
  vnet_main_t *vnm = vnet_get_main ();
  int rv;

  sr_wg_encap_init (sm);

  if (!vnet_get_sw_interface_or_null (vnm, sw_if_index))
    return VNET_API_ERROR_INVALID_SW_IF_INDEX;

  rv = vnet_feature_enable_disable (SR_WG_ENCAP_ARC, SR_WG_ENCAP_NODE,
                                    sw_if_index, enable ? 1 : 0, 0, 0);
  if (rv != 0)
    return rv;

  if (enable)
    {
      if (!hash_get (sm->encap_enabled_ifs, (uword) sw_if_index))
        sm->encap_enabled_count++;
      hash_set (sm->encap_enabled_ifs, (uword) sw_if_index, 1);
    }
  else
    {
      if (hash_get (sm->encap_enabled_ifs, (uword) sw_if_index))
        sm->encap_enabled_count--;
      hash_unset (sm->encap_enabled_ifs, (uword) sw_if_index);
    }

  return 0;
}

int
sr_wg_encap_is_enabled (sr_wg_main_t *sm, u32 sw_if_index)
{
  return hash_get (sm->encap_enabled_ifs, (uword) sw_if_index) != 0;
}
