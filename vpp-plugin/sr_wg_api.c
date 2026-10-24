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

/*
 * REPLY_MACRO 发出的是 `本地枚举值 + REPLY_MSG_ID_BASE`，而
 * api_helper_macros.h 在插件没定义它时把 REPLY_MSG_ID_BASE 默认成 0
 * （#ifndef 兜底，不报错、不警告）。sr_wg.api_enum.h 的枚举从 0 开始，
 * 所以漏掉这一行的后果是 reply 带着**插件内部下标**上线：实测
 * sr_wg_key_set 的应答 msg_id = 5、sr_wg_key_del = 7，正好等于枚举里
 * _REPLY 的下标；而 5 和 7 在全局表里是 rx_thread_exit 与
 * memclnt_read_timeout，客户端于是把应答当成异步通知丢掉，rpc 永远超时
 * ——本机的观测证据：数据面确实执行了（key_dump 随后能 dump 出那把钥匙），
 * 但应答认不出来。
 *
 * CLI 走不到这条路（vppctl 的 sr-wg 命令是本插件自己的 CLI），所以只有
 * 二进制客户端会撞上。手工发送点（*_details、stats_get_reply）都写了
 * `+ sm->msg_id_base`，唯独 REPLY_MACRO 这条少了一个 base 定义。
 */
#define REPLY_MSG_ID_BASE sr_wg_main.msg_id_base
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
          /*
           * 消息里 port 两字节按网络序来，路径表按网络序存 —— 直通即可。
           * （旧写法是 host_to_net(net_to_host(x))，两次 swap 互为恒等，
           * 结果一样但读起来像在漏一次字节序转换。）
           */
          clib_memcpy_fast (cfg.segments[i].port, &mp->segments[i].port, 2);
        }

      /* 从密钥表查找 P-Key 并填入路径配置 */
      sr_wg_key_entry_t *key = sr_wg_key_lookup (sm, cfg.hmac_id);
      if (!key)
        {
          /*
           * 查不到密钥就必须报错，不能让 cfg.pkey 保持全零继续建路径：
           * 全零密钥的路径会正常签名并发包，但对端一律验签失败，
           * 现象是"配置都下发成功了、隧道就是不通"，控制器极难定位。
           */
          rv = VNET_API_ERROR_NO_SUCH_ENTRY;
          goto reply;
        }
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
      else
        /* 路径没了，指向它的隧道绑定必须一起摘掉，否则留下悬空绑定 */
        sr_wg_binds_drop_for_path (sm, path_id);
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

      /* ~0 = 全量；path_id 0 是合法值，不能当"全部"的哨兵用 */
      if (filter_path_id != 0xffff && cfg->path_id != filter_path_id)
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
          /*
           * 段里的 ip 与 port 在路径表中**本来就是网络序**，所以两字节原样搬进
           * 消息即符合 VPP 的 API 约定（消息字段一律网络序）。
           *
           * 这里原先写的是 `rmp->segments[j].port = clib_net_to_host_u16 (…)`：
           * 对已经是网络序的值又 swap 一次，x86 上以小端落进消息 —— 同一个端口在
           * path_add_del（请求，大端）和 path_details（应答，小端）里字节序不一致。
           * Go 客户端实测：装进 51820，dump 回 27850 (= 0x6CCA)。
           */
          clib_memcpy_fast (rmp->segments[j].ip, cfg->segments[j].ip, 4);
          clib_memcpy_fast (&rmp->segments[j].port, cfg->segments[j].port, 2);
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

  /*
   * 还有路径引用这把钥匙时拒绝删除：路径里存的是副本，删了表项路径照旧
   * 签名，但对端已经不再持有这把钥匙 —— 一个"看不见的密钥漂移"。
   * 换密钥请直接用 key set（会同步刷新路径副本），不要先删再建。
   */
  if (sr_path_count_using_hmac_id (&sm->path_table, hmac_id) > 0)
    {
      rv = VNET_API_ERROR_INVALID_VALUE;
      goto reply;
    }

  rv = sr_wg_key_del (sm, hmac_id);
  if (rv != 0)
    rv = VNET_API_ERROR_NO_SUCH_ENTRY;

reply:
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
  u16 port = clib_net_to_host_u16 (mp->port);
  int rv = 0;

  if (port == 0)
    port = SR_PORT_TRANSIT;

  /* 端口是数据面的功能多路复用键：add 内部就重注册，不必另外 rebind。
   * 集合语义让一台中转可以同时服务多个 transit 端口。 */
  rv = sr_wg_port_add_del (sm, SR_WG_PORT_TRANSIT, port, 1);
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
                              clib_net_to_host_u16 (mp->dst_port),
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

  /* 每条 details 的 is_effective 都要读 encap_enabled_count：先把它对回 VPP 的登记 */
  sr_wg_encap_reconcile (sm);

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
      rmp->dst_port = clib_host_to_net_u16 (bind->dst_port);
      clib_memcpy_fast (rmp->endpoint_ip, &bind->endpoint, 4);
      /*
       * underlay 挂点上只有 endpoint_path 能命中 wg0 维度的绑定，且必须
       * 至少有一个 underlay 接口启用了 sr-wg4-encap，否则流量静默旁路。
       */
      rmp->is_effective =
        (bind->endpoint.as_u32 != 0 && sm->encap_enabled_count > 0) ? 1 : 0;

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

/* ==================== 端口 / 挂点配置 API Handlers ==================== */

static void
vl_api_sr_wg_rebind_ports_t_handler (vl_api_sr_wg_rebind_ports_t *mp)
{
  vl_api_sr_wg_rebind_ports_reply_t *rmp;
  sr_wg_main_t *sm = &sr_wg_main;
  int rv = 0;

  sr_wg_ports_rebind (sm);

  REPLY_MACRO (VL_API_SR_WG_REBIND_PORTS_REPLY);
}

/*
 * 增/删一个隧道监听端口或中转端口（定案 2「每隧道一个端口」）。
 * add 是幂等的并且总会把端口重挂回本插件 —— agent 建完 WG 接口之后发的就是它，
 * 因为 wireguard 会把新建接口的 listen-port 抢回 wg4-input。
 */
static void
vl_api_sr_wg_port_add_del_t_handler (vl_api_sr_wg_port_add_del_t *mp)
{
  vl_api_sr_wg_port_add_del_reply_t *rmp;
  sr_wg_main_t *sm = &sr_wg_main;
  u16 port = clib_net_to_host_u16 (mp->port);
  int rv;

  rv = sr_wg_port_add_del (sm, mp->kind, port, mp->is_add ? 1 : 0);

  REPLY_MACRO (VL_API_SR_WG_PORT_ADD_DEL_REPLY);
}

/* 逐条 dump 端口集合：一张 CPE 上可能几十条隧道，定长数组装不下。 */
static void
vl_api_sr_wg_port_dump_t_handler (vl_api_sr_wg_port_dump_t *mp)
{
  sr_wg_main_t *sm = &sr_wg_main;
  vl_api_sr_wg_port_details_t *rmp;
  vl_api_registration_t *reg;
  uword kind;

  reg = vl_api_client_index_to_registration (mp->client_index);
  if (!reg)
    return;

  for (kind = 0; kind <= SR_WG_PORT_TRANSIT; kind++)
    {
      clib_bitmap_t *bitmap = kind == SR_WG_PORT_TRANSIT ? sm->transit_ports
                                                        : sm->vpn_ports;
      uword port;

      /* *INDENT-OFF* */
      clib_bitmap_foreach (port, bitmap)
        {
          rmp = vl_msg_api_alloc (sizeof (*rmp));
          clib_memset (rmp, 0, sizeof (*rmp));
          rmp->_vl_msg_id =
            clib_host_to_net_u16 (VL_API_SR_WG_PORT_DETAILS + sm->msg_id_base);
          rmp->context = mp->context;
          rmp->kind = (u8) kind;
          rmp->port = clib_host_to_net_u16 ((u16) port);

          vl_api_send_msg (reg, (u8 *) rmp);
        }
      /* *INDENT-ON* */
    }
}

static void
vl_api_sr_wg_encap_fib_set_t_handler (vl_api_sr_wg_encap_fib_set_t *mp)
{
  vl_api_sr_wg_encap_fib_set_reply_t *rmp;
  sr_wg_main_t *sm = &sr_wg_main;
  int rv = 0;

  sm->encap_fib_index = clib_net_to_host_u32 (mp->fib_index);

  REPLY_MACRO (VL_API_SR_WG_ENCAP_FIB_SET_REPLY);
}

static void
vl_api_sr_wg_config_dump_t_handler (vl_api_sr_wg_config_dump_t *mp)
{
  vl_api_sr_wg_config_details_t *rmp;
  vl_api_registration_t *reg;
  sr_wg_main_t *sm = &sr_wg_main;
  uword key, val;
  u32 n = 0;

  reg = vl_api_client_index_to_registration (mp->client_index);
  if (!reg)
    return;

  /* 下面 enabled_ifs 这张表直接来自我们的 hash，先把它对回 VPP 的登记 */
  sr_wg_encap_reconcile (sm);

  rmp = vl_msg_api_alloc (sizeof (*rmp));
  clib_memset (rmp, 0, sizeof (*rmp));
  rmp->_vl_msg_id =
    clib_host_to_net_u16 (VL_API_SR_WG_CONFIG_DETAILS + sm->msg_id_base);
  rmp->context = mp->context;
  /*
   * 端口是集合（每隧道一个端口），这里只给数量；端口本身由
   * sr_wg_port_dump 逐条 dump —— 一张 CPE 上几十条隧道时定长数组放不下。
   */
  rmp->vpn_port_count = clib_host_to_net_u16 (
    sr_wg_port_count (sm, SR_WG_PORT_VPN));
  rmp->transit_port_count = clib_host_to_net_u16 (
    sr_wg_port_count (sm, SR_WG_PORT_TRANSIT));
  rmp->transit_enabled = sm->transit_enabled;
  rmp->encap_fib_index = clib_host_to_net_u32 (sm->encap_fib_index);
  rmp->path_count = sm->path_table.count;
  rmp->key_count = sm->key_count;

  /* *INDENT-OFF* */
  hash_foreach (key, val, sm->encap_enabled_ifs, ({
    if (n < ARRAY_LEN (rmp->enabled_ifs))
      rmp->enabled_ifs[n++] = clib_host_to_net_u32 ((u32) key);
  }));
  /* *INDENT-ON* */

  rmp->n_enabled_ifs = clib_host_to_net_u32 (n);

  /* 观测表规模与干扰计数：控制器据此判断"有没有人在往观测表灌假源" */
  rmp->obs_count = clib_host_to_net_u32 (sm->obs.count);
  rmp->obs_records = clib_host_to_net_u64 (sm->obs.records);
  rmp->obs_evictions = clib_host_to_net_u64 (sm->obs.evictions);

  vl_api_send_msg (reg, (u8 *) rmp);
}

/* ==================== 外层源观测 API Handlers ==================== */

static void
vl_api_sr_wg_obs_dump_t_handler (vl_api_sr_wg_obs_dump_t *mp)
{
  sr_wg_main_t *sm = &sr_wg_main;
  vl_api_sr_wg_obs_details_t *rmp;
  vl_api_registration_t *reg;
  sr_obs_entry_t e;
  u32 cursor = 0;
  const u64 now_sec = (u64) vlib_time_now (vlib_get_main ());

  reg = vl_api_client_index_to_registration (mp->client_index);
  if (!reg)
    return;

  while (sr_obs_next (&sm->obs, &cursor, &e))
    {
      u32 src_net;

      rmp = vl_msg_api_alloc (sizeof (*rmp));
      clib_memset (rmp, 0, sizeof (*rmp));
      rmp->_vl_msg_id =
        clib_host_to_net_u16 (VL_API_SR_WG_OBS_DETAILS + sm->msg_id_base);
      rmp->context = mp->context;
      /* 表里存的是主机序 u32（便于比较/单测），API 要的是地址字节序 */
      src_net = clib_host_to_net_u32 (e.src_ip);
      clib_memcpy_fast (rmp->src_ip, &src_net, 4);
      rmp->src_port = clib_host_to_net_u16 (e.src_port);
      rmp->dst_port = clib_host_to_net_u16 (e.dst_port);
      rmp->rx_if = clib_host_to_net_u32 (e.rx_if);
      rmp->hits = clib_host_to_net_u32 (e.hits);
      /* 只给相对时间：插件时钟是"相对 VPP 启动"的，绝对值对控制器无意义 */
      rmp->age_sec = clib_host_to_net_u32 (
        e.last_seen <= now_sec ? (u32) (now_sec - e.last_seen) : 0);
      rmp->verified = e.verified;

      vl_api_send_msg (reg, (u8 *) rmp);
    }
}

static void
vl_api_sr_wg_obs_clear_t_handler (vl_api_sr_wg_obs_clear_t *mp)
{
  vl_api_sr_wg_obs_clear_reply_t *rmp;
  sr_wg_main_t *sm = &sr_wg_main;
  int rv = 0;

  sr_obs_clear (&sm->obs);

  REPLY_MACRO (VL_API_SR_WG_OBS_CLEAR_REPLY);
}

/* ==================== 隧道内探针 API Handlers ==================== */

/* 毫秒 -> 微秒（API 用整数微秒，避免在二进制接口里传浮点） */
static u32
sr_probe_ms_to_us (f64 ms)
{
  if (ms <= 0)
    return 0;
  if (ms > 4294967.0)
    return (u32) ~0;
  return (u32) (ms * 1000.0 + 0.5);
}

static void
vl_api_sr_wg_probe_set_t_handler (vl_api_sr_wg_probe_set_t *mp)
{
  vl_api_sr_wg_probe_set_reply_t *rmp;
  sr_wg_main_t *sm = &sr_wg_main;
  ip4_address_t peer, local;
  int rv;

  clib_memcpy_fast (&peer, mp->peer_ip, 4);
  clib_memcpy_fast (&local, mp->local_ip, 4);

  rv = sr_probe_set (&sm->probe, clib_net_to_host_u32 (mp->sw_if_index),
                     clib_net_to_host_u32 (peer.as_u32),
                     clib_net_to_host_u32 (local.as_u32),
                     clib_net_to_host_u32 (mp->interval_ms),
                     clib_net_to_host_u32 (mp->timeout_ms),
                     mp->is_add ? 1 : 0);

  REPLY_MACRO (VL_API_SR_WG_PROBE_SET_REPLY);
}

static void
vl_api_sr_wg_probe_dump_t_handler (vl_api_sr_wg_probe_dump_t *mp)
{
  sr_wg_main_t *sm = &sr_wg_main;
  sr_probe_main_t *pm = &sm->probe;
  vl_api_sr_wg_probe_details_t *rmp;
  vl_api_registration_t *reg;
  sr_probe_session_t *s;
  u32 i;

  reg = vl_api_client_index_to_registration (mp->client_index);
  if (!reg)
    return;

  /* *INDENT-OFF* */
  pool_foreach_index (i, pm->sessions)
    {
      u32 addr;

      s = pool_elt_at_index (pm->sessions, i);

      rmp = vl_msg_api_alloc (sizeof (*rmp));
      clib_memset (rmp, 0, sizeof (*rmp));
      rmp->_vl_msg_id =
        clib_host_to_net_u16 (VL_API_SR_WG_PROBE_DETAILS + sm->msg_id_base);
      rmp->context = mp->context;
      rmp->sw_if_index = clib_host_to_net_u32 (s->sw_if_index);

      addr = clib_host_to_net_u32 (s->peer_ip);
      clib_memcpy_fast (rmp->peer_ip, &addr, 4);
      addr = clib_host_to_net_u32 (s->local_ip);
      clib_memcpy_fast (rmp->local_ip, &addr, 4);

      rmp->interval_ms = clib_host_to_net_u32 (s->interval_ms);
      rmp->timeout_ms = clib_host_to_net_u32 (s->timeout_ms);
      rmp->enabled = s->enabled;
      rmp->tx_packets = clib_host_to_net_u64 (s->tx);
      rmp->rx_packets = clib_host_to_net_u64 (s->rx);
      rmp->lost_packets = clib_host_to_net_u64 (s->lost);
      rmp->late_replies = clib_host_to_net_u64 (s->late_replies);
      rmp->last_rtt_us = clib_host_to_net_u32 (
        sr_probe_ms_to_us (s->last_rtt_ms));
      rmp->min_rtt_us = clib_host_to_net_u32 (
        sr_probe_ms_to_us (s->min_rtt_ms));
      rmp->max_rtt_us = clib_host_to_net_u32 (
        sr_probe_ms_to_us (s->max_rtt_ms));
      rmp->avg_rtt_us = clib_host_to_net_u32 (
        sr_probe_ms_to_us (s->n_rtt ? s->sum_rtt_ms / s->n_rtt : 0));
      rmp->jitter_us = clib_host_to_net_u32 (
        sr_probe_ms_to_us (s->jitter_ms));
      rmp->loss_permille = clib_host_to_net_u32 (
        sr_probe_loss_permille (s));

      vl_api_send_msg (reg, (u8 *) rmp);
    }
  /* *INDENT-ON* */
}

static void
vl_api_sr_wg_probe_send_t_handler (vl_api_sr_wg_probe_send_t *mp)
{
  vl_api_sr_wg_probe_send_reply_t *rmp;
  sr_wg_main_t *sm = &sr_wg_main;
  u32 sw_if_index = clib_net_to_host_u32 (mp->sw_if_index);
  int rv = 0;

  if (!sr_probe_send_now (&sm->probe, vlib_get_main (), sw_if_index))
    rv = VNET_API_ERROR_NO_SUCH_ENTRY;

  REPLY_MACRO (VL_API_SR_WG_PROBE_SEND_REPLY);
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
