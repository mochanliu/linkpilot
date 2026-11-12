/*
 * sr_qos.c - 隧道出口 QoS 节点（第一阶段：分类骨架 + 挂点验证）
 *
 * 设计边界见 sr_qos.h 头注。这里只强调一条实现约束：
 * 本节点挂在 ip4-output 弧、且排在 wg4-output-tun **之前**，所以
 * vlib_buffer_get_current(b) 指向的是**内层 IP4 头**（明文），
 * 此时既没加密也没加外层头 —— 分类、看 DSCP、按隧道计数都在这一步做。
 * 千万不要在图里假设外层头存在。
 */
#include "sr_qos.h"

#include <vlib/vlib.h>
#include <vnet/feature/feature.h>
#include <vnet/interface_funcs.h>
#include <vnet/ip/ip4_packet.h>
#include <vnet/ip/format.h>

sr_qos_main_t sr_qos_main;

/* ==================== Node ==================== */

VLIB_NODE_FN (sr_wg4_qos_node) (vlib_main_t *vm, vlib_node_runtime_t *node,
                                vlib_frame_t *frame)
{
  sr_qos_main_t *qm = &sr_qos_main;
  u32 n_left = frame->n_vectors;
  u32 *from = vlib_frame_vector_args (frame);
  vlib_buffer_t *bufs[VLIB_FRAME_SIZE];
  u16 nexts[VLIB_FRAME_SIZE];

  vlib_get_buffers (vm, from, bufs, n_left);

  for (u32 i = 0; i < n_left; i++)
    {
      vlib_buffer_t *b = bufs[i];
      u32 sw_if_index = vnet_buffer (b)->sw_if_index[VLIB_TX];
      ip4_header_t *ip4 = vlib_buffer_get_current (b);
      u8 dscp = 0;
      sr_qos_ifc_t *ic;

      /* 防御：万一弧上出现非 IP4（历史上 l2 旁路包进过 ip4-output），
       * 不猜、按 0 类计数并原样放行 —— 分类器出错可以查，改包不行。 */
      if (ip4->ip_version_and_header_length >> 4 == 4)
        dscp = (ip4->tos >> 2) & 0x3f;

      vec_validate (qm->ifs, sw_if_index);
      ic = vec_elt_at_index (qm->ifs, sw_if_index);
      ic->pkts[dscp]++;
      ic->bytes[dscp] += b->current_length;
      ic->total++;
      qm->total_pkts++;

      /* 继续走弧：下一个 feature 就是 wg4-output-tun（加密）。
       * 注意 vnet_feature_next 的形参是 u32*（它按 u32 写），
       * 直接传 &nexts[i] 会写 4 字节进 2 字节的槽 —— 踩栈，必须过 u32。 */
      u32 next0 = 0;
      vnet_feature_next (&next0, b);
      nexts[i] = (u16) next0;
    }

  vlib_buffer_enqueue_to_next (vm, node, from, nexts, n_left);
  return n_left;
}

VLIB_REGISTER_NODE (sr_wg4_qos_node) = {
  .name = SR_QOS_NODE,
  .vector_size = sizeof (u32),
  .type = VLIB_NODE_TYPE_INTERNAL,
  .n_next_nodes = 1,
  .next_nodes = {
    [0] = "error-drop",
  },
};

/*
 * 挂点声明。runs_before 只能是 wg4-output-tun：
 *   - 排在它之前 → 明文、可分类、可排队（本节点要的位置）；
 *   - 排在它之后 → 死点（wg4-output-tun 硬编码 next，不调 vnet_feature_next）；
 *   - runs_before interface-output → 等价于"排在它之后"，同样死。
 */
VNET_FEATURE_INIT (sr_wg4_qos_feat, static) = {
  .arc_name = SR_QOS_ARC,
  .node_name = SR_QOS_NODE,
  .runs_before = VNET_FEATURES (SR_QOS_PRE_NODE),
};

/* ==================== 启用状态 ==================== */

/*
 * 影子位图：只记录我们自己成功调用过 enable 的接口。
 * 不读 VPP 内部 feature config —— 那份结构的读取接口在插件间不稳定，
 * 而本插件是唯一会 enable 这个 feature 的人，且 init 时清零，
 * 所以影子与真实状态在"同一次 VPP 生命周期"内等价。
 */
static uword *sr_qos_enabled;

int
sr_qos_hook_enabled (u32 sw_if_index)
{
  return sr_qos_enabled && clib_bitmap_get (sr_qos_enabled, sw_if_index);
}

int
sr_qos_hook_enable_disable (u32 sw_if_index, int enable)
{
  int rv;

  rv = vnet_feature_enable_disable (SR_QOS_ARC, SR_QOS_NODE, sw_if_index,
                                    enable ? 1 : 0, 0, 0);
  if (rv != 0)
    return rv;

  if (enable)
    {
      if (!clib_bitmap_get (sr_qos_enabled, sw_if_index))
        sr_qos_main.enabled_count++;
      sr_qos_enabled = clib_bitmap_set (sr_qos_enabled, sw_if_index, 1);
    }
  else
    {
      if (clib_bitmap_get (sr_qos_enabled, sw_if_index))
        sr_qos_main.enabled_count--;
      sr_qos_enabled = clib_bitmap_set (sr_qos_enabled, sw_if_index, 0);
    }

  return 0;
}

/* ==================== CLI ==================== */

static clib_error_t *
sr_wg_set_qos_hook_command_fn (vlib_main_t *vm, unformat_input_t *input,
                               vlib_cli_command_t *cmd)
{
  vnet_main_t *vnm = vnet_get_main ();
  u32 sw_if_index = (u32) ~0;
  u8 enable = 0, disable = 0;
  int rv;

  while (unformat_check_input (input) != UNFORMAT_END_OF_INPUT)
    {
      if (unformat (input, "enable"))
        enable = 1;
      else if (unformat (input, "disable"))
        disable = 1;
      else if (unformat (input, "%u", &sw_if_index))
        ;
      else if (unformat (input, "%U", unformat_vnet_sw_interface, vnm,
                         &sw_if_index))
        ;
      else
        break;
    }

  if (enable == disable)
    return clib_error_return (0, "specify 'enable' or 'disable'");
  if (sw_if_index == (u32) ~0)
    return clib_error_return (0, "missing <interface>");

  rv = sr_qos_hook_enable_disable (sw_if_index, enable ? 1 : 0);
  if (rv != 0)
    return clib_error_return (0,
                              "failed to %s %s on %U (rv=%d) —— "
                              "该接口可能不在 %s 弧上",
                              enable ? "enable" : "disable", SR_QOS_NODE,
                              format_vnet_sw_if_index_name, vnm, sw_if_index,
                              rv, SR_QOS_ARC);

  vlib_cli_output (vm, "%s %s on %U (sw_if_index=%u), 排在 %s 之前。",
                   enable ? "Enabled" : "Disabled", SR_QOS_NODE,
                   format_vnet_sw_if_index_name, vnm, sw_if_index, sw_if_index,
                   SR_QOS_PRE_NODE);
  return 0;
}

VLIB_CLI_COMMAND (sr_wg_set_qos_hook_command, static) = {
  .path = "set sr-wg qos-hook",
  .short_help = "set sr-wg qos-hook enable|disable <interface>",
  .function = sr_wg_set_qos_hook_command_fn,
};

static clib_error_t *
sr_wg_show_qos_hook_command_fn (vlib_main_t *vm, unformat_input_t *input,
                                vlib_cli_command_t *cmd)
{
  sr_qos_main_t *qm = &sr_qos_main;
  vnet_main_t *vnm = vnet_get_main ();
  uword sw_if_index;

  vlib_cli_output (vm, "节点 %s：弧 %s，排在 %s 之前；累计 %llu 包，挂在 %u 个口。",
                   SR_QOS_NODE, SR_QOS_ARC, SR_QOS_PRE_NODE,
                   (unsigned long long) qm->total_pkts, qm->enabled_count);

  if (qm->enabled_count == 0)
    vlib_cli_output (vm, "  !! 未启用：`set sr-wg qos-hook enable <interface>`");

  /* *INDENT-OFF* */
  clib_bitmap_foreach (sw_if_index, sr_qos_enabled)
    {
      sr_qos_ifc_t *ic;
      if (sw_if_index >= vec_len (qm->ifs))
        continue;
      ic = vec_elt_at_index (qm->ifs, sw_if_index);
      vlib_cli_output (vm, "  %U: total=%llu",
                       format_vnet_sw_if_index_name, vnm, (u32) sw_if_index,
                       (unsigned long long) ic->total);
      for (u32 d = 0; d < SR_QOS_N_DSCP; d++)
        if (ic->pkts[d])
          vlib_cli_output (vm, "    dscp %2u: %llu pkt / %llu B", d,
                           (unsigned long long) ic->pkts[d],
                           (unsigned long long) ic->bytes[d]);
    }
  /* *INDENT-ON* */

  return 0;
}

VLIB_CLI_COMMAND (sr_wg_show_qos_hook_command, static) = {
  .path = "show sr-wg qos-hook",
  .short_help = "show sr-wg qos-hook   （挂点计数，按隧道与 DSCP 分桶）",
  .function = sr_wg_show_qos_hook_command_fn,
};

/* ==================== Init ==================== */

static clib_error_t *
sr_qos_init (vlib_main_t *vm)
{
  clib_memset (&sr_qos_main, 0, sizeof (sr_qos_main));
  clib_bitmap_alloc (sr_qos_enabled, 1024);
  return 0;
}

VLIB_INIT_FUNCTION (sr_qos_init);
