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
#include <vppinfra/hash.h>
#include <vppinfra/pool.h>

#include "sr_header.h"
#include "sr_hmac.h"
#include "sr_path.h"
#include "sr_counters.h"

#define SR_WG_PLUGIN_NAME  "sr_wg"
#define SR_WG_MAX_KEYS     64

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
 * Encap 绑定表条目：隧道接口 / 端点 IP -> SR path_id
 *
 * 单独用结构体是为了让 CLI / API dump 可以遍历展示。
 *
 * 挂点在 underlay 出口接口，所以数据面真正生效的键是 **endpoint**
 * （外层 dst = WG peer endpoint）。sw_if_index 只在"把 underlay 接口
 * 整体绑到一条路径"这种场景下才命中，保留它是为了兼容旧的按隧道配置。
 */
typedef struct {
  u32 sw_if_index;        /* WG 隧道接口 sw_if_index（tunnel_path 表用） */
  ip4_address_t endpoint; /* WG peer endpoint IPv4（endpoint_path 表用，必配） */
  u16 path_id;
} sr_wg_tunnel_bind_t;

/* 插件主结构 */
typedef struct {
  vlib_main_t *vlib_main;
  vnet_main_t *vnet_main;

  /* 路径表 */
  sr_path_table_t path_table;

  /* 密钥表 */
  sr_wg_key_entry_t keys[SR_WG_MAX_KEYS];
  u32 key_count;

  /* Transit 配置 */
  u16 transit_port;       /* 默认 51821 */
  u16 vpn_port;           /* 默认 51820 */
  u8  transit_enabled;

  /* ==================== Encap 配置 ==================== */

  /*
   * 隧道-路径绑定表（两种索引，指向同一份 sr_wg_tunnel_bind_t 池）：
   *   endpoint_path : key = 外层 dst IPv4（主机序 as_u32），
   *                   **underlay 挂点下真正生效的索引**，优先级最高
   *   tunnel_path   : key = sw_if_index。挂点看到的是 underlay 接口，
   *                   故它是"整张网卡一条默认路径"的粗粒度配法，
   *                   仅在 endpoint 未命中时兜底；绑定 wg0 的条目
   *                   在此挂点下不会命中（历史遗留，兼容用）
   * value 均为 bind 池索引 + 1（0 = 无绑定）。
   */
  uword *tunnel_path;
  uword *endpoint_path;
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

  /* Node indices（缓存） */
  u32 transit_node_index;
  u32 input_node_index;
  u32 encap_node_index;

  /* API */
  u16 msg_id_base;
} sr_wg_main_t;

extern sr_wg_main_t sr_wg_main;

/* ==================== 密钥管理 ==================== */

int sr_wg_key_add(sr_wg_main_t *sm, u16 hmac_id, const u8 *key);
int sr_wg_key_del(sr_wg_main_t *sm, u16 hmac_id);
sr_wg_key_entry_t *sr_wg_key_lookup(sr_wg_main_t *sm, u16 hmac_id);

/* ==================== Encap：隧道-路径绑定 ==================== */

/* 初始化绑定表（hash + pool）。重复调用安全。 */
void sr_wg_encap_init(sr_wg_main_t *sm);

/*
 * 绑定 / 解绑 WG 隧道接口到 SR 路径。
 *   is_add   : 1 = 绑定（path_id 必须存在于路径表）
 *              0 = 解绑
 *              注意：**不会**自动启用 feature。挂点在 underlay 出口接口，
 *              与本函数的 sw_if_index 无关，必须由 `set sr-wg encap enable
 *              <underlay>`（或对应 API）单独打开。
 *   endpoint : WG peer 的外层地址。underlay 挂点下这是唯一能命中绑定的键，
 *              不传则该绑定永远不会生效（CLI 会给出警告）。
 * 返回 0 成功，非 0 为 VNET_API_ERROR_* 错误码。
 */
int sr_wg_tunnel_set_path(sr_wg_main_t *sm, u32 sw_if_index, u16 path_id,
                          const ip4_address_t *endpoint, int is_add);

/*
 * 在 ip4-output arc 上为指定 **underlay 出口接口** 启用 / 禁用
 * sr-wg4-encap feature，并记录到 sm->encap_enabled_ifs。
 * 返回 0 成功，非 0 为 VNET_API_ERROR_* 错误码。
 */
int sr_wg_encap_enable_disable(sr_wg_main_t *sm, u32 sw_if_index, int enable);

/* 指定接口上是否已启用 sr-wg4-encap（控制面/CLI 校验用，非热路径）。 */
int sr_wg_encap_is_enabled(sr_wg_main_t *sm, u32 sw_if_index);

/* 查找绑定条目；未绑定返回 NULL。数据面热路径使用。 */
sr_wg_tunnel_bind_t *sr_wg_bind_lookup(sr_wg_main_t *sm, u32 sw_if_index,
                                       ip4_address_t dst);

/*
 * 删除路径时清理仍指向它的隧道绑定，返回清理条数。
 * 悬空绑定不会立刻报错，只会在收包时按 encap_no_path 静默丢包，
 * 所以 sr_path_del 必须把绑定一起摘掉，控制面才不会出现"路径没了、
 * 流量还在悄悄旁路"的状态。
 */
u32 sr_wg_binds_drop_for_path(sr_wg_main_t *sm, u16 path_id);

/*
 * 按当前 vpn_port / transit_port 重新注册 UDP 目的端口。
 * 建 WireGuard 接口会把 51820 重新注册给 wg4-input，transit 换端口也必须
 * 重注册 —— 端口就是数据面的功能多路复用键，注册表和配置不一致时
 * 包根本进不到我们的节点。
 */
void sr_wg_ports_rebind(sr_wg_main_t *sm);

/* ==================== Node 声明 ==================== */

extern vlib_node_registration_t sr_wg4_transit_node;
extern vlib_node_registration_t sr_wg4_input_node;
extern vlib_node_registration_t sr_wg4_encap_node;

/* ==================== API ==================== */

void sr_wg_api_hookup(vlib_main_t *vm);

#endif /* SR_WG_H */
