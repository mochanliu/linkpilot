/*
 * sr_wg4_mpls_out.c - MPLS over WireGuard 出口分派垫片
 *
 * 为什么需要这个节点：
 *
 *   VRF 里 `ip route add <pfx> table <t> via <peer> wg0 out-labels <n>`
 *   解出来的 MPLS DPO 链是：
 *     mpls-label push -> "mpls via <peer> wg0" -> mpls-output -> tunnel-output
 *   tunnel-output 只会把隧道口的中间跳（midchain）头拷进包（外层 IP4/UDP），
 *   **不负责加密**。WireGuard 的加密在 wg4-output-tun 里，而它只挂在
 *   IPv4 出口链上（ip4-midchain / ip4-rewrite → 它），MPLS 包的出口链
 *   根本不经过它。
 *
 *   [实测 2026-10-25，node1 发 10.20.0.1]
 *     `show node` 增量：tunnel-output→ip4-load-balance +2，
 *                       wg4-output-tun→adj-midchain-tx +0
 *     egress pcap：外层 IP+UDP 是隧道头，SR 头之后是 16 个 0（WG data
 *       头的位置）紧接着明文 MPLS 标签栈（0x000C8140 = label 200,S=1）
 *       和内层 ICMP —— 加密整个被跳过。
 *   同样的路由若不带标签（普通 IPv4 出口），wg4-output-tun 正常加密。
 *
 * 做法（2026-10-25 定稿，实测 ping 10.10.0.1 <-> 10.20.0.1 双向通）：
 *
 *   把 wg* 接口上 VNET_LINK_MPLS 的邻接的 rewrite_header.next_index 指到
 *   本节点（具体原因见文件后半段「改路」注释：换 mpls-output 的槽会被
 *   vlib_node_add_next_with_slot 按目标节点去重，改不动）。
 *   本节点按邻接的 sw_if_index 分派：
 *     wg*  -> wg4-output-tun（与 IPv4 出口同一条加密路径）
 *     其它 -> tunnel-output（保底，正常不会走到）
 *   只改邻接与图，不改、不重编译 wireguard 插件 —— 与 sr-wg4-post 同思路。
 *
 *   出接口从 MPLS 邻接的 rewrite_header.sw_if_index 取：进入本节点的缓冲区
 *   里 ip.adj_index[VLIB_TX] 就是 "mpls via ... wg0" 邻接。
 *
 *   新路由落地后邻接会先指向 tunnel-output（明文！），看门狗 100ms 内改向；
 *   控制面的 FIB 加载器应在装表后立即调用 `sr-wg mpls-egress` 消除窗口。
 *
 * 同一条 MPLS 隧道口上的 IPv4 流量不受影响：那条路径在 ip4-midchain /
 * ip4-rewrite 就已经进了 wg4-output-tun，不经过本节点。
 */
#include "sr_wg.h"

#include <vnet/adj/adj.h>
#include <vnet/interface.h>

/* ==================== Trace 定义 ==================== */

typedef struct {
  u32 next_index;
  u32 adj_index;
  u32 sw_if_index;
  u8 is_wg;
} sr_wg4_mpls_out_trace_t;

static u8 *
format_sr_wg4_mpls_out_trace (u8 *s, va_list *args)
{
  CLIB_UNUSED (vlib_main_t * vm) = va_arg (*args, vlib_main_t *);
  CLIB_UNUSED (vlib_node_t * node) = va_arg (*args, vlib_node_t *);
  sr_wg4_mpls_out_trace_t *t = va_arg (*args, sr_wg4_mpls_out_trace_t *);

  s = format (s, "SR-WG4-MPLS-OUT: adj=%u sw_if_index=%u %s next=%u",
              t->adj_index, t->sw_if_index, t->is_wg ? "wg" : "tunnel",
              t->next_index);
  return s;
}

/* ==================== 错误枚举 ==================== */

#define foreach_sr_wg4_mpls_out_error                                    \
  _ (NONE, "no error")                                                   \
  _ (BAD_ADJ, "MPLS adj is missing or not a midchain")

typedef enum {
#define _(sym, str) SR_WG4_MPLS_OUT_ERROR_##sym,
  foreach_sr_wg4_mpls_out_error
#undef _
    SR_WG4_MPLS_OUT_N_ERROR,
} sr_wg4_mpls_out_error_t;

static char *sr_wg4_mpls_out_error_strings[] = {
#define _(sym, str) str,
  foreach_sr_wg4_mpls_out_error
#undef _
};

/* ==================== Next nodes ==================== */

typedef enum {
  SR_WG4_MPLS_OUT_NEXT_DROP,
  SR_WG4_MPLS_OUT_NEXT_WG,
  SR_WG4_MPLS_OUT_NEXT_TUNNEL,
  SR_WG4_MPLS_OUT_N_NEXT,
} sr_wg4_mpls_out_next_t;

/* ==================== Node 注册 ==================== */

VLIB_REGISTER_NODE (sr_wg4_mpls_out_node) = {
  .name = "sr-wg4-mpls-out",
  .vector_size = sizeof (u32),
  .format_trace = format_sr_wg4_mpls_out_trace,
  .type = VLIB_NODE_TYPE_INTERNAL,
  .flags = VLIB_NODE_FLAG_TRACE_SUPPORTED,
  .n_errors = SR_WG4_MPLS_OUT_N_ERROR,
  .error_strings = sr_wg4_mpls_out_error_strings,
  .n_next_nodes = SR_WG4_MPLS_OUT_N_NEXT,
  .next_nodes = {
    [SR_WG4_MPLS_OUT_NEXT_DROP] = "error-drop",
    [SR_WG4_MPLS_OUT_NEXT_WG] = "wg4-output-tun",
    [SR_WG4_MPLS_OUT_NEXT_TUNNEL] = "tunnel-output",
  },
};

/*
 * 隧道接口判定：wg 接口由 wireguard 插件创建，名字固定为 "wg<N>"。
 * 按名字判而不是按输出节点判，是因为接口的 l3 输出节点没有公开的只读
 * 字段；wg 前缀由插件自己命名，稳定。
 */
static inline int
sr_wg4_mpls_out_if_is_wg (vnet_main_t *vnm, u32 sw_if_index)
{
  vnet_sw_interface_t *sw;
  vnet_hw_interface_t *hw;

  sw = vnet_get_sw_interface (vnm, sw_if_index);
  if (!sw || sw->hw_if_index == ~0)
    return 0;
  hw = vnet_get_hw_interface (vnm, sw->hw_if_index);
  if (!hw || !hw->name || vec_len (hw->name) < 3)
    return 0;
  return hw->name[0] == 'w' && hw->name[1] == 'g' && hw->name[2] >= '0' &&
         hw->name[2] <= '9';
}

/* ==================== Node Function ==================== */

VLIB_NODE_FN (sr_wg4_mpls_out_node) (vlib_main_t *vm,
                                     vlib_node_runtime_t *node,
                                     vlib_frame_t *frame)
{
  vnet_main_t *vnm = vnet_get_main ();
  u32 n_left = frame->n_vectors;
  u32 *from = vlib_frame_vector_args (frame);
  vlib_buffer_t *bufs[VLIB_FRAME_SIZE];
  u16 nexts[VLIB_FRAME_SIZE];
  vlib_buffer_t **buf_ptr = bufs;
  u16 *next_ptr = nexts;

  vlib_get_buffers (vm, from, bufs, n_left);

  while (n_left > 0)
    {
      vlib_buffer_t *b = buf_ptr[0];
      u32 ai = vnet_buffer (b)->ip.adj_index[VLIB_TX];
      u32 sw_if_index = ~0;
      u8 error = SR_WG4_MPLS_OUT_ERROR_NONE;
      u16 next_index = SR_WG4_MPLS_OUT_NEXT_TUNNEL;
      int is_wg = 0;

      if (PREDICT_FALSE (!adj_is_valid (ai)))
        {
          error = SR_WG4_MPLS_OUT_ERROR_BAD_ADJ;
          next_index = SR_WG4_MPLS_OUT_NEXT_DROP;
        }
      else
        {
          ip_adjacency_t *adj = adj_get (ai);
          sw_if_index = adj->rewrite_header.sw_if_index;
          is_wg = sr_wg4_mpls_out_if_is_wg (vnm, sw_if_index);
          if (is_wg)
            {
              /* wg4-output-tun 与 IPv4 出口一样按 TX 接口找隧道。 */
              vnet_buffer (b)->sw_if_index[VLIB_TX] = sw_if_index;
              next_index = SR_WG4_MPLS_OUT_NEXT_WG;
            }
        }

      b->error = error ? node->errors[error] : 0;

      if (PREDICT_FALSE (b->flags & VLIB_BUFFER_IS_TRACED))
        {
          sr_wg4_mpls_out_trace_t *tr =
            vlib_add_trace (vm, node, b, sizeof (*tr));
          tr->next_index = next_index;
          tr->adj_index = ai;
          tr->sw_if_index = sw_if_index;
          tr->is_wg = is_wg;
        }

      next_ptr[0] = next_index;

      n_left--;
      buf_ptr++;
      next_ptr++;
    }

  vlib_buffer_enqueue_to_next (vm, node, from, nexts, frame->n_vectors);
  return frame->n_vectors;
}

/* ==================== 改路：把 WG 接口上的 MPLS 邻接接到本节点 ==================== */

/*
 * 为什么改邻接而不是改 mpls-output 的 next 槽：
 *
 *   vlib_node_add_next_with_slot() 按**目标节点去重**：该节点已是本节点的
 *   某个 next 时直接返回既有槽，请求的槽号被忽略。实测（node1，2026-10-25）：
 *     raw replace slot 3: ret=2 now=502    # 本节点在槽2，槽3 原样不动
 *   而 mpls-output 注册时只有 2 个静态槽（drop/frag），tunnel-output 那一槽
 *   是**每条 MPLS 隧道邻接落地时动态追加**的。占了隧道槽，之后每加一条隧道
 *   就再追加一个 tunnel-output 槽 —— 换槽既会被去重拒绝，也永远追不上新槽。
 *
 * 改为改邻接自己的 rewrite_header.next_index，指到本节点在「使用该邻接的
 * 节点」（ia_node_index，MPLS 隧道邻接是 mpls-midchain）里的 next 槽。
 * 邻接数据直接写，没有去重问题；槽由 vlib_node_add_next() 建一次并固定。
 *
 * 只碰 VNET_LINK_MPLS：同一 wg 接口上的 IPv4 邻接走 wg4-output-tun 的标准
 * 加密路径，不能动。
 *
 * 思科的做法与此一致：MPLS 标签先压好，再由隧道口加密（Cisco SD-WAN 的
 * overlay 就是 UDP/外层IP/ESP/GRE/标签），加密是隧道出口的一个阶段，
 * 不能被绕过。差别只在类型标识：GRE 有 protocol=0x8847，WireGuard 没有，
 * 所以收侧仍需 sr-wg4-post 按 nibble 判 MPLS。
 *
 * 返回改了几条邻接。
 */
typedef struct
{
  vlib_main_t *vm;
  vlib_node_t *self;
  u32 patched;
  int verbose;
} sr_wg4_mpls_out_patch_t;

static adj_walk_rc_t
sr_wg4_mpls_out_patch_adj (adj_index_t ai, void *ctx)
{
  sr_wg4_mpls_out_patch_t *p = ctx;
  ip_adjacency_t *adj = adj_get (ai);
  vlib_node_t *owner;
  u32 slot, before;

  if (adj->ia_link != VNET_LINK_MPLS)
    return ADJ_WALK_RC_CONTINUE;

  owner = vlib_get_node (p->vm, adj->ia_node_index);
  if (!owner || owner->index == p->self->index)
    return ADJ_WALK_RC_CONTINUE;

  slot = vlib_node_get_next (p->vm, owner->index, p->self->index);
  if (slot == ~0)
    slot = vlib_node_add_next (p->vm, owner->index, p->self->index);

  before = adj->rewrite_header.next_index;
  if (p->verbose)
    vlib_cli_output (p->vm, "  adj %u link=mpls owner=%s(%u) sw_if=%u "
                     "before=%u self_slot=%u",
                     ai, owner->name, owner->index,
                     adj->rewrite_header.sw_if_index, before, slot);

  if (slot != before)
    {
      adj->rewrite_header.next_index = slot;
      p->patched++;
      if (p->verbose)
        vlib_cli_output (p->vm, "    -> written, readback=%u",
                         adj->rewrite_header.next_index);
    }
  return ADJ_WALK_RC_CONTINUE;
}

static u32
sr_wg4_mpls_out_apply_ex (vlib_main_t *vm, int verbose)
{
  vnet_main_t *vnm = vnet_get_main ();
  vlib_node_t *self;
  vnet_sw_interface_t *sw;
  sr_wg4_mpls_out_patch_t p = { .vm = vm, .patched = 0, .verbose = verbose };

  self = vlib_get_node_by_name (vm, (u8 *) "sr-wg4-mpls-out");
  if (!self)
    return 0;
  p.self = self;

  pool_foreach (sw, vnm->interface_main.sw_interfaces)
    if (sr_wg4_mpls_out_if_is_wg (vnm, sw->sw_if_index))
      adj_walk (sw->sw_if_index, sr_wg4_mpls_out_patch_adj, &p);
  return p.patched;
}

static u32
sr_wg4_mpls_out_apply (vlib_main_t *vm)
{
  return sr_wg4_mpls_out_apply_ex (vm, 0);
}

static clib_error_t *
sr_wg4_mpls_out_init (vlib_main_t *vm)
{
  if (!vlib_get_node_by_name (vm, (u8 *) "sr-wg4-mpls-out"))
    return clib_error_return (0, "sr-wg4-mpls-out node is not registered");

  /*
   * 此时还没有 MPLS 隧道邻接，通常是 0；看门狗每秒再扫一次，
   * 路由一落地就会把邻接指过来。
   */
  if (sr_wg4_mpls_out_apply (vm))
    vlib_log_notice (sr_wg_log.class,
                     "sr-wg4-mpls-out: MPLS adjacency(ies) repointed");
  return 0;
}

/* 看门狗：邻接是 VPP 在路由落地时建的，接上之前每秒扫一次。 */
static uword
sr_wg4_mpls_out_process (vlib_main_t *vm, vlib_node_runtime_t *node,
                         vlib_frame_t *frame)
{
  CLIB_UNUSED (vlib_node_runtime_t * unused_node) = node;
  CLIB_UNUSED (vlib_frame_t * unused_frame) = frame;

  while (1)
    {
      if (sr_wg4_mpls_out_apply (vm))
        vlib_log_notice (sr_wg_log.class,
                         "sr-wg4-mpls-out: MPLS adjacency(ies) repointed");
      vlib_process_suspend (vm, 0.1);
    }
  return 0;
}

VLIB_REGISTER_NODE (sr_wg4_mpls_out_watch_node) = {
  .name = "sr-wg4-mpls-out-watch",
  .type = VLIB_NODE_TYPE_PROCESS,
  .function = sr_wg4_mpls_out_process,
};

/* 控制面/测试脚本加完 MPLS 路由后可以立刻触发，不必等看门狗下一跳。 */
static clib_error_t *
sr_wg4_mpls_out_cli (vlib_main_t *vm, unformat_input_t *input,
                     vlib_cli_command_t *cmd)
{
  CLIB_UNUSED (unformat_input_t * i) = input;
  CLIB_UNUSED (vlib_cli_command_t * c) = cmd;
  vlib_cli_output (vm, "sr-wg4-mpls-out: %u MPLS adjacency(ies) repointed",
                   sr_wg4_mpls_out_apply_ex (vm, 1));
  return 0;
}

VLIB_CLI_COMMAND (sr_wg4_mpls_out_cli_command) = {
  .path = "sr-wg mpls-egress",
  .short_help = "sr-wg mpls-egress  (mpls-output 隧道出口接到 WG 加密路径)",
  .function = sr_wg4_mpls_out_cli,
};

VLIB_INIT_FUNCTION (sr_wg4_mpls_out_init);
