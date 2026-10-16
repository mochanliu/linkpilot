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
 * WireGuard 插件自身把 "wg4-output-tun" 注册为 "ip4-output" arc 上的
 * feature，并在每个 wg 接口上启用（可用 `show interface features wg0`
 * 验证）。"ip4-output" arc 的 start_nodes 是 ip4-midchain /
 * ip4-mcast-midchain（隧道封装出口），arc 已由 libvnet 注册，本插件
 * 只需追加一个 feature，不能重复声明 VNET_FEATURE_ARC_INIT。
 *
 * 因此 sr-wg4-encap 必须 runs_after wg4-output-tun：
 *   ip4-midchain → wg4-output-tun（加密 + 外层 IP4/UDP）
 *                → sr-wg4-encap（插入 SR Header + 改写外层 dst）
 *                → [arc end] adj-midchain-tx → ip4-rewrite → 网卡 tx
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
 * 以 uword 为 key 存入 hash（path_id + 1，0 表示无绑定）。
 * 单独用结构体是为了让 CLI / API dump 可以遍历展示。
 */
typedef struct {
  u32 sw_if_index;    /* WG 隧道接口 sw_if_index（tunnel_path 表用） */
  ip4_address_t endpoint; /* WG peer endpoint IPv4（endpoint_path 表用） */
  u16 path_id;
  u8  feature_on;     /* ip4-output arc 上的 sr-wg4-encap 是否已启用 */
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
   *   tunnel_path   : key = sw_if_index，encap 节点首选（O(1)）
   *   endpoint_path : key = 外层 dst IPv4（主机序 as_u32），
   *                   当 TX sw_if_index 不可用或需要按 peer 区分时的回退
   * value 均为 bind 池索引 + 1（0 = 无绑定）。
   */
  uword *tunnel_path;
  uword *endpoint_path;
  sr_wg_tunnel_bind_t *binds;   /* pool */

  /*
   * Encap 后重新查表使用的 FIB index。
   * 外层 dst 被改写为第一跳后，必须重新走 ip4-lookup 才能得到正确的
   * 父邻接 L2 rewrite（arc 默认的 adj-midchain-tx 用的是原 peer endpoint
   * 对应的父邻接，其 dst MAC 与新第一跳不匹配）。
   * SR-WG 的 underlay 一般在默认表，故缺省 0，可由 CLI/API 调整。
   */
  u32 encap_fib_index;

  /* 本机地址（transit 改写外层 src 用） */
  ip4_address_t local_ip4;

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
 *   is_add   : 1 = 绑定（path_id 必须存在于路径表，绑定成功后自动
 *                   在 ip4-output arc 上启用 sr-wg4-encap feature）
 *              0 = 解绑（同时禁用 feature）
 *   endpoint : 可选，非 NULL 且非 0 时同时建立 endpoint_ip -> path 索引
 * 返回 0 成功，非 0 为 VNET_API_ERROR_* 错误码。
 */
int sr_wg_tunnel_set_path(sr_wg_main_t *sm, u32 sw_if_index, u16 path_id,
                          const ip4_address_t *endpoint, int is_add);

/*
 * 在 ip4-output arc 上为指定接口启用 / 禁用 sr-wg4-encap feature。
 * 返回 0 成功，非 0 为 VNET_API_ERROR_* 错误码。
 */
int sr_wg_encap_enable_disable(sr_wg_main_t *sm, u32 sw_if_index, int enable);

/* 查找绑定条目；未绑定返回 NULL。数据面热路径使用。 */
sr_wg_tunnel_bind_t *sr_wg_bind_lookup(sr_wg_main_t *sm, u32 sw_if_index,
                                       ip4_address_t dst);

/* ==================== Node 声明 ==================== */

extern vlib_node_registration_t sr_wg4_transit_node;
extern vlib_node_registration_t sr_wg4_input_node;
extern vlib_node_registration_t sr_wg4_encap_node;

/* ==================== API ==================== */

void sr_wg_api_hookup(vlib_main_t *vm);

#endif /* SR_WG_H */
