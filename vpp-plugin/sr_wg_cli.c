/*
 * sr_wg_cli.c - SR-WG Debug CLI
 *
 * 命令列表：
 *   show sr-wg paths
 *   show sr-wg keys
 *   show sr-wg tunnels
 *   show sr-wg stats
 *   clear sr-wg stats
 *   sr-wg path add <path_id> via <ip>:<port> [via <ip>:<port> ...]
 *                  [hmac-id <n>] [inactive]
 *   sr-wg path del <path_id>
 *   sr-wg key set <hmac_id> <hex32>
 *   sr-wg key del <hmac_id>
 *   set sr-wg transit enable port <port>
 *   set sr-wg transit disable
 *   set sr-wg tunnel <sw_if_index> path <path_id> [endpoint <ip>] [del]
 *   set sr-wg encap enable <underlay sw_if_index|name> | disable <…>
 *                   | fib-index <n>
 *   set sr-wg rebind-ports        （建 WG 接口后必做，见 sr_wg.c）
 */
#include "sr_wg.h"
#include <vnet/ip/format.h>
#include <vnet/interface_funcs.h>
#include <vnet/vnet.h>

/* ==================== show sr-wg paths ==================== */

static clib_error_t *
sr_wg_show_paths_command_fn (vlib_main_t *vm, unformat_input_t *input,
                             vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;
  int i;

  vlib_cli_output (vm, "%d path(s) configured:", sm->path_table.count);

  for (i = 0; i < sm->path_table.count; i++)
    {
      sr_path_config_t *cfg = &sm->path_table.paths[i];
      int j;

      vlib_cli_output (vm, "  [%d] path_id=%u hmac_id=%u seg_count=%u "
                           "active=%u",
                       i, cfg->path_id, cfg->hmac_id, cfg->seg_count,
                       cfg->active);

      for (j = 0; j < cfg->seg_count; j++)
        {
          ip4_address_t ip;
          u16 port;
          clib_memcpy_fast (&ip, cfg->segments[j].ip, 4);
          clib_memcpy_fast (&port, cfg->segments[j].port, 2);
          port = clib_net_to_host_u16 (port);
          vlib_cli_output (vm, "      seg[%d]: %U:%u", j,
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

  vlib_cli_output (vm, "%u key(s) configured:", sm->key_count);

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

  /*
   * 挂点在 underlay 出口接口（不是 wg0），所以绑定能不能生效取决于两件事：
   *   (1) 至少一个 underlay 接口启用了 sr-wg4-encap feature
   *   (2) 该绑定带 endpoint —— underlay 挂点上 tx_sw_if_index 是物理口，
   *       只有 endpoint_path 能命中 wg0 维度的绑定
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

      if (bind->endpoint.as_u32 != 0)
        {
          state = "usable";
          n_usable++;
        }
      else
        state = "!! NO ENDPOINT, never matches";

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
                         "  [%u] sw_if_index=%u (%U) path_id=%u endpoint=%U "
                         "%s",
                         bi, bind->sw_if_index, format_vnet_sw_if_index_name,
                         vnm, bind->sw_if_index, (u32) bind->path_id,
                         format_ip4_address, &bind->endpoint, state);
      else
        vlib_cli_output (vm,
                         "  [%u] sw_if_index=%u (<deleted>) path_id=%u "
                         "endpoint=%U %s",
                         bi, bind->sw_if_index, (u32) bind->path_id,
                         format_ip4_address, &bind->endpoint, state);
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

/* ==================== sr-wg path ==================== */

/*
 * 路径表 CLI。
 *
 * 路径 / P-Key 原本只能通过 Binary API 下发，现场排障与 e2e 测试都需要
 * 一条纯 CLI 通道，这里补上（与 vl_api_sr_wg_path_add_del_t_handler /
 * vl_api_sr_wg_key_set_t_handler 语义完全一致）。
 *
 *   sr-wg path add <path_id> via <ip>:<port> [via <ip>:<port> ...]
 *                  [hmac-id <n>] [inactive]
 *   sr-wg path del <path_id>
 *
 * 段列表顺序遵循 sr_header.h 约定：
 *   segments[0]           = 最终目的（对端 CPE，port 51820）
 *   segments[seg_count-1] = 第一跳（transit，port 51821）
 * 因此 via 的书写顺序是「从最终目的到第一跳」。encap 取
 * segments[seg_count-1] 作为外层 dst（"首跳免费"），并把 seg_left
 * 置为 seg_count-1；此后每经过一个 transit 执行一次
 * sr_hdr_pop_segment()（next = segments[seg_left-1]，然后 seg_left--）。
 */
static clib_error_t *
sr_wg_path_command_fn (vlib_main_t *vm, unformat_input_t *input,
                       vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;
  sr_path_config_t cfg;
  ip4_address_t ip;
  u32 path_id = 0, hmac_id = 0, port = 0;
  u8 add = 0, del = 0, inactive = 0;
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
      else if (unformat (input, "via %U:%u", unformat_ip4_address, &ip, &port))
        {
          u16 port_net;

          if (cfg.seg_count >= SR_MAX_SEGS)
            return clib_error_return (0, "too many segments (max %d)",
                                      SR_MAX_SEGS);
          if (port == 0 || port > 65535)
            return clib_error_return (0, "bad port %u in segment %u", port,
                                      cfg.seg_count);
          clib_memcpy_fast (cfg.segments[cfg.seg_count].ip, &ip, 4);
          port_net = clib_host_to_net_u16 ((u16) port);
          clib_memcpy_fast (cfg.segments[cfg.seg_count].port, &port_net, 2);
          cfg.seg_count++;
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
  if (cfg.seg_count == 0)
    return clib_error_return (0, "at least one 'via <ip>:<port>' required");

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
  if (rv != 0)
    return clib_error_return (0, "sr_path_add failed (rv=%d)", rv);

  vlib_cli_output (vm, "SR-WG path %u set: hmac_id=%u seg_count=%u active=%u",
                   path_id, cfg.hmac_id, cfg.seg_count, cfg.active);
  return 0;
}

VLIB_CLI_COMMAND (sr_wg_path_command, static) = {
  .path = "sr-wg path",
  .short_help = "sr-wg path add <path_id> via <ip>:<port> "
                "[via <ip>:<port> ...] [hmac-id <n>] [inactive] | "
                "sr-wg path del <path_id>",
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
  if (rv != 0)
    return clib_error_return (0, "sr_wg_key_add failed (rv=%d)", rv);

  vlib_cli_output (vm, "SR-WG key hmac_id=%u set.", hmac_id);
  return 0;
}

VLIB_CLI_COMMAND (sr_wg_key_command, static) = {
  .path = "sr-wg key",
  .short_help = "sr-wg key set <hmac_id> <hex32> | sr-wg key del <hmac_id>",
  .function = sr_wg_key_command_fn,
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
      if (port == 0)
        port = SR_PORT_TRANSIT;
      sm->transit_port = (u16) port;
      sm->transit_enabled = 1;

      /* 端口就是功能多路复用键：改了端口必须重注册，否则包进不到 transit 节点 */
      sr_wg_ports_rebind (sm);

      vlib_cli_output (vm,
                       "SR-WG transit enabled: port=%u "
                       "(外层源 IP/端口不改写，中转不参与 WireGuard)",
                       sm->transit_port);
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
 *   set sr-wg tunnel <sw_if_index> path <path_id> [endpoint <ip>] [del]
 *
 * 本命令**只写绑定表**，不启用 feature。encap 的挂点在 underlay 出口接口
 * （wg4-output-tun 不参与 feature 链，详见 sr_wg.h），必须另外执行
 * `set sr-wg encap enable <underlay sw_if_index>`。
 *
 * underlay 挂点上数据面按外层 dst 查绑定，所以 add 时 [endpoint] 实际必填；
 * 缺失时这里仍接受配置但立刻告警，避免"配了却没生效"要抓包才发现。
 */
static clib_error_t *
sr_wg_set_tunnel_command_fn (vlib_main_t *vm, unformat_input_t *input,
                             vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;
  ip4_address_t endpoint;
  u32 sw_if_index = (u32) ~0;
  u32 path_id = 0;
  u8 have_path = 0, is_del = 0, have_endpoint = 0;
  int rv;

  clib_memset (&endpoint, 0, sizeof (endpoint));

  while (unformat_check_input (input) != UNFORMAT_END_OF_INPUT)
    {
      if (unformat (input, "%u", &sw_if_index))
        ;
      else if (unformat (input, "path %u", &path_id))
        have_path = 1;
      else if (unformat (input, "endpoint %U", unformat_ip4_address, &endpoint))
        have_endpoint = 1;
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

  rv = sr_wg_tunnel_set_path (sm, sw_if_index, (u16) path_id,
                              have_endpoint ? &endpoint : NULL,
                              is_del ? 0 : 1);
  if (rv != 0)
    return clib_error_return (0, "sr-wg tunnel set path failed (rv=%d)", rv);

  if (is_del)
    {
      vlib_cli_output (vm, "SR-WG tunnel %u unbound.", sw_if_index);
      return 0;
    }

  if (have_endpoint)
    vlib_cli_output (vm,
                     "SR-WG tunnel %u bound to path_id=%u endpoint=%U.",
                     sw_if_index, (u32) (u16) path_id,
                     format_ip4_address, &endpoint);
  else
    vlib_cli_output (vm,
                     "SR-WG tunnel %u bound to path_id=%u, "
                     "!! WARNING: 未指定 endpoint，本绑定在 underlay 挂点上"
                     "永远不会命中，流量将直发 peer。",
                     sw_if_index, (u32) (u16) path_id);

  if (sm->encap_enabled_count == 0)
    vlib_cli_output (vm,
                     "!! WARNING: sr-wg4-encap 尚未在任何接口上启用，"
                     "请执行 `set sr-wg encap enable <underlay sw_if_index>`。");

  return 0;
}

VLIB_CLI_COMMAND (sr_wg_set_tunnel_command, static) = {
  .path = "set sr-wg tunnel",
  .short_help = "set sr-wg tunnel <sw_if_index> path <path_id> "
                "[endpoint <ip>] [del]",
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
 * WireGuard 在创建接口时会重新注册 listen-port（udp_register_dst_port），
 * 覆盖 SR-WG 插件在 init 阶段对 51820 的注册。此命令重新把
 * 51820 → sr-wg4-input、51821 → sr-wg4-transit 注册回来。
 * 必须在所有 WireGuard 接口创建完成后调用。
 */
static clib_error_t *
sr_wg_rebind_ports_command_fn (vlib_main_t *vm, unformat_input_t *input,
                               vlib_cli_command_t *cmd)
{
  sr_wg_main_t *sm = &sr_wg_main;

  sr_wg_ports_rebind (sm);

  vlib_cli_output (vm, "SR-WG ports rebound: %u -> sr-wg4-input, %u -> sr-wg4-transit",
                   sm->vpn_port, sm->transit_port);
  return 0;
}

VLIB_CLI_COMMAND (sr_wg_rebind_ports_command, static) = {
  .path = "set sr-wg rebind-ports",
  .short_help = "set sr-wg rebind-ports",
  .function = sr_wg_rebind_ports_command_fn,
};
