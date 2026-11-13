/*
 * sr_wg.h - SR-WG VPP 插件主头文件
 *
 * 定义插件全局状态、密钥管理接口和 node 声明。
 */
#ifndef SR_WG_H
#define SR_WG_H

#include <vlib/vlib.h>
#include <vnet/vnet.h>
#include <vnet/ip/ip.h>
#include <vnet/udp/udp_local.h>
#include <vnet/ip/ip4_packet.h>
#include <vnet/feature/feature.h>
#include <vppinfra/bitmap.h>
#include <vppinfra/hash.h>
#include <vppinfra/pool.h>

#include "sr_header.h"
#include "sr_hmac.h"
#include "sr_path.h"
#include "sr_counters.h"
#include "sr_obs.h"
#include "sr_wg_probe.h"

#define SR_WG_PLUGIN_NAME  "sr_wg"

/*
 * 密钥表容量：一把 p-key 对应一个"发签/验签的身份"，今天控制器是**每租户一把**
 * （不是每隧道一把），所以这个数不是隧道数上限。但中转盒要给多个租户的流量验
 * 签，同一张表要装下所有引用到的身份；64 是早期随手定的，跟路径表一样抬到
 * 4096，免得"密钥表先满"变成一个看不见的租户上限。
 */
#define SR_WG_MAX_KEYS     4096

/* hmac_id 是 16 位，定位表可以定长：一次数组访问拿到槽位。 */
#define SR_WG_KEY_ID_SPAN  65536

/* sr_wg_key_add 的返回值（调用方必须分得清"参数不对"和"表满了"） */
#define SR_WG_KEY_OK          0
#define SR_WG_KEY_ERR_PARAM  (-1)
#define SR_WG_KEY_ERR_FULL   (-2)

/* 外层 IP4(20) + UDP(8) 回退量（无 IPv4 选项场景） */
#define SR_OUTER_HDR_LEN   (20 + 8)

/*
 * Encap 节点挂载的 feature arc / feature 名。
 *
 * 挂点必须是 **underlay 出口接口**（如 host-eth0），不是 wg0。原因是
 * VPP 26.06 的 wireguard 插件（src/plugins/wireguard/wireguard_output_tun.c）
 * 把 next 硬编码为 adj-midchain-tx，全程不调用 vnet_feature_next()：
 *   [实测] show node wg4-output-tun → adj-midchain-tx=54 vectors,
 *          sr-wg4-encap=0 vectors（即使 feature 已按 runs_after 排在 wg0 上）
 * 所以挂在 wg0 的 ip4-output arc 上是**结构性死代码**，加密后的包永远不会
 * 经过我们。
 *
 * 而 "ip4-output" arc 的 start_nodes 含 ip4-rewrite（ip4_forward.c:
 * VNET_FEATURE_ARC_INIT(ip4_output)），WG 加密后的外层包最终一定走：
 *   wg4-output-tun → adj-midchain-tx → ip4-load-balance → ip4-rewrite
 *                  → [host-eth0 的 ip4-output arc] → interface-output → 网卡
 * 因此把同一个 feature 启用在 underlay 接口上就能拿到包（已实测：
 * `set sr-wg encap enable 1` 后 4 个 ping 精确地变成 4 vectors）。
 *
 * 代价与约束：该 arc 上 ip4-rewrite 已经写完 L2，所以节点入口处
 * 缓冲区是 [ETH][外层 IP4][UDP][WG transport][密文]，必须先剥 L2 再插
 * SR Header，改完外层 dst 后交回 ip4-lookup 重查第一跳的邻接。
 */
#define SR_WG_ENCAP_ARC    "ip4-output"
#define SR_WG_ENCAP_NODE   "sr-wg4-encap"

/* P-Key 密钥表条目 */
typedef struct {
  u16 hmac_id;
  u8  key[SR_PKEY_LEN];
  u8  active;
} sr_wg_key_entry_t;

/*
 * Encap 绑定表条目：隧道 -> SR path_id
 *
 * 单独用结构体是为了让 CLI / API dump 可以遍历展示。
 *
 * 数据面生效的键是**本端的外层 UDP 源端口**：这个数据面每条隧道占一个自己的
 * 监听端口（定案 2），WG 发包时把它写进外层源端口，所以"这一包属于哪条隧道"
 * 在本机是端口说了算 —— 而不是对端地址说了算。
 *
 * 对端地址不再参与选路，这是有意的：endpoint 可能是 NAT 学来的、会变，也可能
 * 压根没配（WG 允许一端不配 endpoint）。把它当键，等于让"路径"依赖一个本端
 * 不掌握、还会漂移的值。src_port = 0 表示这条绑定还没给出可用的键，
 * 数据面永远不会命中它。
 *
 * sw_if_index 是隧道接口号：一条隧道一张 WG 接口，配置侧靠它定位要改/删哪条
 * 绑定（一接口一绑定），不参与收包判定。
 */
typedef struct {
  u32 sw_if_index;        /* WG 隧道接口 sw_if_index（配置侧定位用） */
  u16 src_port;           /* 本端外层 UDP 源端口 = 这条隧道的监听端口 */
  u16 path_id;

  /*
   * FEC（sr_fec.h / 插件 1.2.0）。fec_ratio = 每几个数据包 1 个异或冗余包
   * （0 = 关，1..4）；fec_idx = sr_fec 状态池下标 + 1（0 = 没有状态）。
   * 经 sr_wg_fec_set（API）或 `set sr-wg fec`（CLI）设置；绑定原地更新时
   * 两个值都保留（控制面对账重放不会抹掉 FEC 配置）。
   */
  u8  fec_ratio;
  u32 fec_idx;
} sr_wg_tunnel_bind_t;

/* 插件主结构 */
typedef struct {
  vlib_main_t *vlib_main;
  vnet_main_t *vnet_main;

  /* 路径表 */
  sr_path_table_t path_table;

  /* 密钥表 */
  sr_wg_key_entry_t keys[SR_WG_MAX_KEYS];
  /*
   * key_index[hmac_id] = 槽位下标 + 1，0 表示"这把钥匙不在表里"。
   * 存 +1 是因为槽 0 是合法位置，而 0 必须留给"没有"。
   *
   * 这张表存在的理由是**热路径**：sr-wg4-transit 与 sr-wg4-input 每个收包都要按
   * 头部声明的 hmac_id 取一次密钥，4096 把线性扫在最坏情况下要比较 4096 次。
   * keys[] 仍保持紧凑（[0..key_count-1] 有效，删除用末位搬移），dump/CLI 的遍历
   * 照旧走 key_count。
   */
  u16 key_index[SR_WG_KEY_ID_SPAN];
  u32 key_count;

  /* ==================== UDP 目的端口集合（功能多路复用键） ====================
   *
   * 定案 2："未来扩展的所有 function 都是通过 udp 端口来区分"，而且**每隧道一个
   * 端口**：同一对 CPE 可以开多条 WG 隧道，各自绑不同监听端口。所以这里必须是
   * 集合，不能是单个 vpn_port —— 单个端口时，第二条隧道的包进不到 sr-wg4-input
   * （不解密、不进观测表），结果是"隧道建起来了、SR 静默没生效"。
   *
   *   vpn_ports     -> sr-wg4-input   （解封装；magic != 0x53 旁路给 wg4-input）
   *   transit_ports -> sr-wg4-transit （验签 + 弹一段，不解密）
   *
   * 集合只用于**收包**分发表。encap 不看这些集合：它是发送侧，判据是绑定表里
   * 的本端外层 UDP 源端口（= 这条隧道的监听端口），与"本机在听哪些端口"无关。
   * 发往对端的那个目的端口是对端的监听端口，本机集合里没有它，也不需要。
   *
   * 用 clib_bitmap（≤64K 位，按需增长）而不是向量：UDP 分发表按目的端口
   * 查成员，bitmap 取值是 O(1)；用向量就得每个包线性扫一遍。
   * 两个集合都缺省带一个种子端口（51820 / 51821），使既有单隧道配置不改也能跑。
   *
   * 类型注意：VPP 的 `typedef uword clib_bitmap_t`，位图变量必须写成指针
   * （clib_bitmap_t *  ==  uword *），vec_new / clib_bitmap_set 都按这个来。
   */
  clib_bitmap_t *vpn_ports;
  clib_bitmap_t *transit_ports;
  u8  transit_enabled;

  /* ==================== Encap 配置 ==================== */

  /*
   * 隧道-路径绑定表（两种索引，指向同一份 sr_wg_tunnel_bind_t 池）：
   *   src_port_path : key = 本端外层 UDP 源端口。**数据面唯一用的索引** ——
   *                   每条隧道一个监听端口（定案 2），WG 把它写进外层源端口，
   *                   所以本机发出去的那个包自带隧道身份。
   *   tunnel_path   : key = WG 隧道接口 sw_if_index。不参与收包判定，只用于
   *                   配置侧定位"改/删这张接口的绑定"（一接口一绑定）以及
   *                   删路径时回收指向它的绑定。
   * value 均为 bind 池索引 + 1（0 = 无绑定）。
   */
  uword *tunnel_path;
  uword *src_port_path;
  sr_wg_tunnel_bind_t *binds;   /* pool */

  /*
   * sr-wg4-encap feature 已启用的 **underlay 出口接口** 集合。
   * key = sw_if_index，value = 1；count 用于 CLI/API 判断"是否一条都没开"
   * （开了绑定却没开接口 = 静默旁路，必须能看出来）。
   */
  uword *encap_enabled_ifs;
  u32 encap_enabled_count;

  /*
   * Encap 后重新查表使用的 FIB index。
   * 外层 dst 被改写为第一跳后，必须重新走 ip4-lookup 才能得到第一跳的
   * 父邻接 L2 rewrite（arc 默认的 adj-midchain-tx 用的是原 peer endpoint
   * 对应的父邻接，其 dst MAC 与第一跳不匹配）。
   *
   * 该值是通过 **vnet_buffer(b)->sw_if_index[VLIB_TX]** 传给 ip4-lookup 的：
   * 26.06 的 ip4-lookup 约定该字段 == ~0 时按"收包接口的 FIB"做 LPM，
   * 否则直接把它当 FIB index 用（不是接口索引）。详见 sr_wg4_encap.c Step 15。
   *
   * SR-WG 的 underlay 一般在默认表，故缺省 0，可由 CLI/API 调整。
   */
  u32 encap_fib_index;

  /* 计数器 */
  sr_counters_t counters;

  /*
   * 外层源观测表（vBOND 地址发现的数据面依据，见 sr_obs.h）。
   * 采集点三处：sr-wg4-input 的解封装成功路径（verified=1，发送者持有 p-key，
   * 但地址本身不在签名覆盖范围内）、sr-wg4-input 的旁路路径（verified=0，普通 WG 的
   * handshake-init/keepalive，入网阶段只能靠它），以及 sr-wg4-transit 的
   * 转发路径（verified=1，dst_port = transit_port，hub 形态下最自然的证人）。
   * 地址发现的信号量就是 keepalive，不再另开探针 socket 去量。
   */
  sr_obs_table_t obs;

  /*
   * 隧道内质量探针（延迟/抖动/丢包）。可达性仍由 VPP core BFD 负责，
   * 两套会话表在 CLI/北向合成一张视图（定案 2026-10-24 方案 C）。
   */
  sr_probe_main_t probe;

  /* Node indices（缓存） */
  u32 transit_node_index;
  u32 input_node_index;
  u32 encap_node_index;

  /* API */
  u16 msg_id_base;
} sr_wg_main_t;

extern sr_wg_main_t sr_wg_main;

/* 日志类别（sr_wg.c 中 VLIB_REGISTER_LOG_CLASS 定义） */
extern vlib_log_class_registration_t sr_wg_log;

/* ==================== 密钥管理 ==================== */

/*
 * 添加/更新密钥（同一 hmac_id 再来一次就是换密钥，会同步刷新路径里的副本）。
 * 返回 SR_WG_KEY_OK；参数非法 SR_WG_KEY_ERR_PARAM；表里已有 SR_WG_MAX_KEYS 把
 * 且 hmac_id 是新的，SR_WG_KEY_ERR_FULL —— 覆盖已有密钥不受容量限制。
 */
int sr_wg_key_add(sr_wg_main_t *sm, u16 hmac_id, const u8 *key);
int sr_wg_key_del(sr_wg_main_t *sm, u16 hmac_id);
sr_wg_key_entry_t *sr_wg_key_lookup(sr_wg_main_t *sm, u16 hmac_id);

/* ==================== Encap：隧道-路径绑定 ==================== */

/* 初始化绑定表（hash + pool）。重复调用安全。 */
void sr_wg_encap_init(sr_wg_main_t *sm);

/*
 * 绑定 / 解绑 WG 隧道接口到 SR 路径。
 *   is_add    : 1 = 绑定（path_id 必须存在于路径表）
 *               0 = 解绑
 *               注意：**不会**自动启用 feature。挂点在 underlay 出口接口，
 *               与本函数的 sw_if_index 无关，必须由 `set sr-wg encap enable
 *               <underlay>`（或对应 API）单独打开。
 *   src_port  : 本端外层 UDP 源端口 = 这条隧道的监听端口，数据面唯一的键。
 *               0 = 还没给出可用键，这条绑定永远不会命中（CLI/API 会报出来）。
 *               两张接口填同一个 src_port 时后写的赢：本数据面是点对点隧道，
 *               一个端口只会对应一条隧道，配重了属于控制面 bug，
 *               `show sr-wg tunnels` 全表可查，插件不做额外防护。
 * 返回 0 成功，非 0 为 VNET_API_ERROR_* 错误码。
 */
int sr_wg_tunnel_set_path(sr_wg_main_t *sm, u32 sw_if_index, u16 path_id,
                          u16 src_port, int is_add);

/*
 * 在 ip4-output arc 上为指定 **underlay 出口接口** 启用 / 禁用
 * sr-wg4-encap feature，并记录到 sm->encap_enabled_ifs。
 * 返回 0 成功，非 0 为 VNET_API_ERROR_* 错误码。
 */
int sr_wg_encap_enable_disable(sr_wg_main_t *sm, u32 sw_if_index, int enable);

/* 指定接口上是否已启用 sr-wg4-encap（控制面/CLI 校验用，非热路径）。 */
int sr_wg_encap_is_enabled(sr_wg_main_t *sm, u32 sw_if_index);

/*
 * 把 sm->encap_enabled_ifs / encap_enabled_count 对回 VPP 的真实登记，
 * 摘掉接口已删除、或 ip4-output 上已经没有本 feature 的条目。
 *
 * 为什么需要：enable_disable 只在**写的时候**校验接口存在，接口后来被删除时没有任何
 * 钩子来摘这条记录，而 `set sr-wg encap disable <已删的号>` 也会被同一处校验挡回
 * INVALID_SW_IF_INDEX —— 于是实测出现过 `show sr-wg tunnels` 里的
 * `DELETED (3) (sw_if_index=3)`，它会一直活到 VPP 重启。后果不是转发（本 feature 的
 * 生效与否由 VPP 的 arc 决定），而是 **encap_enabled_count 虚高**：tunnel details 的
 * is_effective 与 CLI 的 WARNING 都读这个数，"看起来开了其实没开"就是从这里来的。
 * 所以所有读这张表的地方（CLI show / set tunnel 的告警 / API tunnel_details /
 * config_dump）进来先 reconcile 一次。
 */
void sr_wg_encap_reconcile(sr_wg_main_t *sm);

/*
 * 按本端外层 UDP 源端口（主机序）查绑定条目；未绑定返回 NULL。
 * 数据面热路径使用：encap 每个出站包一次，命中即"这一包属于这条隧道"。
 */
sr_wg_tunnel_bind_t *sr_wg_bind_lookup(sr_wg_main_t *sm, u16 src_port);

/*
 * 删除路径时清理仍指向它的隧道绑定，返回清理条数。
 * 悬空绑定不会立刻报错，只会在收包时按 encap_no_path 静默丢包，
 * 所以 sr_path_del 必须把绑定一起摘掉，控制面才不会出现"路径没了、
 * 流量还在悄悄旁路"的状态。
 */
u32 sr_wg_binds_drop_for_path(sr_wg_main_t *sm, u16 path_id);

/*
 * 按当前端口集合重新注册 UDP 目的端口：vpn_ports 全部 -> sr-wg4-input，
 * transit_ports 全部 -> sr-wg4-transit。
 * 建 WireGuard 接口会把它自己的 listen-port 重新注册给 wg4-input（覆盖 51820
 * 以及任何一条隧道的端口），transit 换端口也一样 —— 端口就是数据面的功能
 * 多路复用键，注册表和配置不一致时包根本进不到我们的节点。
 */
void sr_wg_ports_rebind(sr_wg_main_t *sm);

/* ==================== UDP 端口集合（每隧道一个端口） ==================== */

/* 端口种类：与 vl_api_sr_wg_port_* 的 kind 字段同值，避免两处各数一遍。 */
#define SR_WG_PORT_VPN     0
#define SR_WG_PORT_TRANSIT 1

/*
 * 增/删一个 UDP 目的端口。**两个调用都会立刻作用到 UDP 分发表**，
 * 不需要另外 rebind：漏一次 rebind 就是"配置说端口归我们、包却进了 wg4-input"
 * 这种最难查的静默状态。
 *   port = 0            -> VNET_API_ERROR_INVALID_VALUE
 *   重复 add / 删不存在的端口 -> 幂等，返回 0（控制器对账时不必先 dump）
 * 删除 vpn 端口时把端口交回 wg4-input（它是 WG 接口的 listen-port，交回之后
 * 那条隧道仍能当普通 WG 用）；删除 transit 端口时直接反注册（那端口本就不
 * 属于 wireguard）。
 */
int sr_wg_port_add_del(sr_wg_main_t *sm, u8 kind, u16 port, int is_add);

/* 集合大小（CLI/API 汇报用；不是热路径，走 popcount）。 */
u32 sr_wg_port_count(sr_wg_main_t *sm, u8 kind);

/* ==================== Node 声明 ==================== */

extern vlib_node_registration_t sr_wg4_transit_node;
extern vlib_node_registration_t sr_wg4_input_node;
extern vlib_node_registration_t sr_wg4_encap_node;
extern vlib_node_registration_t sr_wg4_post_node;
extern vlib_node_registration_t sr_wg4_mpls_out_node;
extern vlib_node_registration_t sr_wg4_probe_node;
extern vlib_node_registration_t sr_wg_probe_process_node;

/* ==================== API ==================== */

void sr_wg_api_hookup(vlib_main_t *vm);

#endif /* SR_WG_H */
