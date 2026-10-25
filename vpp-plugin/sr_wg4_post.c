/*
 * sr_wg4_post.c - WireGuard 解密后的报文类型分派（MPLS 入口垫片）
 *
 * 为什么需要这个节点：
 *
 *   VPP 里 MPLS 报文的正常入口是 ethernet-input 按 ethertype 0x8847 交给
 *   mpls-input。WireGuard 解密后的内层报文**不经过 ethernet-input**：
 *   wg4-input 自己按载荷首字节的 IP 版本号分派（wireguard_input.c）：
 *     nibble 4 -> ip4-input-no-checksum   (slot 2)
 *     其它     -> ip6-input               (slot 3)
 *   MPLS 标签栈首字节的高 4 位是标签高 4 位（标签 16..255 全为 0x0），
 *   于是 MPLS 包被当成 IPv6 送进 ip6-input，按版本号不符丢弃。
 *
 * 本节点的做法：
 *
 *   注册 sr-wg4-post，在 VLIB_INIT_FUNCTION 里把 wg4-input 的 slot 3 从
 *   ip6-input 换成自己（运行时改图，vlib_node_add_next_with_slot）。之后：
 *     nibble 0 且长度 >= 4 -> mpls-input
 *     nibble 4             -> ip4-input-no-checksum
 *     nibble 6             -> ip6-input
 *     其它                 -> error-drop
 *   IPv6 行为与改图前完全一致（同一个目标节点），MPLS 则拿到与
 *   ethernet-input 之后等价的入口。不修改、不重编译 wireguard 插件。
 *
 *   为什么按 nibble 判 MPLS：解密后的缓冲区里没有 ethernet 头，MPLS 也没有
 *   版本字段。业界都有显式类型位（IPsec 用 ESP Next Header=137、GRE 用
 *   protocol 0x8847、TUN 的 PI 模式用 skb->protocol），而 WireGuard 报文格式
 *   里没有这类字段 —— 这正是它原生不支持 MPLS 的原因。标签 16..255 的高
 *   4 位恒为 0，与 IPv4(4)/IPv6(6) 无歧义。
 *
 * 两个前置条件（由配置面负责，缺一不可）：
 *
 *   1. peer allowed-ips 必须含 ::/0。wg4-input 在分派**之前**做 allowed-ips
 *      源地址检查，非 IPv4 的包被它当 IPv6 读 8..24 字节（对 MPLS 包而言
 *      是标签栈之后的载荷）。只配 0.0.0.0/0 时 MPLS 包在这里就 error-punt，
 *      根本到不了本节点。allowed-ips 只做源过滤、不参与选路。
 *
 *   2. `set interface mpls <wg> enable`。mpls-input 用
 *      sw_if_index[VLIB_RX]（wg4-input 已设为 peer 的 wg 接口）判断该接口
 *      是否开了 MPLS，未开时直接给 mpls-not-enabled；同一条命令还会把接口
 *      MPLS MTU 从 0 设起来，否则大包过不了 MTU 检查。
 */
#include "sr_wg.h"

/* ==================== Trace 定义 ==================== */

typedef struct {
  u32 next_index;
  u32 label;    /* 仅 MPLS 分支有意义：标签栈栈顶 */
  u8  nibble;
  u8  error;
} sr_wg4_post_trace_t;

static u8 *
format_sr_wg4_post_trace (u8 *s, va_list *args)
{
  CLIB_UNUSED (vlib_main_t * vm) = va_arg (*args, vlib_main_t *);
  CLIB_UNUSED (vlib_node_t * node) = va_arg (*args, vlib_node_t *);
  sr_wg4_post_trace_t *t = va_arg (*args, sr_wg4_post_trace_t *);

  if (t->error)
    s = format (s, "SR-WG4-POST: nibble=0x%x err=%u next=%u", t->nibble,
                t->error, t->next_index);
  else if (t->nibble == 0x0)
    s = format (s, "SR-WG4-POST: mpls label=%u next=%u", t->label,
                t->next_index);
  else
    s = format (s, "SR-WG4-POST: nibble=0x%x next=%u", t->nibble,
                t->next_index);
  return s;
}

/* ==================== 错误枚举 ==================== */

#define foreach_sr_wg4_post_error                                        \
  _ (NONE, "no error")                                                   \
  _ (BAD_NIBBLE, "payload is neither MPLS nor IPv4/IPv6")                \
  _ (TRUNCATED, "MPLS label stack truncated")

typedef enum {
#define _(sym, str) SR_WG4_POST_ERROR_##sym,
  foreach_sr_wg4_post_error
#undef _
    SR_WG4_POST_N_ERROR,
} sr_wg4_post_error_t;

static char *sr_wg4_post_error_strings[] = {
#define _(sym, str) str,
  foreach_sr_wg4_post_error
#undef _
};

/* ==================== Next nodes ==================== */

typedef enum {
  SR_WG4_POST_NEXT_DROP,
  SR_WG4_POST_NEXT_MPLS,
  SR_WG4_POST_NEXT_IP4,
  SR_WG4_POST_NEXT_IP6,
  SR_WG4_POST_N_NEXT,
} sr_wg4_post_next_t;

/* ==================== Node 注册 ==================== */

VLIB_REGISTER_NODE (sr_wg4_post_node) = {
  .name = "sr-wg4-post",
  .vector_size = sizeof (u32),
  .format_trace = format_sr_wg4_post_trace,
  .type = VLIB_NODE_TYPE_INTERNAL,
  .flags = VLIB_NODE_FLAG_TRACE_SUPPORTED,
  .n_errors = SR_WG4_POST_N_ERROR,
  .error_strings = sr_wg4_post_error_strings,
  .n_next_nodes = SR_WG4_POST_N_NEXT,
  .next_nodes = {
    [SR_WG4_POST_NEXT_DROP] = "error-drop",
    [SR_WG4_POST_NEXT_MPLS] = "mpls-input",
    [SR_WG4_POST_NEXT_IP4] = "ip4-input-no-checksum",
    [SR_WG4_POST_NEXT_IP6] = "ip6-input",
  },
};

/* ==================== Node Function ==================== */

VLIB_NODE_FN (sr_wg4_post_node) (vlib_main_t *vm, vlib_node_runtime_t *node,
                                 vlib_frame_t *frame)
{
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
      u8 *p = vlib_buffer_get_current (b);
      u8 nibble = PREDICT_TRUE (b->current_length > 0) ? (p[0] >> 4) : 0xff;
      u32 label = 0;
      u32 next_index = SR_WG4_POST_NEXT_DROP;
      u8 error = SR_WG4_POST_ERROR_NONE;

      switch (nibble)
        {
        case 0x0:
          /* MPLS label stack: 20-bit label | 3-bit TC | 1-bit S */
          if (PREDICT_FALSE (b->current_length < sizeof (u32)))
            {
              error = SR_WG4_POST_ERROR_TRUNCATED;
              break;
            }
          label = clib_net_to_host_u32 (*(u32 *) p) >> 12;
          next_index = SR_WG4_POST_NEXT_MPLS;
          break;
        case 0x4:
          next_index = SR_WG4_POST_NEXT_IP4;
          break;
        case 0x6:
          next_index = SR_WG4_POST_NEXT_IP6;
          break;
        default:
          error = SR_WG4_POST_ERROR_BAD_NIBBLE;
          break;
        }

      b->error = error ? node->errors[error] : 0;

      if (PREDICT_FALSE (b->flags & VLIB_BUFFER_IS_TRACED))
        {
          sr_wg4_post_trace_t *tr =
            vlib_add_trace (vm, node, b, sizeof (*tr));
          tr->next_index = next_index;
          tr->label = label;
          tr->nibble = nibble;
          tr->error = error;
        }

      next_ptr[0] = next_index;

      n_left--;
      buf_ptr++;
      next_ptr++;
    }

  vlib_buffer_enqueue_to_next (vm, node, from, nexts, frame->n_vectors);
  return frame->n_vectors;
}

/* ==================== 运行时改图：wg4-input slot 3 ==================== */

/*
 * 槽位号来自 wireguard_input.c 的 wg_input_next_t：
 *   0 handshake-handoff, 1 data-handoff, 2 ip4-input-no-checksum,
 *   3 ip6-input, 4 error-punt, 5 error-drop, 6 crypto-enq
 * 只接管 3 号槽（"不是 IPv4" 的那一支）。IPv4 继续走 2 号槽，不经过本节点，
 * 热路径上零额外开销。
 */
#define SR_WG4_POST_WG_SLOT 3

static clib_error_t *
sr_wg4_post_init (vlib_main_t *vm)
{
  vlib_node_t *wg, *post, *ip6, *cur;

  wg = vlib_get_node_by_name (vm, (u8 *) "wg4-input");
  post = vlib_get_node_by_name (vm, (u8 *) "sr-wg4-post");

  if (!post)
    return clib_error_return (0, "sr-wg4-post node is not registered");

  /*
   * wireguard 插件没加载时 MPLS over WireGuard 本就不存在，不阻断 VPP
   * 启动：其余功能（transit/encap/探针）仍要能用。本插件 load_after
   * wireguard_plugin.so，正常配置下这里不会发生。
   */
  if (!wg)
    {
      vlib_log_notice (sr_wg_log.class,
                       "wg4-input not found; sr-wg4-post stays unreachable");
      return 0;
    }

  if (SR_WG4_POST_WG_SLOT >= vec_len (wg->next_nodes))
    return clib_error_return (0,
                              "wg4-input has %u nexts, slot %u missing",
                              vec_len (wg->next_nodes),
                              SR_WG4_POST_WG_SLOT);

  if (wg->next_nodes[SR_WG4_POST_WG_SLOT] == post->index)
    return 0;   /* 已经接好（重复 init，或别的路径先改过） */

  cur = vlib_get_node (vm, wg->next_nodes[SR_WG4_POST_WG_SLOT]);
  ip6 = vlib_get_node_by_name (vm, (u8 *) "ip6-input");
  if (!ip6 || !cur || cur->index != ip6->index)
    {
      /*
       * 上游 wireguard 插件的图变了：slot 3 已不是 ip6-input。硬替换会把
       * 占用者的功能切断，所以保持原图，只报一条告警让人看见 —— 此时
       * MPLS over WireGuard 不通，但常规 WG/IPv4/IPv6 隧道不受影响。
       */
      vlib_log_warn (sr_wg_log.class,
                     "wg4-input slot %u is %s (expected ip6-input); "
                     "not rewiring, MPLS over WireGuard will not work",
                     SR_WG4_POST_WG_SLOT, cur ? (char *) cur->name : "?");
      return 0;
    }

  vlib_node_add_next_with_slot (vm, wg->index, post->index,
                                SR_WG4_POST_WG_SLOT);

  vlib_log_notice (sr_wg_log.class,
                   "wg4-input slot %u rewired: ip6-input -> sr-wg4-post "
                   "(MPLS ingress after WireGuard decrypt)",
                   SR_WG4_POST_WG_SLOT);
  return 0;
}

VLIB_INIT_FUNCTION (sr_wg4_post_init);
