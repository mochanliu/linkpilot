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
 * 端口集合里的每个端口都指向本插件的节点：
 *   vpn_ports     -> sr-wg4-input（decap）。它对 magic != 0x53 的包直接旁路到
 *                    wg4-input，所以没启用 SR-WG 的普通 WireGuard 隧道仍然正常。
 *   transit_ports -> sr-wg4-transit（只验签转发，不解密）。
 *
 * 为什么是集合而不是单个 vpn_port：同一对 CPE 之间可以开多条 WG 隧道，每隧道
 * 绑一个自己的监听端口（定案 2）。只注册一个端口时，第二条隧道的包进不到
 * sr-wg4-input（不解密、不进观测表）—— 表现是"隧道看着通、SR 静默没生效"。
 *
 * 端口集合只决定**收包**去哪个节点；encap 的隧道身份由绑定表里的**本端外层
 * UDP 源端口**决定（WG 发包时用的就是这个端口），与集合内容无关 —— 发往对端
 * 的包目的端口是对端的监听端口，本机集合里没有它，也不需要。
 *
 * 建 WireGuard 接口时 wireguard 插件会把**该接口的** listen-port 重新注册给
 * 自己的 wg4-input，把我们的旁路入口摘掉；因此 WG 建完之后、以及 transit 换
 * 端口之后，都必须重新调用 rebind。CLI `set sr-wg rebind-ports` 是手工入口。
 */
static void
sr_wg_port_register (sr_wg_main_t *sm, u8 kind, u16 port)
{
  udp_register_dst_port (sm->vlib_main, (udp_dst_port_t) port,
                         kind == SR_WG_PORT_TRANSIT ?
                           sr_wg4_transit_node.index : sr_wg4_input_node.index,
                         /* is_ipv4 */ 1);
}

/*
 * 把端口交出去。删的是 vpn 端口时**不能**简单反注册：那个端口本来就是某张
 * WG 接口的 listen-port，反注册之后连 wg4-input 也收不到它，那条隧道连同
 * keepalive 探针一起死掉。所以按 wireguard 建接口时的原样把它注册回 wg4-input
 * （wireguard 插件没加载时——本插件 load_after 它，理论上不可能——才退化为
 * 反注册）。transit 端口不属于 wireguard，直接反注册。
 */
static void
sr_wg_port_release (sr_wg_main_t *sm, u8 kind, u16 port)
{
  if (kind == SR_WG_PORT_VPN)
    {
      vlib_node_t *wg =
        vlib_get_node_by_name (sm->vlib_main, (u8 *) "wg4-input");
      if (wg)
        {
          udp_register_dst_port (sm->vlib_main, (udp_dst_port_t) port,
                                 wg->index, /* is_ipv4 */ 1);
          return;
        }
    }

  udp_unregister_dst_port (sm->vlib_main, (udp_dst_port_t) port,
                           /* is_ipv4 */ 1);
}

static clib_bitmap_t **
sr_wg_port_bitmap (sr_wg_main_t *sm, u8 kind)
{
  return kind == SR_WG_PORT_TRANSIT ? &sm->transit_ports : &sm->vpn_ports;
}

/*
 * 增/删一个 UDP 目的端口，并**立刻**作用到 UDP 分发表。
 * 集合是功能多路复用键的唯一真相：控制器下发、CLI 手敲、对账 dump 都读它，
 * 所以这里不能有"改了集合但忘了 rebind"的中间态。
 */
int
sr_wg_port_add_del (sr_wg_main_t *sm, u8 kind, u16 port, int is_add)
{
  clib_bitmap_t **bp = sr_wg_port_bitmap (sm, kind);
  clib_bitmap_t *set;

  if (port == 0)
    return VNET_API_ERROR_INVALID_VALUE;

  if (is_add)
    {
      /* 已经在集合里也要重注册：控制器"重复下发同一份配置"必须顺手把端口抢回来
       * —— WG 建接口会把同端口抢去 wg4-input，幂等的 add 才是自愈动作。 */
      set = clib_bitmap_set (*bp, port, 1);
      if (set != *bp)
        *bp = set;
      sr_wg_port_register (sm, kind, port);
      return 0;
    }

  if (!clib_bitmap_get (*bp, port))
    return 0;   /* 本就不在集合里：删除是幂等的 */

  set = clib_bitmap_set (*bp, port, 0);
  if (set != *bp)
    *bp = set;
  sr_wg_port_release (sm, kind, port);
  return 0;
}

u32
sr_wg_port_count (sr_wg_main_t *sm, u8 kind)
{
  return (u32) clib_bitmap_count_set_bits (*sr_wg_port_bitmap (sm, kind));
}

void
sr_wg_ports_rebind (sr_wg_main_t *sm)
{
  uword port;

  /* *INDENT-OFF* */
  clib_bitmap_foreach (port, sm->vpn_ports)
    {
      sr_wg_port_register (sm, SR_WG_PORT_VPN, (u16) port);
    }
  clib_bitmap_foreach (port, sm->transit_ports)
    {
      sr_wg_port_register (sm, SR_WG_PORT_TRANSIT, (u16) port);
    }
  /* *INDENT-ON* */
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

  /* 密钥表清零（定位表和条目必须一起清：只清 keys 会让 index 指向幽灵槽） */
  clib_memset (sm->keys, 0, sizeof (sm->keys));
  clib_memset (sm->key_index, 0, sizeof (sm->key_index));
  sm->key_count = 0;

  /* 计数器清零 */
  clib_memset (&sm->counters, 0, sizeof (sm->counters));

  /*
   * 端口集合：一次分配满 64K 位（各 8KB），之后 add/del 只置位、不 realloc。
   * 各带一个种子端口，让"单隧道 + 默认端口"的既有配置与手敲测试不改任何东西
   * 也能继续跑。
   */
  clib_bitmap_alloc (sm->vpn_ports, 65536);
  clib_bitmap_alloc (sm->transit_ports, 65536);
  sm->vpn_ports = clib_bitmap_set (sm->vpn_ports, SR_PORT_VPN, 1);
  sm->transit_ports = clib_bitmap_set (sm->transit_ports, SR_PORT_TRANSIT, 1);
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

  /*
   * 探针：会话表 + UDP 51829 -> sr-wg4-probe。
   * 端口不在 vpn/transit 集合里（那两个是 WireGuard/中转的功能多路键），
   * 探针的内层目的地址已经是隧道 overlay /32，隧道身份由 IP 表达；
   * wireguard 建接口不会碰这个端口，所以也不需要 rebind。
   */
  sr_probe_init (&sm->probe);
  udp_register_dst_port (vm, (udp_dst_port_t) SR_PROBE_PORT,
                         sr_wg4_probe_node.index, /* is_ipv4 */ 1);

  /* 缓存 node indices */
  sm->transit_node_index = sr_wg4_transit_node.index;
  sm->input_node_index = sr_wg4_input_node.index;
  sm->encap_node_index = sr_wg4_encap_node.index;

  sr_wg_ports_rebind (sm);

  /* API hookup */
  sr_wg_api_hookup (vm);

  vlib_log_notice (sr_wg_log.class,
                   "SR-WG plugin initialized, %u vpn port(s) -> sr-wg4-input, "
                   "%u transit port(s) -> sr-wg4-transit, encap arc=%s node=%s",
                   sr_wg_port_count (sm, SR_WG_PORT_VPN),
                   sr_wg_port_count (sm, SR_WG_PORT_TRANSIT),
                   SR_WG_ENCAP_ARC, SR_WG_ENCAP_NODE);

  return error;
}

VLIB_INIT_FUNCTION (sr_wg_init);
