/*
 * sr_wg_cli.c - SR-WG Debug CLI
 *
 * 命令列表：
 *   show sr-wg paths
 *   show sr-wg keys
 *   show sr-wg tunnels
 *   show sr-wg ports
 *   show sr-wg stats
 *   show sr-wg observed
 *   clear sr-wg stats
 *   clear sr-wg observed
 *   sr-wg path add <path_id> [via <ip>:<port> ...] [no-relay]
 *                  [hmac-id <n>] [inactive]
 *   sr-wg path del <path_id>
 *   sr-wg key set <hmac_id> <hex32>
 *   sr-wg key del <hmac_id>
 *   set sr-wg port add|del vpn|transit <port>
 *   set sr-wg transit enable port <port>
 *   set sr-wg transit disable
 *   set sr-wg tunnel <sw_if_index> path <path_id> [src-port <port>] [del]
 *   set sr-wg encap enable <underlay sw_if_index|name> | disable <…>
 *                   | fib-index <n>
 *   set sr-wg rebind-ports        （建 WG 接口后必做，见 sr_wg.c）
 *   set sr-wg probe <sw_if_index> peer <ip> [local <ip>]
 *                   [interval <ms>] [timeout <ms>] | del
 *   show sr-wg probe
 *   sr-wg probe send <sw_if_index>
 */
#include "sr_wg.h"
#include <vnet/ip/format.h>
#include <vnet/interface_funcs.h>
#include <vnet/vnet.h>

/* ==================== 共用格式化 ==================== */

/*
 * 把端口集合打印成 "51820,51830"。%U 的实参是 clib_bitmap_t *（== uword *）。
 * 空集合打 "<none>"：agent 对账时"一个端口都没注册"和"读不到表"必须能分辨。
 */
static u8 *
format_sr_wg_port_set (u8 *s, va_list *args)
{
  clib_bitmap_t *bitmap = va_arg (*args, clib_bitmap_t *);
  uword port;
  u8 first = 1;

  /* *INDENT-OFF* */
  clib_bitmap_foreach (port, bitmap)
    {
      s = format (s, "%s%u", first ? "" : ",", (u32) port);
      first = 0;
    }
  /* *INDENT-ON* */

  if (first)
    s = format (s, "<none>");

  return s;
}

/* ==================== show sr-wg paths ==================== */

static clib_error_t *
sr_wg_show_paths_command_fn (vlib_main_t *vm, unformat_input_t *input,
                             vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;
  int i;

  vlib_cli_output (vm, "%d path(s) configured (容量 %d，一条隧道一条路径):",
                   sm->path_table.count, SR_MAX_PATHS);

  for (i = 0; i < sm->path_table.count; i++)
    {
      sr_path_config_t *cfg = &sm->path_table.paths[i];
      int j;

      vlib_cli_output (vm, "  [%d] path_id=%u hmac_id=%u relay_count=%u "
                           "active=%u",
                       i, cfg->path_id, cfg->hmac_id, cfg->relay_count,
                       cfg->active);

      /* 只有中转跳：终点段不在这张表里，encap 从报文外层 dst 现取 */
      for (j = 0; j < cfg->relay_count; j++)
        {
          ip4_address_t ip;
          u16 port;
          clib_memcpy_fast (&ip, cfg->relays[j].ip, 4);
          clib_memcpy_fast (&port, cfg->relays[j].port, 2);
          port = clib_net_to_host_u16 (port);
          vlib_cli_output (vm, "      relay[%d]%s: %U:%u", j,
                           j == cfg->relay_count - 1 ? " (第一跳)" : "",
                           format_ip4_address, &ip, port);
        }
    }

  return 0;
}

VLIB_CLI_COMMAND (sr_wg_show_paths_command, static) = {
  .path = "show sr-wg paths",
  .short_help = "show sr-wg paths",
  .function = sr_wg_show_paths_command_fn,
};

/* ==================== show sr-wg keys ==================== */

static clib_error_t *
sr_wg_show_keys_command_fn (vlib_main_t *vm, unformat_input_t *input,
                            vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;
  u32 i;

  vlib_cli_output (vm, "%u key(s) configured (容量 %u，一把钥匙一个签名身份):",
                   sm->key_count, (u32) SR_WG_MAX_KEYS);

  for (i = 0; i < sm->key_count; i++)
    {
      vlib_cli_output (vm, "  [%u] hmac_id=%u active=%u", i,
                       sm->keys[i].hmac_id, sm->keys[i].active);
    }

  return 0;
}

VLIB_CLI_COMMAND (sr_wg_show_keys_command, static) = {
  .path = "show sr-wg keys",
  .short_help = "show sr-wg keys",
  .function = sr_wg_show_keys_command_fn,
};

/* ==================== show sr-wg tunnels ==================== */

static clib_error_t *
sr_wg_show_tunnels_command_fn (vlib_main_t *vm, unformat_input_t *input,
                               vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;
  vnet_main_t *vnm = vnet_get_main ();
  u32 bi, n = 0, n_usable = 0;

  /* 下面既要打印挂点表、又要用 encap_enabled_count 判 WARNING，先把表对回 VPP 的登记 */
  sr_wg_encap_reconcile (sm);

  /*
   * 挂点在 underlay 出口接口（不是 wg0），所以绑定能不能生效取决于两件事：
   *   (1) 至少一个 underlay 接口启用了 sr-wg4-encap feature
   *   (2) 该绑定带本端源端口 —— 数据面按外层 UDP 源端口选路（每隧道一个监听
   *       端口，定案 2），src_port=0 的绑定查不到
   * 任一条不满足，流量都会静默旁路直发 peer，破坏源路由语义，必须显式提示。
   */
  vlib_cli_output (vm, "encap fib-index=%u, encap enabled on %u interface(s):",
                   sm->encap_fib_index, sm->encap_enabled_count);

  if (sm->encap_enabled_count == 0)
    vlib_cli_output (
      vm, "  !! WARNING: sr-wg4-encap 未启用在任何接口上，"
          "所有 SR 路径都不会生效。"
          "请执行 `set sr-wg encap enable <underlay sw_if_index>`");
  else
    {
      uword key, val;
      /* *INDENT-OFF* */
      hash_foreach (key, val, sm->encap_enabled_ifs, ({
        vlib_cli_output (vm, "  %U (sw_if_index=%u)",
                         format_vnet_sw_if_index_name, vnm, (u32) key,
                         (u32) key);
      }));
      /* *INDENT-ON* */
    }

  for (bi = 0; sm->binds && bi < vec_len (sm->binds); bi++)
    {
      sr_wg_tunnel_bind_t *bind;
      const char *state;

      if (pool_is_free_index (sm->binds, bi))
        continue;

      bind = pool_elt_at_index (sm->binds, bi);

      if (bind->src_port)
        {
          state = "usable";
          n_usable++;
        }
      else
        state = "!! NO SRC PORT, never matches";

      /*
       * 26.06 的 vnet_sw_interface_t 已无 name 字段，必须用格式化函数取名。
       *
       * 注意两个同名系函数的 va_arg 约定在 26.06 中**不同**（已用 objdump
       * 核对 libvnet.so 实测确认）：
       *   format_vnet_sw_interface_name  (vnet_main_t *, vnet_sw_interface_t *)
       *   format_vnet_sw_if_index_name   (vnet_main_t *, u32 sw_if_index)
       * 传错会直接 SIGSEGV，且前者不做越界检查，故这里用后者。
       */
      if (vnet_get_sw_interface_or_null (vnm, bind->sw_if_index))
        vlib_cli_output (vm,
                         "  [%u] sw_if_index=%u (%U) path_id=%u "
                         "src_port=%u %s",
                         bi, bind->sw_if_index, format_vnet_sw_if_index_name,
                         vnm, bind->sw_if_index, (u32) bind->path_id,
                         (u32) bind->src_port, state);
      else
        vlib_cli_output (vm,
                         "  [%u] sw_if_index=%u (<deleted>) path_id=%u "
                         "src_port=%u %s",
                         bi, bind->sw_if_index, (u32) bind->path_id,
                         (u32) bind->src_port, state);
      n++;
    }

  vlib_cli_output (vm, "%u tunnel binding(s) configured, %u usable.", n,
                   n_usable);
  return 0;
}

VLIB_CLI_COMMAND (sr_wg_show_tunnels_command, static) = {
  .path = "show sr-wg tunnels",
  .short_help = "show sr-wg tunnels",
  .function = sr_wg_show_tunnels_command_fn,
};

/* ==================== show sr-wg ports ==================== */

static clib_error_t *
sr_wg_show_ports_command_fn (vlib_main_t *vm, unformat_input_t *input,
                             vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;

  vlib_cli_output (vm, "vpn     %2u 个端口 -> sr-wg4-input : %U",
                   sr_wg_port_count (sm, SR_WG_PORT_VPN),
                   format_sr_wg_port_set, sm->vpn_ports);
  vlib_cli_output (vm, "transit %2u 个端口 -> sr-wg4-transit: %U (%s)",
                   sr_wg_port_count (sm, SR_WG_PORT_TRANSIT),
                   format_sr_wg_port_set, sm->transit_ports,
                   sm->transit_enabled ? "已启用" : "未启用，收到也丢");

  if (sr_wg_port_count (sm, SR_WG_PORT_VPN) == 0)
    vlib_cli_output (
      vm, "  !! WARNING: vpn 端口集合是空的，隧道收包会落到 wg4-input 而不是 "
          "sr-wg4-input（SR 头不会被解掉）。用 `set sr-wg port add vpn <port>` 补。");

  return 0;
}

VLIB_CLI_COMMAND (sr_wg_show_ports_command, static) = {
  .path = "show sr-wg ports",
  .short_help = "show sr-wg ports",
  .function = sr_wg_show_ports_command_fn,
};

/* ==================== show sr-wg stats ==================== */

static clib_error_t *
sr_wg_show_stats_command_fn (vlib_main_t *vm, unformat_input_t *input,
                             vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;
  sr_counters_t *c = &sm->counters;

  vlib_cli_output (vm, "SR-WG Statistics:");
  vlib_cli_output (vm, "  Encap:");
  vlib_cli_output (vm, "    ok:              %llu", c->encap_ok);
  vlib_cli_output (vm, "    no_path:         %llu", c->encap_no_path);
  vlib_cli_output (vm, "    hmac_fail:       %llu", c->encap_hmac_fail);
  vlib_cli_output (vm, "    bypass:          %llu", c->encap_bypass);
  vlib_cli_output (vm, "    no_headroom:     %llu", c->encap_no_headroom);
  vlib_cli_output (vm, "  Transit:");
  vlib_cli_output (vm, "    ok:              %llu", c->transit_ok);
  vlib_cli_output (vm, "    hmac_fail:       %llu", c->transit_hmac_fail);
  vlib_cli_output (vm, "    seg_exhausted:   %llu", c->transit_seg_exhausted);
  vlib_cli_output (vm, "    bad_magic:       %llu", c->transit_bad_magic);
  vlib_cli_output (vm, "    bad_ver:         %llu", c->transit_bad_ver);
  vlib_cli_output (vm, "    bad_len:         %llu", c->transit_bad_len);
  vlib_cli_output (vm, "  Decap:");
  vlib_cli_output (vm, "    ok:              %llu", c->decap_ok);
  vlib_cli_output (vm, "    hmac_fail:       %llu", c->decap_hmac_fail);
  vlib_cli_output (vm, "    seg_not_zero:    %llu", c->decap_seg_not_zero);
  vlib_cli_output (vm, "    bypass:          %llu", c->decap_bypass);
  vlib_cli_output (vm, "  General:");
  vlib_cli_output (vm, "    drop_total:      %llu", c->drop_total);

  return 0;
}

VLIB_CLI_COMMAND (sr_wg_show_stats_command, static) = {
  .path = "show sr-wg stats",
  .short_help = "show sr-wg stats",
  .function = sr_wg_show_stats_command_fn,
};

/* ==================== clear sr-wg stats ==================== */

static clib_error_t *
sr_wg_clear_stats_command_fn (vlib_main_t *vm, unformat_input_t *input,
                              vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;

  clib_memset (&sm->counters, 0, sizeof (sm->counters));
  vlib_cli_output (vm, "SR-WG statistics cleared.");

  return 0;
}

VLIB_CLI_COMMAND (sr_wg_clear_stats_command, static) = {
  .path = "clear sr-wg stats",
  .short_help = "clear sr-wg stats",
  .function = sr_wg_clear_stats_command_fn,
};

/* ==================== show / clear sr-wg observed ==================== */

/*
 * vBOND 地址发现的现场证据：本节点看到的每个外层源元组。
 *
 * 时间只报"多少秒前最后一次看到"，不报绝对时间戳 —— 插件的时钟是相对 VPP
 * 启动的秒数，把它打印成日期会误导排障（要绝对时间请让 agent 打自己的墙钟）。
 *
 * verified 一列是本命令的关键：0 表示这条观测来自旁路的普通 WG 包，外层源
 * 无法在本节点认证（观测点在 WireGuard 解密之前），只能当候选映射；1 表示
 * 至少有一次观测来自 SR HMAC 校验通过的包。看到 evictions 在涨就说明有人在
 * 往这个端口灌伪造源。
 */
static clib_error_t *
sr_wg_show_observed_command_fn (vlib_main_t *vm, unformat_input_t *input,
                               vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;
  vnet_main_t *vnm = vnet_get_main ();
  sr_obs_entry_t e;
  u32 cursor = 0, n = 0, n_verified = 0;
  const u64 now_sec = (u64) vlib_time_now (vm);

  while (sr_obs_next (&sm->obs, &cursor, &e))
    {
      ip4_address_t src;
      u64 age = e.last_seen <= now_sec ? now_sec - e.last_seen : 0;

      src.as_u32 = clib_host_to_net_u32 (e.src_ip);
      n++;
      n_verified += e.verified ? 1 : 0;

      vlib_cli_output (vm, "  %U:%u -> :%u on %U age=%llu s hits=%u verified=%u",
                       format_ip4_address, &src, (u32) e.src_port,
                       (u32) e.dst_port, format_vnet_sw_if_index_name, vnm,
                       (u32) e.rx_if, age, e.hits, e.verified);
    }

  vlib_cli_output (vm,
                   "%u observed tuple(s), %u verified / %u unverified. "
                   "records=%llu evictions=%llu",
                   n, n_verified, n - n_verified, sm->obs.records,
                   sm->obs.evictions);

  if (n == 0)
    vlib_cli_output (
      vm, "  (空表。确认隧道端口在 vpn 集合 %U 里、且已注册到 sr-wg4-input："
          "`set sr-wg port add vpn <port>` 与建 WG 接口之后的 "
          "`set sr-wg rebind-ports`)",
      format_sr_wg_port_set, sm->vpn_ports);

  return 0;
}

VLIB_CLI_COMMAND (sr_wg_show_observed_command, static) = {
  .path = "show sr-wg observed",
  .short_help = "show sr-wg observed",
  .function = sr_wg_show_observed_command_fn,
};

static clib_error_t *
sr_wg_clear_observed_command_fn (vlib_main_t *vm, unformat_input_t *input,
                                vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;

  sr_obs_clear (&sm->obs);
  vlib_cli_output (vm, "SR-WG observation table cleared.");

  return 0;
}

VLIB_CLI_COMMAND (sr_wg_clear_observed_command, static) = {
  .path = "clear sr-wg observed",
  .short_help = "clear sr-wg observed",
  .function = sr_wg_clear_observed_command_fn,
};

/* ==================== sr-wg path ==================== */

/*
 * 路径表 CLI。
 *
 * 路径 / P-Key 原本只能通过 Binary API 下发，现场排障与 e2e 测试都需要
 * 一条纯 CLI 通道，这里补上（与 vl_api_sr_wg_path_add_del_t_handler /
 * vl_api_sr_wg_key_set_t_handler 语义完全一致）。
 *
 *   sr-wg path add <path_id> [via <ip>:<port> [via <ip>:<port> ...]]
 *                  [hmac-id <n>] [inactive]
 *   sr-wg path del <path_id>
 *
 * `via` 只写**中转跳**，从离终点最近的那台写到第一跳；终点不写 —— 它是
 * 报文自己的外层 dst，encap 发包时现取（所以 peer 的地址/端口怎么变都不用
 * 重建路径，也不用知道）。一条 via 都不给 = 没有中转跳，仍然合法：那种隧道
 * 照样带 SR 头，用来让对端把外层源记成 verified 观测。
 *
 * 填进线格式后的段序（sr_header.h 约定）：
 *   segments[0]           = 报文外层 dst（最终目的 = 对端 CPE）
 *   segments[seg_count-1] = 第一条 via = 第一跳（transit，port 51821）
 * encap 取它作为改写后的外层 dst（"首跳免费"），并把 seg_left 置为
 * relay_count；此后每经过一个 transit 执行一次 sr_hdr_pop_segment()
 * （next = segments[seg_left-1]，然后 seg_left--）。
 */
static clib_error_t *
sr_wg_path_command_fn (vlib_main_t *vm, unformat_input_t *input,
                       vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;
  sr_path_config_t cfg;
  ip4_address_t ip;
  u32 path_id = 0, hmac_id = 0, port = 0;
  u8 add = 0, del = 0, inactive = 0, no_relay = 0;
  int rv;

  clib_memset (&cfg, 0, sizeof (cfg));
  clib_memset (&ip, 0, sizeof (ip));

  while (unformat_check_input (input) != UNFORMAT_END_OF_INPUT)
    {
      if (unformat (input, "add %u", &path_id))
        add = 1;
      else if (unformat (input, "del %u", &path_id))
        del = 1;
      else if (unformat (input, "delete %u", &path_id))
        del = 1;
      else if (unformat (input, "hmac-id %u", &hmac_id))
        ;
      else if (unformat (input, "inactive"))
        inactive = 1;
      else if (unformat (input, "no-relay"))
        no_relay = 1;
      else if (unformat (input, "via %U:%u", unformat_ip4_address, &ip, &port))
        {
          u16 port_net;

          if (cfg.relay_count >= SR_MAX_RELAYS)
            return clib_error_return (0, "too many relay hops (max %d)",
                                      SR_MAX_RELAYS);
          if (port == 0 || port > 65535)
            return clib_error_return (0, "bad port %u in relay hop %u", port,
                                      cfg.relay_count);
          clib_memcpy_fast (cfg.relays[cfg.relay_count].ip, &ip, 4);
          port_net = clib_host_to_net_u16 ((u16) port);
          clib_memcpy_fast (cfg.relays[cfg.relay_count].port, &port_net, 2);
          cfg.relay_count++;
        }
      else
        break;
    }

  if (del)
    {
      u32 n_binds;

      rv = sr_path_del (&sm->path_table, (u16) path_id);
      if (rv != 0)
        return clib_error_return (0, "path %u not found", path_id);

      /* 指向它的隧道绑定一起摘掉：留着就是收包时按 encap_no_path 静默丢包 */
      n_binds = sr_wg_binds_drop_for_path (sm, (u16) path_id);
      vlib_cli_output (vm, "SR-WG path %u deleted (%u tunnel bind(s) dropped).",
                       path_id, n_binds);
      return 0;
    }

  if (!add)
    return clib_error_return (0, "specify 'add <path_id>' or 'del <path_id>'");
  /*
   * 没有 via 时不许默默建一条零中转跳的路径：`via` 拼错会被 unformat 静默
   * 停在 else break，后果是"路径建成功了、流量直发对端"，正是本改动要消灭的
   * 那类故障。确实要零中转跳（只为了让包带 SR 头）请显式写 `no-relay`。
   */
  if (cfg.relay_count == 0 && !no_relay)
    return clib_error_return (
      0, "no 'via <ip>:<port>' parsed; 直连隧道不需要 SR 路径。"
         "若确实要建零中转跳的路径，加 `no-relay`");

  cfg.path_id = (u16) path_id;
  cfg.hmac_id = (u16) hmac_id;
  cfg.active = inactive ? 0 : 1;

  /* 与 API handler 一致：从密钥表取 P-Key 填入路径配置 */
  {
    sr_wg_key_entry_t *key = sr_wg_key_lookup (sm, cfg.hmac_id);
    if (!key)
      return clib_error_return (
        0, "no active key for hmac_id=%u; run `sr-wg key set %u <hex>` first "
           "(建一条全零密钥的路径 = 包发得出去但对端一律验签失败)",
        cfg.hmac_id, cfg.hmac_id);
    clib_memcpy_fast (cfg.pkey, key->key, SR_PKEY_LEN);
  }

  rv = sr_path_add (&sm->path_table, &cfg);
  if (rv == SR_PATH_ERR_FULL)
    return clib_error_return (
      0, "路径表已满（%d 条）—— 一台盒子的隧道数到顶了；"
         "先 `sr-wg path del` 腾槽位，或扩 SR_MAX_PATHS",
      SR_MAX_PATHS);
  if (rv != 0)
    return clib_error_return (0, "sr_path_add failed (rv=%d)", rv);

  vlib_cli_output (vm, "SR-WG path %u set: hmac_id=%u relay_count=%u active=%u",
                   path_id, cfg.hmac_id, cfg.relay_count, cfg.active);
  return 0;
}

VLIB_CLI_COMMAND (sr_wg_path_command, static) = {
  .path = "sr-wg path",
  .short_help = "sr-wg path add <path_id> [via <ip>:<port> ...] "
                "(中转跳，离终点近的写前面；终点不写) [no-relay] "
                "[hmac-id <n>] [inactive] | sr-wg path del <path_id>",
  .function = sr_wg_path_command_fn,
};

/* ==================== sr-wg key ==================== */

/*
 * P-Key CLI。SipHash-2-4 的密钥固定 128-bit，因此只接受恰好 32 位十六进制。
 * 不做长度补齐：SHA 时代的"短密钥左侧补零"会静默把用户以为的密钥强度
 * 缩到实际熵，且让同一条配置在换原语后含义改变。
 */
static int
sr_wg_parse_hex_key (const u8 *s, uword len, u8 *out)
{
  uword i;

  if (len != 2 * SR_PKEY_LEN)
    return -1;

  clib_memset (out, 0, SR_PKEY_LEN);

  for (i = 0; i < len; i++)
    {
      u8 c = s[i], v;

      if (c >= '0' && c <= '9')
        v = (u8) (c - '0');
      else if (c >= 'a' && c <= 'f')
        v = (u8) (c - 'a' + 10);
      else if (c >= 'A' && c <= 'F')
        v = (u8) (c - 'A' + 10);
      else
        return -1;

      out[i / 2] |= (u8) (v << ((i & 1) ? 0 : 4));
    }
  return 0;
}

static clib_error_t *
sr_wg_key_command_fn (vlib_main_t *vm, unformat_input_t *input,
                      vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;
  u8 *key_hex = 0;
  u8 key[SR_PKEY_LEN];
  u32 hmac_id = 0;
  u8 set = 0, del = 0;
  int rv;

  while (unformat_check_input (input) != UNFORMAT_END_OF_INPUT)
    {
      if (unformat (input, "set %u %v", &hmac_id, &key_hex))
        set = 1;
      else if (unformat (input, "del %u", &hmac_id))
        del = 1;
      else if (unformat (input, "delete %u", &hmac_id))
        del = 1;
      else
        break;
    }

  if (del)
    {
      int n_paths = sr_path_count_using_hmac_id (&sm->path_table,
                                                 (u16) hmac_id);
      if (n_paths > 0)
        return clib_error_return (
          0, "hmac_id=%u still used by %d path(s); 换密钥请用 "
             "`sr-wg key set %u <hex>`（会自动刷新路径里的密钥副本）",
          hmac_id, n_paths, hmac_id);

      rv = sr_wg_key_del (sm, (u16) hmac_id);
      if (rv != 0)
        return clib_error_return (0, "key hmac_id=%u not found", hmac_id);
      vlib_cli_output (vm, "SR-WG key %u deleted.", hmac_id);
      return 0;
    }

  if (!set || !key_hex)
    return clib_error_return (
      0, "specify 'set <hmac_id> <hex>' or 'del <hmac_id>'");

  if (sr_wg_parse_hex_key (key_hex, vec_len (key_hex), key) != 0)
    {
      vec_free (key_hex);
      return clib_error_return (0,
                                "bad key: expect exactly %d hex chars "
                                "(128-bit SipHash key)",
                                2 * SR_PKEY_LEN);
    }
  vec_free (key_hex);

  rv = sr_wg_key_add (sm, (u16) hmac_id, key);
  if (rv == SR_WG_KEY_ERR_FULL)
    return clib_error_return (
      0, "密钥表已满（%d 把）。hmac_id=%u 是新身份，装不下；"
         "`show sr-wg keys` 看现状，先 `sr-wg key del` 腾位，或扩 SR_WG_MAX_KEYS",
      SR_WG_MAX_KEYS, hmac_id);
  if (rv != SR_WG_KEY_OK)
    return clib_error_return (0, "sr_wg_key_add failed (rv=%d)", rv);

  vlib_cli_output (vm, "SR-WG key hmac_id=%u set.", hmac_id);
  return 0;
}

VLIB_CLI_COMMAND (sr_wg_key_command, static) = {
  .path = "sr-wg key",
  .short_help = "sr-wg key set <hmac_id> <hex32> | sr-wg key del <hmac_id>",
  .function = sr_wg_key_command_fn,
};

/* ==================== set sr-wg port ==================== */

/*
 *   set sr-wg port add vpn <port>
 *   set sr-wg port del vpn <port>
 *   set sr-wg port add transit <port>
 *
 * 每隧道一个监听端口（定案 2）：同一对 CPE 可以开多条 WG 隧道，端口就是数据面
 * 认隧道的身份。add/del **立刻**作用到 UDP 分发表，不需要另外 rebind-ports；
 * rebind 是"WG 建接口把端口抢回 wg4-input 之后"的修复动作。
 *
 * 删 vpn 端口会把端口交回 wg4-input（那条隧道还能当普通 WG 用），
 * 删 transit 端口直接反注册。
 */
static clib_error_t *
sr_wg_set_port_command_fn (vlib_main_t *vm, unformat_input_t *input,
                           vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;
  u8 is_add = 0, is_del = 0, have_kind = 0, is_vpn = 0;
  u32 port = 0;
  u8 kind;
  int rv;

  while (unformat_check_input (input) != UNFORMAT_END_OF_INPUT)
    {
      if (unformat (input, "add"))
        is_add = 1;
      else if (unformat (input, "del"))
        is_del = 1;
      else if (unformat (input, "vpn"))
        {
          have_kind = 1;
          is_vpn = 1;
        }
      else if (unformat (input, "transit"))
        {
          have_kind = 1;
          is_vpn = 0;
        }
      else if (unformat (input, "%u", &port))
        ;
      else
        break;
    }

  if (is_add == is_del)
    return clib_error_return (0, "specify 'add' or 'del'");
  if (!have_kind)
    return clib_error_return (0, "specify 'vpn' or 'transit'");
  if (port == 0)
    return clib_error_return (0, "<port> must be 1..65535");

  kind = is_vpn ? SR_WG_PORT_VPN : SR_WG_PORT_TRANSIT;
  rv = sr_wg_port_add_del (sm, kind, (u16) port, is_add ? 1 : 0);
  if (rv != 0)
    return clib_error_return (0, "sr_wg_port_add_del failed (rv=%d)", rv);

  vlib_cli_output (vm,
                   "SR-WG %s port %u %s；集合现有 %u 个端口: %U",
                   is_vpn ? "vpn" : "transit", port,
                   is_add ? "已加入并重挂" : "已移出并交回",
                   sr_wg_port_count (sm, kind), format_sr_wg_port_set,
                   is_vpn ? sm->vpn_ports : sm->transit_ports);
  return 0;
}

VLIB_CLI_COMMAND (sr_wg_set_port_command, static) = {
  .path = "set sr-wg port",
  .short_help = "set sr-wg port add|del vpn|transit <port>",
  .function = sr_wg_set_port_command_fn,
};

/* ==================== set sr-wg transit ==================== */

static clib_error_t *
sr_wg_set_transit_command_fn (vlib_main_t *vm, unformat_input_t *input,
                              vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;
  u8 enable = 0;
  u8 disable = 0;
  u32 port = 0;

  while (unformat_check_input (input) != UNFORMAT_END_OF_INPUT)
    {
      if (unformat (input, "enable"))
        enable = 1;
      else if (unformat (input, "disable"))
        disable = 1;
      else if (unformat (input, "port %u", &port))
        ;
      else
        break;
    }

  if (enable && !disable)
    {
      int rv;

      if (port == 0)
        port = SR_PORT_TRANSIT;

      /* 端口是集合：一台中转可以服务多个租户/多条通道，各占一个 transit 端口。
       * sr_wg_port_add_del 内部已经重注册，这里只再置启用标志。 */
      rv = sr_wg_port_add_del (sm, SR_WG_PORT_TRANSIT, (u16) port, 1);
      if (rv != 0)
        return clib_error_return (0, "sr_wg_port_add_del failed (rv=%d)", rv);
      sm->transit_enabled = 1;

      vlib_cli_output (vm,
                       "SR-WG transit enabled: port=%u（集合 %U）"
                       "，外层源 IP/端口不改写，中转不参与 WireGuard",
                       port, format_sr_wg_port_set, sm->transit_ports);
    }
  else if (disable)
    {
      sm->transit_enabled = 0;
      vlib_cli_output (vm, "SR-WG transit disabled.");
    }
  else
    {
      return clib_error_return (0, "specify 'enable' or 'disable'");
    }

  return 0;
}

VLIB_CLI_COMMAND (sr_wg_set_transit_command, static) = {
  .path = "set sr-wg transit",
  .short_help = "set sr-wg transit enable port <port> | "
                "set sr-wg transit disable",
  .function = sr_wg_set_transit_command_fn,
};

/* ==================== set sr-wg tunnel ==================== */

/*
 * 绑定 / 解绑 WG 隧道接口到 SR 路径。
 *
 *   set sr-wg tunnel <sw_if_index> path <path_id> [src-port <port>] [del]
 *
 * 本命令**只写绑定表**，不启用 feature。encap 的挂点在 underlay 出口接口
 * （wg4-output-tun 不参与 feature 链，详见 sr_wg.h），必须另外执行
 * `set sr-wg encap enable <underlay sw_if_index>`。
 *
 * 数据面按**本端外层 UDP 源端口**查绑定（每隧道一个监听端口，定案 2），所以
 * add 时 [src-port] 实际必填 —— 不给就是"配了但永不命中"，这里接受配置并立刻
 * 告警，避免要靠抓包才发现。它是这条隧道自己的端口，取自建接口时的 listen-port。
 */
static clib_error_t *
sr_wg_set_tunnel_command_fn (vlib_main_t *vm, unformat_input_t *input,
                             vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;
  u32 sw_if_index = (u32) ~0;
  u32 path_id = 0;
  u32 src_port = 0;
  u8 have_path = 0, is_del = 0, have_src_port = 0;
  int rv;

  while (unformat_check_input (input) != UNFORMAT_END_OF_INPUT)
    {
      if (unformat (input, "%u", &sw_if_index))
        ;
      else if (unformat (input, "path %u", &path_id))
        have_path = 1;
      else if (unformat (input, "src-port %u", &src_port))
        have_src_port = 1;
      else if (unformat (input, "del"))
        is_del = 1;
      else if (unformat (input, "delete"))
        is_del = 1;
      else
        break;
    }

  if (sw_if_index == (u32) ~0)
    return clib_error_return (0, "missing <sw_if_index>");
  if (!is_del && !have_path)
    return clib_error_return (0, "missing 'path <path_id>' (or use 'del')");
  if (!is_del && have_src_port && (src_port == 0 || src_port > 65535))
    return clib_error_return (0, "bad src-port %u", src_port);

  rv = sr_wg_tunnel_set_path (sm, sw_if_index, (u16) path_id,
                              (u16) src_port, is_del ? 0 : 1);
  if (rv != 0)
    return clib_error_return (0, "sr-wg tunnel set path failed (rv=%d)", rv);

  if (is_del)
    {
      vlib_cli_output (vm, "SR-WG tunnel %u unbound.", sw_if_index);
      return 0;
    }

  if (have_src_port)
    vlib_cli_output (vm,
                     "SR-WG tunnel %u bound to path_id=%u src_port=%u",
                     sw_if_index, (u32) (u16) path_id, src_port);
  else
    vlib_cli_output (vm,
                     "SR-WG tunnel %u bound to path_id=%u, "
                     "!! WARNING: 未指定 src-port，本绑定永远不会命中，"
                     "流量将直发 peer。",
                     sw_if_index, (u32) (u16) path_id);

  sr_wg_encap_reconcile (sm);
  if (sm->encap_enabled_count == 0)
    vlib_cli_output (vm,
                     "!! WARNING: sr-wg4-encap 尚未在任何接口上启用，"
                     "请执行 `set sr-wg encap enable <underlay sw_if_index>`。");

  return 0;
}

VLIB_CLI_COMMAND (sr_wg_set_tunnel_command, static) = {
  .path = "set sr-wg tunnel",
  .short_help = "set sr-wg tunnel <sw_if_index> path <path_id> "
                "[src-port <port>] [del]",
  .function = sr_wg_set_tunnel_command_fn,
};

/* ==================== set sr-wg encap ==================== */

/*
 * 单独控制 ip4-output arc 上的 sr-wg4-encap feature，或调整外层重查表 FIB。
 *
 *   set sr-wg encap enable <sw_if_index>
 *   set sr-wg encap disable <sw_if_index>
 *   set sr-wg encap fib-index <n>
 *
 * 也接受 "set sr-wg encap <sw_if_index> enable|disable" 的词序。
 *
 * <sw_if_index> 必须是 **underlay 出口接口**（`show interface` 里物理/宿主
 * 网卡那一项，如 host-eth0），不是 wg0：wg4-output-tun 不进入 feature 链，
 * 挂在 wg0 上收不到任何包。
 */
static clib_error_t *
sr_wg_set_encap_command_fn (vlib_main_t *vm, unformat_input_t *input,
                            vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;
  vnet_main_t *vnm = vnet_get_main ();
  u32 sw_if_index = (u32) ~0;
  u32 fib_index = (u32) ~0;
  u8 enable = 0, disable = 0, have_fib = 0;
  int rv;

  while (unformat_check_input (input) != UNFORMAT_END_OF_INPUT)
    {
      if (unformat (input, "enable"))
        enable = 1;
      else if (unformat (input, "disable"))
        disable = 1;
      else if (unformat (input, "fib-index %u", &fib_index))
        have_fib = 1;
      else if (unformat (input, "%u", &sw_if_index))
        ;
      else if (unformat (input, "%U", unformat_vnet_sw_interface, vnm,
                         &sw_if_index))
        ;
      else
        break;
    }

  if (have_fib)
    {
      sm->encap_fib_index = fib_index;
      vlib_cli_output (vm, "SR-WG encap fib-index set to %u.", fib_index);
      /* fib-index 可以单独设置，不伴随 enable/disable */
      if (!enable && !disable)
        return 0;
    }

  if (enable == disable)
    return clib_error_return (0, "specify 'enable' or 'disable'");
  if (sw_if_index == (u32) ~0)
    return clib_error_return (0, "missing <sw_if_index>");

  rv = sr_wg_encap_enable_disable (sm, sw_if_index, enable ? 1 : 0);
  if (rv != 0)
    return clib_error_return (0,
                              "failed to %s '%s' on '%s' for sw_if_index %u "
                              "(rv=%d)",
                              enable ? "enable" : "disable", SR_WG_ENCAP_NODE,
                              SR_WG_ENCAP_ARC, sw_if_index, rv);

  vlib_cli_output (vm, "%s %s on %U (sw_if_index=%u, arc %s).",
                   enable ? "Enabled" : "Disabled", SR_WG_ENCAP_NODE,
                   format_vnet_sw_if_index_name, vnm, sw_if_index, sw_if_index,
                   SR_WG_ENCAP_ARC);
  return 0;
}

VLIB_CLI_COMMAND (sr_wg_set_encap_command, static) = {
  .path = "set sr-wg encap",
  .short_help = "set sr-wg encap enable <sw_if_index> | "
                "set sr-wg encap disable <sw_if_index> | "
                "set sr-wg encap fib-index <n>",
  .function = sr_wg_set_encap_command_fn,
};

/* ==================== set sr-wg rebind-ports ==================== */

/*
 * WireGuard 在创建接口时会把**该接口的** listen-port 重新注册给 wg4-input
 * （udp_register_dst_port），覆盖 SR-WG 之前挂上去的注册 —— 每隧道一个端口
 * 之后，被覆盖的是那一条隧道的端口，不是全局 51820。所以每张 WG 接口建完
 * 都要 rebind 一次，把端口集合里所有端口重新挂回本插件的节点。
 */
static clib_error_t *
sr_wg_rebind_ports_command_fn (vlib_main_t *vm, unformat_input_t *input,
                               vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;

  sr_wg_ports_rebind (sm);

  vlib_cli_output (vm,
                   "SR-WG ports rebound: vpn %U -> sr-wg4-input, "
                   "transit %U -> sr-wg4-transit",
                   format_sr_wg_port_set, sm->vpn_ports,
                   format_sr_wg_port_set, sm->transit_ports);
  return 0;
}

VLIB_CLI_COMMAND (sr_wg_rebind_ports_command, static) = {
  .path = "set sr-wg rebind-ports",
  .short_help = "set sr-wg rebind-ports",
  .function = sr_wg_rebind_ports_command_fn,
};

/* ==================== 隧道内探针（延迟/抖动/丢包） ==================== */

/*
 * 一条会话 = (本地隧道接口, 对端 overlay /32)。
 *
 *   set sr-wg probe <sw_if_index|name> peer <ip> [local <ip>]
 *                   [interval <ms>] [timeout <ms>]
 *   set sr-wg probe <sw_if_index|name> del
 *
 * 不给 local 时取该接口的第一个 IPv4 地址；interval 缺省 300ms，
 * timeout 缺省 3×interval。探针从本机走**正常 FIB**（路由指向 wgX），
 * 所以它能量到 SR 封装/中转/解封装整条路径，而不是裸 underlay 回声。
 *
 * 可达性仍由 VPP core BFD 负责；这里只有质量指标（定案 2026-10-24）。
 */
static clib_error_t *
sr_wg_set_probe_command_fn (vlib_main_t *vm, unformat_input_t *input,
                            vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;
  vnet_main_t *vnm = vnet_get_main ();
  ip4_address_t peer, local;
  u32 sw_if_index = (u32) ~0;
  u32 interval_ms = 0, timeout_ms = 0;
  u8 have_peer = 0, is_del = 0;
  int rv;

  clib_memset (&peer, 0, sizeof (peer));
  clib_memset (&local, 0, sizeof (local));

  while (unformat_check_input (input) != UNFORMAT_END_OF_INPUT)
    {
      if (unformat (input, "peer %U", unformat_ip4_address, &peer))
        have_peer = 1;
      else if (unformat (input, "local %U", unformat_ip4_address, &local))
        ;
      else if (unformat (input, "interval %u", &interval_ms))
        ;
      else if (unformat (input, "timeout %u", &timeout_ms))
        ;
      else if (unformat (input, "del") || unformat (input, "disable"))
        is_del = 1;
      else if (unformat (input, "%U", unformat_vnet_sw_interface, vnm,
                         &sw_if_index))
        ;
      else if (unformat (input, "%u", &sw_if_index))
        ;
      else
        break;
    }

  if (sw_if_index == (u32) ~0)
    return clib_error_return (0, "missing <sw_if_index>");
  if (!is_del && !have_peer)
    return clib_error_return (0, "missing 'peer <ip>' (or use 'del')");

  rv = sr_probe_set (&sm->probe, sw_if_index,
                     clib_net_to_host_u32 (peer.as_u32),
                     clib_net_to_host_u32 (local.as_u32),
                     interval_ms, timeout_ms, is_del ? 0 : 1);

  if (rv == VNET_API_ERROR_INVALID_SW_IF_INDEX)
    return clib_error_return (0, "interface %u does not exist", sw_if_index);
  if (rv == VNET_API_ERROR_ADDRESS_NOT_FOUND_FOR_INTERFACE)
    return clib_error_return (0,
                              "interface %u has no IPv4 address; "
                              "specify 'local <ip>'", sw_if_index);
  if (rv == VNET_API_ERROR_INVALID_VALUE)
    return clib_error_return (0,
                              "invalid parameters: peer must be non-zero, "
                              "interval 10..60000ms, timeout >= interval");
  if (rv != 0)
    return clib_error_return (0, "sr_probe_set failed (rv=%d)", rv);

  if (is_del)
    {
      vlib_cli_output (vm, "SR-WG probe on %U removed.",
                       format_vnet_sw_if_index_name, vnm, sw_if_index);
      return 0;
    }

  {
    sr_probe_session_t *s = sr_probe_get (&sm->probe, sw_if_index);
    ip4_address_t l, p;

    l.as_u32 = clib_host_to_net_u32 (s->local_ip);
    p.as_u32 = clib_host_to_net_u32 (s->peer_ip);
    vlib_cli_output (vm,
                     "SR-WG probe on %U: %U -> %U  interval=%ums "
                     "timeout=%ums (udp port %u, node sr-wg4-probe)",
                     format_vnet_sw_if_index_name, vnm, sw_if_index,
                     format_ip4_address, &l, format_ip4_address, &p,
                     s->interval_ms, s->timeout_ms, SR_PROBE_PORT);
  }

  return 0;
}

VLIB_CLI_COMMAND (sr_wg_set_probe_command, static) = {
  .path = "set sr-wg probe",
  .short_help = "set sr-wg probe <sw_if_index> peer <ip> [local <ip>] "
                "[interval <ms>] [timeout <ms>] | "
                "set sr-wg probe <sw_if_index> del",
  .function = sr_wg_set_probe_command_fn,
};

/* ==================== show sr-wg probe ==================== */

static clib_error_t *
sr_wg_show_probe_command_fn (vlib_main_t *vm, unformat_input_t *input,
                             vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;
  sr_probe_main_t *pm = &sm->probe;
  vnet_main_t *vnm = vnet_get_main ();
  sr_probe_session_t *s;
  u32 i;

  vlib_cli_output (vm,
                   "SR-WG probe: udp port %u, %u session(s), "
                   "echoed=%llu unknown_replies=%llu alloc_fail=%llu",
                   SR_PROBE_PORT, pm->count, pm->echoed,
                   pm->unknown_replies, pm->alloc_fail);

  /* *INDENT-OFF* */
  pool_foreach_index (i, pm->sessions)
    {
      ip4_address_t l, p;
      u32 permille;

      s = pool_elt_at_index (pm->sessions, i);
      l.as_u32 = clib_host_to_net_u32 (s->local_ip);
      p.as_u32 = clib_host_to_net_u32 (s->peer_ip);
      permille = sr_probe_loss_permille (s);

      vlib_cli_output (vm,
                       "  %U (sw_if_index=%u) %U -> %U  interval=%ums "
                       "timeout=%ums %s",
                       format_vnet_sw_if_index_name, vnm, s->sw_if_index,
                       s->sw_if_index, format_ip4_address, &l,
                       format_ip4_address, &p, s->interval_ms, s->timeout_ms,
                       s->enabled ? "enabled" : "disabled");
      vlib_cli_output (vm,
                       "    tx=%llu rx=%llu lost=%llu late=%llu  "
                       "loss=%u.%u%%  rtt last=%.3fms min=%.3fms "
                       "avg=%.3fms max=%.3fms  jitter=%.3fms",
                       s->tx, s->rx, s->lost, s->late_replies,
                       permille / 10, permille % 10, s->last_rtt_ms,
                       s->min_rtt_ms,
                       s->n_rtt ? s->sum_rtt_ms / s->n_rtt : 0.0,
                       s->max_rtt_ms, s->jitter_ms);
    }
  /* *INDENT-ON* */

  if (pm->count == 0)
    vlib_cli_output (vm,
                     "  (none) —— 用 `set sr-wg probe <sw_if_index> "
                     "peer <ip>` 建一条");

  return 0;
}

VLIB_CLI_COMMAND (sr_wg_show_probe_command, static) = {
  .path = "show sr-wg probe",
  .short_help = "show sr-wg probe",
  .function = sr_wg_show_probe_command_fn,
};

/* ==================== sr-wg probe send ==================== */

static clib_error_t *
sr_wg_probe_send_command_fn (vlib_main_t *vm, unformat_input_t *input,
                             vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;
  vnet_main_t *vnm = vnet_get_main ();
  u32 sw_if_index = (u32) ~0;

  while (unformat_check_input (input) != UNFORMAT_END_OF_INPUT)
    {
      if (unformat (input, "%U", unformat_vnet_sw_interface, vnm,
                    &sw_if_index))
        ;
      else if (unformat (input, "%u", &sw_if_index))
        ;
      else
        break;
    }

  if (sw_if_index == (u32) ~0)
    return clib_error_return (0, "missing <sw_if_index>");

  if (!sr_probe_send_now (&sm->probe, vm, sw_if_index))
    return clib_error_return (0,
                              "no enabled probe session on %U",
                              format_vnet_sw_if_index_name, vnm, sw_if_index);

  vlib_cli_output (vm, "SR-WG probe sent on %U (结果见 `show sr-wg probe`)",
                   format_vnet_sw_if_index_name, vnm, sw_if_index);
  return 0;
}

VLIB_CLI_COMMAND (sr_wg_probe_send_command, static) = {
  .path = "sr-wg probe send",
  .short_help = "sr-wg probe send <sw_if_index>",
  .function = sr_wg_probe_send_command_fn,
};
