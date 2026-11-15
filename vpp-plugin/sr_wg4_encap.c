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
 * 与 sr_header.h、docs/sr-header-format.md 一致的位序约定：
 *   segments[0]            = 最终目的节点
 *   segments[seg_count-1]  = 第一跳中转（"首跳免费"，由外层 IP/UDP 承载）
 *   sr_hdr_get_next_hop()  = &segments[seg_left - 1]
 *   sr_hdr_pop_segment()   = 取走 segments[seg_left-1] 后 seg_left--
 *
 * 但**终点那一段不来自路径表，来自这个包本身**：路径表里只存中间跳
 * （relay_count 个），终点段在封装前从报文的外层 dst / UDP 目的端口现取。
 * 所以路径表字段叫 relays[]/relay_count，线格式仍然是
 *   seg_count            = relay_count + 1
 *   segments[0]          = 本包封装前的 (外层 dst IP, 外层 UDP 目的端口)
 *   segments[1 + i]      = path->relays[i]      （i = 0..relay_count-1）
 *   segments[seg_count-1]= path->relays[relay_count-1] = 第一跳
 *
 * 故 encap 必须：
 *   外层 IP4 dst      = segments[seg_count-1].ip
 *   外层 UDP dst_port = segments[seg_count-1].port
 *   seg_left          = seg_count - 1 = relay_count（首跳已在外层）
 *
 * 注意 sr_hdr_init()/sr_path_fill_header() 把 seg_left 初始化为 seg_count，
 * 必须在其之后覆盖为 relay_count；seg_left 不在 HMAC 覆盖范围内，
 * 覆盖后无需重新签名。
 *
 * relay_count = 0（隧道没有中转跳）时 seg_count=1、seg_left=0，"第一跳"就是
 * 终点自己：Step 11 把外层 dst 改写成它当前的值，是无害的自我改写。这一条
 * 必须留着 —— 那个 SR 头证明"发端持有这把 P-Key"，是收端把外层源记成
 * verified 观测的唯一凭证（endpoint 未配的对端地址全靠它上报）。
 *
 * 为什么终点不写进路径表：路径表存的是"中间怎么走"，终点地址是 WG 的事，
 * 可能学自 NAT、会变、也可能压根没配。把会漂移的值写进表里，地址一变表里
 * 就留着一个没人再听的老地址，而 SR 头的 HMAC 覆盖终点段，改它就得重签。
 *
 * 验证（relay_count=1: segments[0]=node2:51820, segments[1]=transit:51821）：
 *   encap   → seg_left=1，外层 dst=transit:51821
 *   transit → next_hop=segments[0]=node2:51820，seg_left 1→0，改写外层
 *   node2   → sr-wg4-input 见 seg_left==0 → 剥 SR 头 → wg4-input 解密
 */
#include "sr_wg.h"
#include "sr_qos.h"
#include "sr_fec.h"

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
  u16 src_port;            /* 外层 UDP 源端口 = 本端隧道监听端口 = 绑定表的键 */
  ip4_address_t orig_dst;      /* 原外层 dst：终点段就是从它取的 */
  u16 orig_dport;          /* 封装前的外层 UDP 目的端口（终点段的端口） */
  ip4_address_t first_hop_ip;  /* 改写后的外层 dst */
  u16 first_hop_port;
  i16 current_data;            /* 封装后 current_data，用于观测 headroom */
  u8  error;
  u8  bypass;
  u8  qos_cls;                 /* 写进 action/外层 ToS 的 QoS 类别 */
} sr_wg4_encap_trace_t;

static u8 *
format_sr_wg4_encap_trace (u8 *s, va_list *args)
{
  CLIB_UNUSED (vlib_main_t * vm) = va_arg (*args, vlib_main_t *);
  CLIB_UNUSED (vlib_node_t * node) = va_arg (*args, vlib_node_t *);
  sr_wg4_encap_trace_t *t = va_arg (*args, sr_wg4_encap_trace_t *);

  /*
   * 两条分支都带 src_port：它是"这一包属于哪条隧道"的唯一判据，未命中的包
   * 最需要看的恰好就是它的源端口是多少（对不上任何绑定 = 旁路的根因）。
   */
  if (t->bypass)
    s = format (s, "SR-WG-ENCAP: bypass sw_if_index=%u src_port=%u "
                   "endpoint=%U:%u err=%u next=%u",
                t->tx_sw_if_index, t->src_port, format_ip4_address,
                &t->orig_dst, t->orig_dport, t->error, t->next_index);
  else
    s = format (s, "SR-WG-ENCAP: path_id=%u seg_count=%u seg_left=%u "
                   "sr_len=%u l2_len=%u src_port=%u qos_cls=%u "
                   "endpoint=%U:%u "
                   "-> first_hop=%U:%u cd=%d err=%u next=%u",
                t->path_id, t->seg_count, t->seg_left, t->sr_len, t->l2_len,
                t->src_port, t->qos_cls, format_ip4_address,
                &t->orig_dst, t->orig_dport,
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

/*
 * 输出序列的槽位数：一个数据包最多带出一个冗余包（ratio >= 1，见
 * sr_fec.h 设计 1），所以 2 × 帧长是上界；再按 vlib_buffer_enqueue_to_next
 * 的约定补 64 —— 该函数自己会读过 count 之后的少量元素
 * （vlib/buffer_node.h 的 VLIB_BUFFER_ENQUEUE_MIN_SIZE 注释），数组必须容得下，
 * 否则读到未映射页会段错误。
 */
#define SR_WG_ENCAP_OUT_SLOTS  (2 * VLIB_FRAME_SIZE + 64)

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
  /* bufs 是数组，不可自增；用指针游标遍历 */
  vlib_buffer_t **buf_ptr = bufs;
  sr_wg_main_t *sm = &sr_wg_main;
  u32 fib_index = sm->encap_fib_index;
  /* SR Header 时间戳（秒级）。transit/decap 不做时效校验，取 VPP 时钟即可 */
  u32 now = (u32) vlib_time_now (vm);
  /*
   * 输出序列（Step 15）：成员包与"它触发出来的那个冗余包"按这个顺序**连续**
   * 入队，整帧一次 enqueue 走完。为什么不能学老写法分两批入队，见 Step 15。
   */
  u32 out_bufs[SR_WG_ENCAP_OUT_SLOTS];
  u16 out_nexts[SR_WG_ENCAP_OUT_SLOTS];
  u32 out_n = 0;
  u32 in_i = 0;   /* 当前包在 from[] 里的下标 */

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
      /* 终点段：封装前的 (外层 dst, 外层 UDP 目的端口)，两者都保持网络序 */
      sr_segment_t endpoint;
      ip4_address_t orig_dst;
      u8 *p;
      u32 tx_sw_if_index = vnet_buffer (b)->sw_if_index[VLIB_TX];
      u32 next_index = SR_WG4_ENCAP_NEXT_DROP;
      u32 ip4_len = 0, outer_len = 0, sr_len = 0, l2_len = 0;
      u16 src_port = 0;  /* 本端外层源端口 = 绑定表的键 */
      u16 orig_dport = 0;
      u16 path_id = 0;
      u8 seg_count = 0, seg_left = 0;
      u8 qos_cls = SR_QOS_CLASS_UNMARKED; /* 本包的 QoS 类别（Step 7.5 定） */
      u8 error = SR_WG4_ENCAP_ERROR_NONE;
      u8 bypass = 1;   /* 缺省直通：保证 WG 直连模式不受影响 */
      u32 parity_bi = (u32) ~0;  /* Step 14.5 造出的冗余包（~0 = 本包没有） */

      clib_memset (&orig_dst, 0, sizeof (orig_dst));
      clib_memset (&endpoint, 0, sizeof (endpoint));
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

      /*
       * 终点段在这里捕获：ip4->dst_address 与 udp->dst_port 都还是网络序，
       * 直接进 sr_segment_t 不用换序。必须在 Step 11 改写外层 dst 之前取，
       * 改写之后外层就是第一跳了，终点信息只剩在这一对局部变量里。
       */
      orig_dst = ip4->dst_address;
      orig_dport = clib_net_to_host_u16 (udp->dst_port);
      clib_memcpy_fast (endpoint.ip, &ip4->dst_address, 4);
      clib_memcpy_fast (endpoint.port, &udp->dst_port, 2);

      /*
       * Step 3: 环路守卫。载荷首字节已是 SR_MAGIC 说明本包已经封装过
       * SR Header。本节点改写外层 dst 后会交回 ip4-lookup，重查表的包
       * 会**第二次**经过本节点（同一张 underlay 网卡），这条判据保证
       * 第二次只走直通，不会二次封装。
       * 该判据安全：WG transport header 首字节是消息类型（1/2/3/4），
       * 永不等于 0x53。
       */
      if (PREDICT_FALSE (payload[0] == SR_MAGIC))
        goto done;

      /*
       * Step 4: 查绑定（键 = 本端外层 UDP 源端口）。
       *
       * 本数据面每隧道一个监听端口（定案 2），WG 把那个端口写进外层源端口，
       * 所以"这一包属于哪条隧道"在本机是**源端口**说了算。它是本机自己选的、
       * 发出去之前一定正确的值，因此不需要别的信息来佐证。
       *
       * 判据不能是外层 dst：那是**对端**的监听端口 + 对端地址，本机既不能保证
       * 对端改了配置后立刻知道，也不能保证它经过 NAT 后还是那个值（我们的前提
       * 是"源端口经 NAT 不变"，对端地址从来不是这个前提的一部分）。旧规则把
       * (对端 IP, 对端端口) 当键，代价是 endpoint 一旦漂移或压根没配，绑定就
       * 静默旁路 —— 隧道仍然通、SR 路径悄悄失效，是最难查的那类故障。
       *
       * 未命中即普通 WG / 非 WG 流量，直通。封装后第二遍经过本节点时源端口
       * 不变、仍会命中同一条绑定，但 Step 3 的 magic 判据已先拦住，不会二次封装。
       */
      src_port = clib_net_to_host_u16 (udp->src_port);
      bind = sr_wg_bind_lookup (sm, src_port);
      if (PREDICT_FALSE (bind == NULL))
        goto done;   /* 未绑定 → 普通 WG 隧道，直通 */

      /* Step 5: 查路径。已显式绑定却查不到/未激活属于配置错误，丢包并计数，
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
      seg_count = (u8) (path->relay_count + 1);

      /*
       * Step 6: 构造 SR Header（含 HMAC 签名）。
       * 终点段由 &endpoint 传入 —— 路径表里只有中转跳，终点必须**签名前**
       * 就位（它在 HMAC 覆盖范围内），所以不能等签完再往包里补。
       */
      if (PREDICT_FALSE (sr_path_fill_header (path, &srh, now, &endpoint) != 0))
        {
          error = SR_WG4_ENCAP_ERROR_HMAC_FAIL;
          bypass = 0;
          sm->counters.encap_hmac_fail++;
          sm->counters.drop_total++;
          goto done;
        }

      /* Step 7: 首跳免费——seg_left 覆盖为 relay_count（不在 HMAC 覆盖内） */
      seg_left = path->relay_count;
      srh.seg_left = seg_left;

      /*
       * Step 7.5: QoS 类别进 SR 头 action 高 3bit（docs/qos-path-c-plan.md §13）。
       *
       * 类别来源：入口标记器（sr-wg4-qos）经 vnet_buffer2(b)->qos 传过来——
       * 内层头此刻已进密文，encap 自己不可能重新分类，只信标记器；
       * 没被标记过（QOS_DATA_VALID 没置位，如本机自发的握手/keepalive）落
       * BULK，等价 ToS=0，行为与未加 QoS 时一致。
       *
       * 载体选 action 不选 magic：magic=0x53 被 sr_hdr_validate 和中转校验，
       * 动不得；action 在 HMAC 排除集（不用重签）、数据路径没人读、
       * validate 不检查 —— 三重零兼容代价（§12.2 已核）。
       * 这个字节是端到端权威类别：运营商可能重标记外层 ToS，但不会动我们的
       * UDP 载荷；中转与对端 decap 都从这里恢复。
       */
      {
        u8 cls = (b->flags & VNET_BUFFER_F_QOS_DATA_VALID)
                   ? sr_qos_class_of_buffer (b)
                   : SR_QOS_CLASS_UNMARKED;

        srh.action = SR_ACTION_FORWARD | sr_qos_class_to_action (cls);
        qos_cls = cls;
      }

      first_hop = &srh.segments[seg_count - 1];
      sr_len = sr_hdr_total_len (seg_count);

      /*
       * Step 8: headroom 守卫。
       * vlib_buffer_get_current() = b->data + b->current_data，
       * 可用 headroom = b->current_data + VLIB_BUFFER_PRE_DATA_SIZE。
       * 此处 current 仍指向 L2 头，Step 9 会先剥掉 l2_len 再抢 sr_len，
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
       * Step 9: 剥 L2 → 抢 headroom → 把外层 IP4/UDP 拉回包首。
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

      /* Step 10: 写入 SR Header（仅 sr_len 字节，不越界覆盖 WG 载荷） */
      clib_memcpy_fast (p + outer_len, &srh, sr_len);

      /* Step 11: 改写外层 IP4 dst / UDP dst_port 为第一跳。
       * relay_count=0 时第一跳就是终点自己（segments[0]，即 Step 2 之后捕获的
       * 那对原值），这两次写是等值自写，包内容不变。 */
      clib_memcpy_fast (&ip4->dst_address, first_hop->ip, 4);
      clib_memcpy_fast (&udp->dst_port, first_hop->port, 2);

      /*
       * Step 11.5: 类别写进最外层 IP ToS（类别代表 DSCP，见 sr_qos.h）。
       * RFC 3270 pipe 模式：外层 ToS 只服务"本机到下一跳"之间的调度
       * （含本机出向物理口调度），端到端权威在 action 3bit —— 运营商改了
       * ToS 也不怕，中转/对端会从 action 重断言。必须写在 Step 13 算校验和
       * 之前，否则 ToS 改了 checksum 没跟着改，包在下一跳被丢。
       */
      ip4->tos = (u8) (sr_qos_dscp_of_class (qos_cls) << 2);

      /* Step 12: 长度增加 sr_len */
      ip4->length =
        clib_host_to_net_u16 (clib_net_to_host_u16 (ip4->length) + sr_len);
      udp->length =
        clib_host_to_net_u16 (clib_net_to_host_u16 (udp->length) + sr_len);

      /* Step 13: 校验和。UDP checksum 置 0（与 sr-wg4-transit 保持一致，
       * 也是 WireGuard 的惯例）；同时清掉所有 offload 标志，避免下游按
       * 过期的 hdr offset 重算。 */
      vnet_buffer_offload_flags_clear (b, vnet_buffer (b)->oflags);
      b->flags &= ~(VNET_BUFFER_F_L2_HDR_OFFSET_VALID |
                    VNET_BUFFER_F_L3_HDR_OFFSET_VALID |
                    VNET_BUFFER_F_L4_HDR_OFFSET_VALID);
      udp->checksum = 0;
      ip4->checksum = ip4_header_checksum (ip4);

      /*
       * Step 14: 交回 ip4-lookup 重新查表。
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

      /*
       * Step 14.5: FEC（插件 1.2.0，sr_fec.h 设计 3）。
       *
       * 数据包已经封装完、马上交回 ip4-lookup —— FEC 只顺带把它的 WG 帧
       * 异或进所属隧道的发送组，组满（第 ratio 个成员）时造一个冗余包
       * 走同一条出口路径。数据包本身不被修改、不延迟：组不满时这一步只有
       * 内存异或，流量停在半组时那段就没有冗余（没有定时器，故意）。
       *
       * 这里只把冗余包的 buffer index 记下来，**入队留到 Step 15 跟成员包
       * 排在一起** —— 顺序是协议语义（sr_fec.h 设计 4 的计数法配对），
       * 早一步、晚一步都会让对端数错。
       *
       * wg_len 从更新过的 udp->length 反推：Step 12 加过 sr_len，所以
       * WG 帧长 = UDP 总长 - 8 - sr_len。
       */
      if (PREDICT_FALSE (bind->fec_ratio > 0))
	parity_bi = sr_fec_tx (vm, sm, bind, b, p, outer_len,
			       (u16) sr_len,
			       (u16) (clib_net_to_host_u16 (udp->length) -
				      8 - sr_len),
			       fib_index);

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
          tr->src_port = src_port;
          tr->orig_dst = orig_dst;
          tr->orig_dport = orig_dport;
          tr->current_data = b->current_data;
          tr->error = error;
          tr->bypass = bypass;
          tr->qos_cls = qos_cls;
          if (!bypass && seg_count)
            {
              clib_memcpy_fast (&tr->first_hop_ip, srh.segments[seg_count - 1].ip,
                                4);
              u16 port_net;
              clib_memcpy_fast (&port_net, srh.segments[seg_count - 1].port, 2);
              tr->first_hop_port = clib_net_to_host_u16 (port_net);
            }
        }

      /*
       * Step 15: 按线上顺序入队 —— 成员包，紧接着它触发出来的冗余包。
       *
       * 老写法是"成员包整帧入队，冗余包再补一次 enqueue"，一帧里挤进两个
       * 以上的组时，第 N 组的冗余包就排到第 N+1 组的数据包后面。对端
       * （sr_fec.c 的计数法配对）是**数**两次冗余包之间的数据帧数 j 的：
       * 冗余包晚到几个包，j 就多算几个，本来 j == k（全到）或 k-1（丢一
       * 个可还原）的组被判成"多丢"，这一组的冗余白费。一帧里的包越多错
       * 得越狠 —— 容器 lab（tap 批量收包）200 pps 下 500 组错 271 次；
       * 真机 200 pps 因为单包成帧而看不出来，突发/高包速才露头。
       * 数字见 docs/fec-and-lb-plan.md §9。
       *
       * 冗余包的 ToS 与 action 类别位被 sr_fec_tx 清零（走 BE，不抢业务
       * 类带宽），所以它跟成员包**不一定同一条出口队列**：一旦开了 QoS
       * 整形，调度器仍可能把它们换个位置。这里消除的是节点内部这个主因，
       * 不是"任何配置下都对齐"的保证。
       */
      out_bufs[out_n] = from[in_i];
      out_nexts[out_n] = (u16) next_index;
      out_n++;

      if (PREDICT_FALSE (parity_bi != (u32) ~0))
        {
          out_bufs[out_n] = parity_bi;
          out_nexts[out_n] = SR_WG4_ENCAP_NEXT_IP4_LOOKUP;
          out_n++;
        }

      in_i++;
      n_left--;
      buf_ptr++;
    }

  vlib_buffer_enqueue_to_next (vm, node, out_bufs, out_nexts, out_n);

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
  if (!sm->src_port_path)
    sm->src_port_path = hash_create (0, sizeof (uword));
  if (!sm->encap_enabled_ifs)
    sm->encap_enabled_ifs = hash_create (0, sizeof (uword));
  /* 一台盒子的绑定数与路径数同阶（一隧道一路径），共用 SR_MAX_PATHS 容量 */
  if (!sm->binds)
    pool_alloc (sm->binds, SR_MAX_PATHS);
}

/*
 * 内部：登记/摘除 bind 在 src_port_path 里的键。
 *
 * src_port = 0 表示这条绑定还没给出可用的键（控制面还没把这台盒子的监听端口
 * 填进来），此时它只存在于 tunnel_path 里 —— 数据面永远查不到它，
 * `show sr-wg tunnels` 会把它标成未生效而不是假装命中。
 */
static void
sr_wg_bind_set_port (sr_wg_main_t *sm, const sr_wg_tunnel_bind_t *bind, u32 bi)
{
  if (bind->src_port)
    hash_set (sm->src_port_path, (uword) bind->src_port, bi + 1);
}

static void
sr_wg_bind_unset_port (sr_wg_main_t *sm, const sr_wg_tunnel_bind_t *bind)
{
  if (bind->src_port)
    hash_unset (sm->src_port_path, (uword) bind->src_port);
}

sr_wg_tunnel_bind_t *
sr_wg_bind_lookup (sr_wg_main_t *sm, u16 src_port)
{
  uword *p;

  p = hash_get (sm->src_port_path, (uword) src_port);
  if (p && p[0] != 0)
    return pool_elt_at_index (sm->binds, p[0] - 1);

  return NULL;
}

int
sr_wg_tunnel_set_path (sr_wg_main_t *sm, u32 sw_if_index, u16 path_id,
                       u16 src_port, int is_add)
{
  vnet_main_t *vnm = vnet_get_main ();
  sr_wg_tunnel_bind_t *bind;
  u16 old_port;
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

      sr_wg_bind_unset_port (sm, bind);
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

  p = hash_get (sm->tunnel_path, (uword) sw_if_index);
  if (p && p[0] != 0)
    {
      bi = p[0] - 1;
      bind = pool_elt_at_index (sm->binds, bi);
    }
  else
    {
      pool_get_zero (sm->binds, bind);
      bi = bind - sm->binds;
      hash_set (sm->tunnel_path, (uword) sw_if_index, bi + 1);
    }

  /*
   * 改了本端源端口 = 这条隧道换了身份，旧键必须摘掉：留着就是一条僵尸键，
   * 会把"还在用旧端口发"的那条流的包拽进这条路径（旧端口此后归属谁未定）。
   */
  old_port = bind->src_port;

  bind->sw_if_index = sw_if_index;
  bind->path_id = path_id;
  bind->src_port = src_port;

  if (old_port && old_port != src_port)
    hash_unset (sm->src_port_path, (uword) old_port);

  sr_wg_bind_set_port (sm, bind, bi);

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

      /* 摘掉本端源端口键：不摘就留一条指向已回收 bind 的僵尸键 */
      sr_wg_bind_unset_port (sm, bind);
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

void
sr_wg_encap_reconcile (sr_wg_main_t *sm)
{
  vnet_main_t *vnm = vnet_get_main ();
  u32 *dead = 0;
  u32 count = 0;
  uword key, val;
  int i;

  if (!sm->encap_enabled_ifs)
    return;

  /*
   * 判据取 VPP 自己的登记，不取我们的 hash：接口已删除（format_vnet_sw_if_index_name
   * 只能打出 DELETED(n)），或者接口还在但 ip4-output 上已经没有这个 feature
   * （含"号被别的接口复用而 arc 没跟着复用"这一种），两种都算这条挂点没了。
   */
  /* *INDENT-OFF* */
  hash_foreach (key, val, sm->encap_enabled_ifs, ({
    if (!vnet_get_sw_interface_or_null (vnm, (u32) key) ||
        !vnet_feature_is_enabled (SR_WG_ENCAP_ARC, SR_WG_ENCAP_NODE,
                                  (u32) key))
      vec_add1 (dead, (u32) key);
  }));
  /* *INDENT-ON* */

  for (i = 0; i < vec_len (dead); i++)
    hash_unset (sm->encap_enabled_ifs, (uword) dead[i]);
  vec_free (dead);

  /* *INDENT-OFF* */
  hash_foreach (key, val, sm->encap_enabled_ifs, ({ count++; }));
  /* *INDENT-ON* */
  sm->encap_enabled_count = count;
}
