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

  /* 本机地址清零（未配置） */
  clib_memset (&sm->local_ip4, 0, sizeof (sm->local_ip4));

  /* 缓存 node indices */
  sm->transit_node_index = sr_wg4_transit_node.index;
  sm->input_node_index = sr_wg4_input_node.index;
  sm->encap_node_index = sr_wg4_encap_node.index;

  /* 注册 UDP 端口 -> transit node */
  udp_register_dst_port (vm, (udp_dst_port_t) SR_PORT_TRANSIT,
                         sr_wg4_transit_node.index, /* is_ipv4 */ 1);

  /*
   * 注册 UDP 端口 -> input (decap) node。
   * sr-wg4-input 取代了 wg4-input 在 51820 上的注册，但它对
   * 非 SR 包（magic != 0x53）会直接旁路到 wg4-input，
   * 因此未启用 SR-WG 的普通 WireGuard 隧道仍然正常工作。
   */
  udp_register_dst_port (vm, (udp_dst_port_t) SR_PORT_VPN,
                         sr_wg4_input_node.index, /* is_ipv4 */ 1);

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
