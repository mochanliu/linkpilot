/*
 * sr_wg_api.c - SR-WG Binary API Handlers
 *
 * VPP 26.06 API 模式：
 *   - handler 命名: vl_api_<msg_name>_t_handler
 *   - REPLY_MACRO 来自 api_helper_macros.h
 *   - setup_message_id_table() 来自自动生成的 .api.c
 */
#include "sr_wg.h"

#include <vlibapi/api.h>
#include <vlibmemory/api.h>
#include <vlibapi/api_helper_macros.h>

/*
 * 自动生成的 API 定义（由 CMake/vppapigen 从 sr_wg.api 生成）。
 *
 * 外部插件构建（find_package(VPP)）时，生成产物平铺在 build 根目录，
 * 而非 in-tree 构建的 plugins/<name>/ 子目录，故不带 sr_wg/ 前缀。
 *
 * 注意：sr_wg.api.h 没有整体 include guard，而是按 vl_typedefs /
 * vl_endianfun / vl_calcsizefun / vl_printfun 分段生成内容。必须先定义
 * vl_typedefs 再包含，否则不会生成任何类型（并触发
 * "no content included" 警告）。endian / calc_size / print 函数以及
 * setup_message_id_table() 由文件末尾包含的 sr_wg.api.c 提供，
 * 因此此处不能提前定义 vl_endianfun 等宏，否则 .api.c 里的同名
 * 函数会因 section guard 而被跳过。
 */
#define vl_typedefs
#include <sr_wg.api.h>
#undef vl_typedefs

/* VL_API_SR_WG_* 消息 ID 枚举（.api.h 不会自动包含它） */
#include <sr_wg.api_enum.h>

/*
 * ip4_address / ip6_address 在 ip_types.api 中是 manual_print typedef，
 * 其 format_vl_api_ip4_address_t 等格式化函数不会由 vppapigen 生成，
 * 需由本头文件提供；否则 sr_wg.api.c 展开 vl_printfun 段时
 * 会报 'format_vl_api_ip4_address_t' undeclared。
 */
#include <vnet/ip/ip_format_fns.h>

/* ==================== Path API Handlers ==================== */

static void
vl_api_sr_wg_path_add_del_t_handler (vl_api_sr_wg_path_add_del_t *mp)
{
  vl_api_sr_wg_path_add_del_reply_t *rmp;
  sr_wg_main_t *sm = &sr_wg_main;
  int rv = 0;

  if (mp->is_add)
    {
      sr_path_config_t cfg;
      int i;

      clib_memset (&cfg, 0, sizeof (cfg));
      cfg.path_id = clib_net_to_host_u16 (mp->path_id);
      cfg.hmac_id = clib_net_to_host_u16 (mp->hmac_id);
      cfg.seg_count = mp->seg_count;
      cfg.active = mp->active;

      if (cfg.seg_count == 0 || cfg.seg_count > SR_MAX_SEGS)
        {
          rv = VNET_API_ERROR_INVALID_VALUE;
          goto reply;
        }

      /* 拷贝段列表 */
      for (i = 0; i < cfg.seg_count; i++)
        {
          clib_memcpy_fast (cfg.segments[i].ip, mp->segments[i].ip, 4);
          /* port: API 传递为 host order u16, 转为网络序存储 */
          u16 port_net = clib_host_to_net_u16 (
            clib_net_to_host_u16 (mp->segments[i].port));
          clib_memcpy_fast (cfg.segments[i].port, &port_net, 2);
        }

      /* 从密钥表查找 P-Key 并填入路径配置 */
      sr_wg_key_entry_t *key = sr_wg_key_lookup (sm, cfg.hmac_id);
      if (key)
        clib_memcpy_fast (cfg.pkey, key->key, SR_PKEY_LEN);

      rv = sr_path_add (&sm->path_table, &cfg);
      if (rv != 0)
        rv = VNET_API_ERROR_NO_SUCH_TABLE;
    }
  else
    {
      u16 path_id = clib_net_to_host_u16 (mp->path_id);
      rv = sr_path_del (&sm->path_table, path_id);
      if (rv != 0)
        rv = VNET_API_ERROR_NO_SUCH_ENTRY;
    }

reply:
  REPLY_MACRO (VL_API_SR_WG_PATH_ADD_DEL_REPLY);
}

static void
vl_api_sr_wg_path_dump_t_handler (vl_api_sr_wg_path_dump_t *mp)
{
  sr_wg_main_t *sm = &sr_wg_main;
  vl_api_sr_wg_path_details_t *rmp;
  vl_api_registration_t *reg;
  u16 filter_path_id = clib_net_to_host_u16 (mp->path_id);
  int i;

  /* vl_api_send_msg 需要客户端注册句柄（而非 vlib_main_t），
   * 否则会在 rp->registration_type 解引用时崩溃 */
  reg = vl_api_client_index_to_registration (mp->client_index);
  if (!reg)
    return;

  for (i = 0; i < sm->path_table.count; i++)
    {
      sr_path_config_t *cfg = &sm->path_table.paths[i];
      int j;

      if (filter_path_id != 0 && cfg->path_id != filter_path_id)
        continue;

      rmp = vl_msg_api_alloc (sizeof (*rmp));
      clib_memset (rmp, 0, sizeof (*rmp));
      rmp->_vl_msg_id =
        clib_host_to_net_u16 (VL_API_SR_WG_PATH_DETAILS + sm->msg_id_base);
      rmp->context = mp->context;
      rmp->path_id = clib_host_to_net_u16 (cfg->path_id);
      rmp->hmac_id = clib_host_to_net_u16 (cfg->hmac_id);
      rmp->seg_count = cfg->seg_count;
      rmp->active = cfg->active;

      for (j = 0; j < cfg->seg_count; j++)
        {
          clib_memcpy_fast (rmp->segments[j].ip, cfg->segments[j].ip, 4);
          u16 port_net;
          clib_memcpy_fast (&port_net, cfg->segments[j].port, 2);
          rmp->segments[j].port = clib_net_to_host_u16 (port_net);
        }

      vl_api_send_msg (reg, (u8 *) rmp);
    }
}

/* ==================== Key API Handlers ==================== */

static void
vl_api_sr_wg_key_set_t_handler (vl_api_sr_wg_key_set_t *mp)
{
  vl_api_sr_wg_key_set_reply_t *rmp;
  sr_wg_main_t *sm = &sr_wg_main;
  u16 hmac_id = clib_net_to_host_u16 (mp->hmac_id);
  int rv;

  rv = sr_wg_key_add (sm, hmac_id, mp->key);
  if (rv != 0)
    rv = VNET_API_ERROR_NO_SUCH_TABLE;

  REPLY_MACRO (VL_API_SR_WG_KEY_SET_REPLY);
}

static void
vl_api_sr_wg_key_del_t_handler (vl_api_sr_wg_key_del_t *mp)
{
  vl_api_sr_wg_key_del_reply_t *rmp;
  sr_wg_main_t *sm = &sr_wg_main;
  u16 hmac_id = clib_net_to_host_u16 (mp->hmac_id);
  int rv;

  rv = sr_wg_key_del (sm, hmac_id);
  if (rv != 0)
    rv = VNET_API_ERROR_NO_SUCH_ENTRY;

  REPLY_MACRO (VL_API_SR_WG_KEY_DEL_REPLY);
}

static void
vl_api_sr_wg_key_dump_t_handler (vl_api_sr_wg_key_dump_t *mp)
{
  sr_wg_main_t *sm = &sr_wg_main;
  vl_api_sr_wg_key_details_t *rmp;
  vl_api_registration_t *reg;
  u32 i;

  reg = vl_api_client_index_to_registration (mp->client_index);
  if (!reg)
    return;

  for (i = 0; i < sm->key_count; i++)
    {
      rmp = vl_msg_api_alloc (sizeof (*rmp));
      clib_memset (rmp, 0, sizeof (*rmp));
      rmp->_vl_msg_id =
        clib_host_to_net_u16 (VL_API_SR_WG_KEY_DETAILS + sm->msg_id_base);
      rmp->context = mp->context;
      rmp->hmac_id = clib_host_to_net_u16 (sm->keys[i].hmac_id);

      vl_api_send_msg (reg, (u8 *) rmp);
    }
}

/* ==================== Transit API Handlers ==================== */

static void
vl_api_sr_wg_transit_enable_t_handler (vl_api_sr_wg_transit_enable_t *mp)
{
  vl_api_sr_wg_transit_enable_reply_t *rmp;
  sr_wg_main_t *sm = &sr_wg_main;
  int rv = 0;

  sm->transit_port = clib_net_to_host_u16 (mp->port);
  clib_memcpy_fast (&sm->local_ip4, mp->local_ip, 4);
  sm->transit_enabled = 1;

  REPLY_MACRO (VL_API_SR_WG_TRANSIT_ENABLE_REPLY);
}

static void
vl_api_sr_wg_transit_disable_t_handler (vl_api_sr_wg_transit_disable_t *mp)
{
  vl_api_sr_wg_transit_disable_reply_t *rmp;
  sr_wg_main_t *sm = &sr_wg_main;
  int rv = 0;

  sm->transit_enabled = 0;
  clib_memset (&sm->local_ip4, 0, sizeof (sm->local_ip4));

  REPLY_MACRO (VL_API_SR_WG_TRANSIT_DISABLE_REPLY);
}

/* ==================== Encap API Handlers ==================== */

static void
vl_api_sr_wg_tunnel_set_path_t_handler (vl_api_sr_wg_tunnel_set_path_t *mp)
{
  vl_api_sr_wg_tunnel_set_path_reply_t *rmp;
  sr_wg_main_t *sm = &sr_wg_main;
  ip4_address_t endpoint;
  u32 sw_if_index = clib_net_to_host_u32 (mp->sw_if_index);
  u16 path_id = clib_net_to_host_u16 (mp->path_id);
  int rv;

  clib_memcpy_fast (&endpoint, mp->endpoint_ip, 4);

  rv = sr_wg_tunnel_set_path (sm, sw_if_index, path_id,
                              endpoint.as_u32 ? &endpoint : NULL,
                              mp->is_add ? 1 : 0);

  REPLY_MACRO (VL_API_SR_WG_TUNNEL_SET_PATH_REPLY);
}

static void
vl_api_sr_wg_tunnel_dump_t_handler (vl_api_sr_wg_tunnel_dump_t *mp)
{
  sr_wg_main_t *sm = &sr_wg_main;
  vl_api_sr_wg_tunnel_details_t *rmp;
  vl_api_registration_t *reg;
  u32 filter = clib_net_to_host_u32 (mp->sw_if_index);
  u32 bi;

  reg = vl_api_client_index_to_registration (mp->client_index);
  if (!reg)
    return;

  /* pool 就是带 free bitmap 的 vec，直接遍历 + 跳过空洞 */
  for (bi = 0; bi < vec_len (sm->binds); bi++)
    {
      sr_wg_tunnel_bind_t *bind;

      if (pool_is_free_index (sm->binds, bi))
        continue;

      bind = pool_elt_at_index (sm->binds, bi);

      if (filter != (u32) ~0 && bind->sw_if_index != filter)
        continue;

      rmp = vl_msg_api_alloc (sizeof (*rmp));
      clib_memset (rmp, 0, sizeof (*rmp));
      rmp->_vl_msg_id =
        clib_host_to_net_u16 (VL_API_SR_WG_TUNNEL_DETAILS + sm->msg_id_base);
      rmp->context = mp->context;
      rmp->sw_if_index = clib_host_to_net_u32 (bind->sw_if_index);
      rmp->path_id = clib_host_to_net_u16 (bind->path_id);
      clib_memcpy_fast (rmp->endpoint_ip, &bind->endpoint, 4);
      rmp->feature_enabled = bind->feature_on;

      vl_api_send_msg (reg, (u8 *) rmp);
    }
}

static void
vl_api_sr_wg_encap_enable_t_handler (vl_api_sr_wg_encap_enable_t *mp)
{
  vl_api_sr_wg_encap_enable_reply_t *rmp;
  sr_wg_main_t *sm = &sr_wg_main;
  u32 sw_if_index = clib_net_to_host_u32 (mp->sw_if_index);
  int rv;

  rv = sr_wg_encap_enable_disable (sm, sw_if_index, mp->enable ? 1 : 0);

  REPLY_MACRO (VL_API_SR_WG_ENCAP_ENABLE_REPLY);
}

/* ==================== Stats API Handlers ==================== */

static void
vl_api_sr_wg_stats_get_t_handler (vl_api_sr_wg_stats_get_t *mp)
{
  vl_api_sr_wg_stats_get_reply_t *rmp;
  vl_api_registration_t *reg;
  sr_wg_main_t *sm = &sr_wg_main;

  reg = vl_api_client_index_to_registration (mp->client_index);
  if (!reg)
    return;

  rmp = vl_msg_api_alloc (sizeof (*rmp));
  clib_memset (rmp, 0, sizeof (*rmp));
  rmp->_vl_msg_id =
    clib_host_to_net_u16 (VL_API_SR_WG_STATS_GET_REPLY + sm->msg_id_base);
  rmp->context = mp->context;
  rmp->retval = 0;

  /* 计数器（host -> network order） */
  rmp->encap_ok = clib_host_to_net_u64 (sm->counters.encap_ok);
  rmp->encap_no_path = clib_host_to_net_u64 (sm->counters.encap_no_path);
  rmp->encap_hmac_fail = clib_host_to_net_u64 (sm->counters.encap_hmac_fail);
  rmp->encap_bypass = clib_host_to_net_u64 (sm->counters.encap_bypass);
  rmp->encap_no_headroom =
    clib_host_to_net_u64 (sm->counters.encap_no_headroom);
  rmp->transit_ok = clib_host_to_net_u64 (sm->counters.transit_ok);
  rmp->transit_hmac_fail =
    clib_host_to_net_u64 (sm->counters.transit_hmac_fail);
  rmp->transit_seg_exhausted =
    clib_host_to_net_u64 (sm->counters.transit_seg_exhausted);
  rmp->transit_bad_magic =
    clib_host_to_net_u64 (sm->counters.transit_bad_magic);
  rmp->transit_bad_ver = clib_host_to_net_u64 (sm->counters.transit_bad_ver);
  rmp->transit_bad_len = clib_host_to_net_u64 (sm->counters.transit_bad_len);
  rmp->decap_ok = clib_host_to_net_u64 (sm->counters.decap_ok);
  rmp->decap_hmac_fail = clib_host_to_net_u64 (sm->counters.decap_hmac_fail);
  rmp->decap_seg_not_zero =
    clib_host_to_net_u64 (sm->counters.decap_seg_not_zero);
  rmp->decap_bypass = clib_host_to_net_u64 (sm->counters.decap_bypass);
  rmp->drop_total = clib_host_to_net_u64 (sm->counters.drop_total);

  vl_api_send_msg (reg, (u8 *) rmp);
}

static void
vl_api_sr_wg_stats_clear_t_handler (vl_api_sr_wg_stats_clear_t *mp)
{
  vl_api_sr_wg_stats_clear_reply_t *rmp;
  sr_wg_main_t *sm = &sr_wg_main;
  int rv = 0;

  clib_memset (&sm->counters, 0, sizeof (sm->counters));

  REPLY_MACRO (VL_API_SR_WG_STATS_CLEAR_REPLY);
}

/* ==================== API 消息注册 ==================== */

/* 包含自动生成的注册代码（提供 setup_message_id_table） */
#include <sr_wg.api.c>

void
sr_wg_api_hookup (vlib_main_t *vm)
{
  sr_wg_main_t *sm = &sr_wg_main;
  sm->msg_id_base = setup_message_id_table ();
}
