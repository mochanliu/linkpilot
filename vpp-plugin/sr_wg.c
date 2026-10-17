/*
 * sr_wg.c - SR-WG VPP 插件入口和初始化
 *
 * 注册插件、初始化全局状态、注册 UDP 端口处理节点。
 */
#include "sr_wg.h"
#include <vlib/unix/plugin.h>
#include <vpp/app/version.h>

sr_wg_main_t sr_wg_main;

/* 注册日志类别：VLIB_REGISTER_LOG_CLASS 展开为
 * vlib_log_class_registration_t 类型的变量，实际的 log class 句柄在 .class 中 */
VLIB_REGISTER_LOG_CLASS (sr_wg_log) = {
  .class_name = "sr_wg",
};

/* ==================== 插件注册 ==================== */

VLIB_PLUGIN_REGISTER () = {
  .version = "1.0.0",
  .version_required = VPP_BUILD_VER,
  .load_after = "wireguard_plugin.so",
  .description = "SR-WG: Segment Routing over WireGuard transit/encap/decap",
};

/* ==================== UDP 端口注册（功能多路复用键） ====================
 *
 * 两个端口都指向本插件的节点：
 *   vpn_port     -> sr-wg4-input（decap）。它对 magic != 0x53 的包直接旁路到
 *                   wg4-input，所以没启用 SR-WG 的普通 WireGuard 隧道仍然正常。
 *   transit_port -> sr-wg4-transit（只验签转发，不解密）。
 *
 * 建 WireGuard 接口时 wireguard 插件会把 51820 重新注册给自己的 wg4-input，
 * 把我们的旁路入口摘掉；因此 WG 建完之后、以及 transit 换端口之后，
 * 都必须重新调用本函数。CLI `set sr-wg rebind-ports` 就是它的手工入口。
 */
void
sr_wg_ports_rebind (sr_wg_main_t *sm)
{
  udp_register_dst_port (sm->vlib_main, (udp_dst_port_t) sm->vpn_port,
                         sr_wg4_input_node.index, /* is_ipv4 */ 1);
  udp_register_dst_port (sm->vlib_main, (udp_dst_port_t) sm->transit_port,
                         sr_wg4_transit_node.index, /* is_ipv4 */ 1);
}

/* ==================== 初始化 ==================== */

static clib_error_t *
sr_wg_init (vlib_main_t *vm)
{
  sr_wg_main_t *sm = &sr_wg_main;
  clib_error_t *error = NULL;

  sm->vlib_main = vm;
  sm->vnet_main = vnet_get_main ();

  /* 路径表初始化 */
  sr_path_table_init (&sm->path_table);

  /* 密钥表清零 */
  clib_memset (sm->keys, 0, sizeof (sm->keys));
  sm->key_count = 0;

  /* 计数器清零 */
  clib_memset (&sm->counters, 0, sizeof (sm->counters));

  /* 默认端口配置 */
  sm->transit_port = SR_PORT_TRANSIT;
  sm->vpn_port = SR_PORT_VPN;
  sm->transit_enabled = 0;

  /*
   * Encap：隧道-路径绑定表（hash + pool）。
   *
   * 注意这里**不能**调用 vnet_feature_enable_disable() 之类的接口：
   * "ip4-output" arc 与 "sr-wg4-encap" feature 的注册由
   * sr_wg4_encap.c 中的 VNET_FEATURE_INIT 完成，排序发生在
   * vnet_feature_arc_init()（早于所有 VLIB_INIT_FUNCTION），
   * feature 的实际启用由 API / CLI 按接口触发。
   */
  sr_wg_encap_init (sm);
  sm->encap_fib_index = 0;   /* 默认在 main FIB 重新查外层第一跳 */

  /* 缓存 node indices */
  sm->transit_node_index = sr_wg4_transit_node.index;
  sm->input_node_index = sr_wg4_input_node.index;
  sm->encap_node_index = sr_wg4_encap_node.index;

  sr_wg_ports_rebind (sm);

  /* API hookup */
  sr_wg_api_hookup (vm);

  vlib_log_notice (sr_wg_log.class,
                   "SR-WG plugin initialized, transit=%u, vpn=%u, "
                   "encap arc=%s node=%s",
                   sm->transit_port, sm->vpn_port,
                   SR_WG_ENCAP_ARC, SR_WG_ENCAP_NODE);

  return error;
}

VLIB_INIT_FUNCTION (sr_wg_init);
