/*
 * sr_wg_probe.c - 隧道内质量探针：定时发送 / 对端折返 / RTT·抖动·丢包
 *
 * 两个 graph 实体：
 *   sr-wg-probe-process（PROCESS）  按每会话 interval 造包，注入 ip4-lookup；
 *                                   同时把超时未回包的窗口槽判丢。
 *   sr-wg4-probe（INTERNAL）        UDP 51829 的收包节点：
 *                                     flags 无 REPLY -> 对端折返（对调 IP/UDP
 *                                       源目、盖 t2/t3、交回 ip4-lookup）
 *                                     有 REPLY      -> 本机是发送方，按
 *                                       (tx_sw_if_index, seq) 配对算 RTT。
 *
 * 为什么探针走内层正常路由而不是直接往外层 UDP socket 发：通道上还有
 * SR 封装/中转/解封装，只有让探针成为**一条真实的内层业务流**，量到的
 * RTT 才包含 SR 路径本身的处理。直接在外层打环回只能量 underlay。
 */
#include "sr_wg.h"

#include <vnet/ip/ip4.h>
#include <vnet/udp/udp_packet.h>

/* ==================== Trace / 错误 ==================== */

typedef struct
{
  u32 next_index;
  u32 sw_if_index;
  u32 seq;
  u8 flags;
  u8 is_request;
  u8 error;
} sr_wg4_probe_trace_t;

static u8 *
format_sr_wg4_probe_trace (u8 *s, va_list *args)
{
  CLIB_UNUSED (vlib_main_t * vm) = va_arg (*args, vlib_main_t *);
  CLIB_UNUSED (vlib_node_t * node) = va_arg (*args, vlib_node_t *);
  sr_wg4_probe_trace_t *t = va_arg (*args, sr_wg4_probe_trace_t *);

  s = format (s, "SR-WG-PROBE: %s seq=%u if=%u flags=0x%x err=%u next=%u",
              t->is_request ? "request" : "reply", t->seq, t->sw_if_index,
              t->flags, t->error, t->next_index);
  return s;
}

#define foreach_sr_wg4_probe_error                                      \
  _ (NONE, "no error")                                                  \
  _ (BAD_LEN, "short probe packet")                                     \
  _ (BAD_MAGIC, "bad probe magic")                                      \
  _ (BAD_VER, "unsupported probe version")                              \
  _ (BAD_IP, "bad inner ip4 header (options or truncation)")

typedef enum
{
#define _(sym, str) SR_WG4_PROBE_ERROR_##sym,
  foreach_sr_wg4_probe_error
#undef _
    SR_WG4_PROBE_N_ERROR,
} sr_wg4_probe_error_t;

static char *sr_wg4_probe_error_strings[] = {
#define _(sym, str) str,
  foreach_sr_wg4_probe_error
#undef _
};

typedef enum
{
  SR_WG4_PROBE_NEXT_DROP,
  SR_WG4_PROBE_NEXT_IP4_LOOKUP,
  SR_WG4_PROBE_N_NEXT,
} sr_wg4_probe_next_t;

VLIB_REGISTER_NODE (sr_wg4_probe_node) = {
  .name = "sr-wg4-probe",
  .vector_size = sizeof (u32),
  .format_trace = format_sr_wg4_probe_trace,
  .type = VLIB_NODE_TYPE_INTERNAL,
  .flags = VLIB_NODE_FLAG_TRACE_SUPPORTED,
  .n_errors = SR_WG4_PROBE_N_ERROR,
  .error_strings = sr_wg4_probe_error_strings,
  .n_next_nodes = SR_WG4_PROBE_N_NEXT,
  .next_nodes = {
    [SR_WG4_PROBE_NEXT_DROP] = "error-drop",
    [SR_WG4_PROBE_NEXT_IP4_LOOKUP] = "ip4-lookup",
  },
};

/* ==================== 会话表 ==================== */

void
sr_probe_init (sr_probe_main_t *pm)
{
  clib_memset (pm, 0, sizeof (*pm));
  pm->node_index = sr_wg4_probe_node.index;
  pm->process_node_index = sr_wg_probe_process_node.index;
}

sr_probe_session_t *
sr_probe_get (sr_probe_main_t *pm, u32 sw_if_index)
{
  uword *p;

  if (!pm->session_by_sw_if)
    return NULL;

  p = hash_get (pm->session_by_sw_if, sw_if_index);
  if (!p)
    return NULL;

  return pool_elt_at_index (pm->sessions, p[0]);
}

u32
sr_probe_count (sr_probe_main_t *pm)
{
  return pm->count;
}

static void
sr_probe_reset_stats (sr_probe_session_t *s)
{
  s->seq = 0;
  s->tx = s->rx = s->lost = s->late_replies = 0;
  s->last_rtt_ms = s->prev_rtt_ms = s->min_rtt_ms = s->max_rtt_ms = 0;
  s->sum_rtt_ms = 0;
  s->n_rtt = 0;
  s->jitter_ms = 0;
  s->win_head = 0;
  clib_memset (s->window, 0, sizeof (s->window));
}

int
sr_probe_set (sr_probe_main_t *pm, u32 sw_if_index, u32 peer_ip,
              u32 local_ip, u32 interval_ms, u32 timeout_ms, int is_add)
{
  vnet_main_t *vnm = vnet_get_main ();
  sr_probe_session_t *s;
  uword *p;

  if (sw_if_index > 0xffff)
    return VNET_API_ERROR_INVALID_SW_IF_INDEX;

  p = pm->session_by_sw_if ? hash_get (pm->session_by_sw_if, sw_if_index) : 0;

  if (!is_add)
    {
      /* 删不存在的会话是幂等的：控制器对账时不必先 dump */
      if (!p)
        return 0;
      s = pool_elt_at_index (pm->sessions, p[0]);
      hash_unset (pm->session_by_sw_if, sw_if_index);
      pool_put (pm->sessions, s);
      pm->count--;
      return 0;
    }

  if (!vnet_get_sw_interface_or_null (vnm, sw_if_index))
    return VNET_API_ERROR_INVALID_SW_IF_INDEX;
  if (peer_ip == 0)
    return VNET_API_ERROR_INVALID_VALUE;

  if (local_ip == 0)
    {
      /*
       * 不给本地地址就从接口取第一个 IPv4。探针的内层源必须是对端的
       * allowed-ips 能接受、且回程路由可达的地址 —— 正常就是隧道接口上
       * 的那个 overlay /32。取不到就直接报错：源 0.0.0.0 的包会被
       * ip4-lookup 当本地源处理，对端回包也没有回程路由。
       */
      ip_interface_address_t *ia = 0;
      ip4_address_t *a =
        ip4_interface_first_address (&ip4_main, sw_if_index, &ia);
      if (!a)
        return VNET_API_ERROR_ADDRESS_NOT_FOUND_FOR_INTERFACE;
      local_ip = clib_net_to_host_u32 (a->as_u32);
    }

  if (interval_ms == 0)
    interval_ms = SR_PROBE_DEFAULT_INTERVAL_MS;
  if (interval_ms < SR_PROBE_MIN_INTERVAL_MS ||
      interval_ms > SR_PROBE_MAX_INTERVAL_MS)
    return VNET_API_ERROR_INVALID_VALUE;

  if (timeout_ms == 0)
    timeout_ms = interval_ms * SR_PROBE_TIMEOUT_MULT;
  /* 超时短于间隔时窗口的槽会在被回包之前就判丢，纯属自欺 */
  if (timeout_ms < interval_ms)
    return VNET_API_ERROR_INVALID_VALUE;

  if (p)
    {
      s = pool_elt_at_index (pm->sessions, p[0]);
      if (s->peer_ip != peer_ip || s->local_ip != local_ip)
        sr_probe_reset_stats (s);
    }
  else
    {
      pool_get (pm->sessions, s);
      clib_memset (s, 0, sizeof (*s));
      s->sw_if_index = sw_if_index;
      if (!pm->session_by_sw_if)
        pm->session_by_sw_if = hash_create (0, sizeof (uword));
      hash_set (pm->session_by_sw_if, sw_if_index, s - pm->sessions);
      pm->count++;
    }

  s->peer_ip = peer_ip;
  s->local_ip = local_ip;
  s->interval_ms = interval_ms;
  s->timeout_ms = timeout_ms;
  s->enabled = 1;
  s->next_send = vlib_time_now (vlib_get_main ()); /* 下一次 process tick 就发 */

  return 0;
}

/* ==================== 窗口记账 ==================== */

static void
sr_probe_expire (sr_probe_session_t *s, f64 now)
{
  f64 timeout = (f64) s->timeout_ms / 1000.0;
  int i;

  for (i = 0; i < SR_PROBE_WINDOW; i++)
    {
      sr_probe_slot_t *slot = &s->window[i];

      if (slot->state == SR_PROBE_SLOT_OUTSTANDING &&
          now - slot->sent_at > timeout)
        {
          slot->state = SR_PROBE_SLOT_LOST;
          s->lost++;
        }
    }
}

static void
sr_probe_window_put (sr_probe_session_t *s, u32 seq, f64 now)
{
  sr_probe_slot_t *slot = &s->window[s->win_head];

  /* 槽被覆盖时还没回包：按丢处理（expire 可能还没来得及跑） */
  if (slot->state == SR_PROBE_SLOT_OUTSTANDING)
    {
      slot->state = SR_PROBE_SLOT_LOST;
      s->lost++;
    }

  slot->seq = seq;
  slot->sent_at = now;
  slot->state = SR_PROBE_SLOT_OUTSTANDING;
  s->win_head = (s->win_head + 1) % SR_PROBE_WINDOW;
}

/*
 * 从**窗口内**回包算丢包率（千分比）。累计计数器会永远背着历史，
 * 断链恢复后看起来还是"一直丢包"，所以对外报的是窗口值；
 * 累计 tx/rx/lost 仍然给出，供控制器算长周期趋势。
 */
u32
sr_probe_loss_permille (const sr_probe_session_t *s)
{
  u32 replied = 0, lost = 0;
  int i;

  for (i = 0; i < SR_PROBE_WINDOW; i++)
    {
      if (s->window[i].state == SR_PROBE_SLOT_REPLIED)
        replied++;
      else if (s->window[i].state == SR_PROBE_SLOT_LOST)
        lost++;
    }

  if (replied + lost == 0)
    return 0;

  return (u32) (((u64) lost * 1000) / (replied + lost));
}

/* ==================== 发送 ==================== */

static void
sr_probe_send_one (vlib_main_t *vm, sr_probe_session_t *s, f64 now)
{
  sr_wg_main_t *sm = &sr_wg_main;
  sr_probe_main_t *pm = &sm->probe;
  vlib_buffer_t *b;
  vlib_frame_t *f;
  u32 *to_next;
  u32 bi;
  u8 *p;
  ip4_header_t *ip;
  udp_header_t *udp;
  sr_probe_hdr_t *ph;
  u64 now_ns = (u64) (now * 1e9);

  /*
   * 接口被删掉后 FIB 里指向它的路由也没了，包会停在 ip4-lookup 的 dpo-drop
   * （不碰 RX），但仍在这里挡一道，避免任何路径上再用到失效的 sw_if_index。
   */
  if (!vnet_get_sw_interface_or_null (vnet_get_main (), s->sw_if_index))
    return;

  if (vlib_buffer_alloc (vm, &bi, 1) != 1)
    {
      pm->alloc_fail++;
      return;
    }

  b = vlib_get_buffer (vm, bi);
  b->current_data = 0;
  b->current_length = SR_PROBE_PKT_LEN;
  b->flags = 0;
  b->error = 0;

  p = vlib_buffer_get_current (b);
  clib_memset (p, 0, SR_PROBE_PKT_LEN);

  ip = (ip4_header_t *) p;
  udp = (udp_header_t *) (ip + 1);
  ph = (sr_probe_hdr_t *) (udp + 1);

  ip->ip_version_and_header_length = 0x45;
  ip->tos = 0;
  ip->length = clib_host_to_net_u16 (SR_PROBE_PKT_LEN);
  ip->fragment_id = 0;
  ip->flags_and_fragment_offset = 0;
  ip->ttl = 64;
  ip->protocol = IP_PROTOCOL_UDP;
  ip->src_address.as_u32 = clib_host_to_net_u32 (s->local_ip);
  ip->dst_address.as_u32 = clib_host_to_net_u32 (s->peer_ip);
  ip->checksum = ip4_header_checksum (ip);

  udp->src_port = clib_host_to_net_u16 (SR_PROBE_PORT);
  udp->dst_port = clib_host_to_net_u16 (SR_PROBE_PORT);
  udp->length = clib_host_to_net_u16 (8 + SR_PROBE_HDR_LEN);
  udp->checksum = 0; /* IPv4 允许 UDP 校验和为零 */

  ph->magic = clib_host_to_net_u32 (SR_PROBE_MAGIC);
  ph->version = SR_PROBE_VERSION;
  ph->flags = 0;
  ph->tx_sw_if_index = clib_host_to_net_u16 ((u16) s->sw_if_index);
  ph->seq = clib_host_to_net_u32 (s->seq);
  ph->t1_ns = clib_host_to_net_u64 (now_ns);
  ph->t2_ns = 0;
  ph->t3_ns = 0;

  /*
   * RX **必须填一个真实存在的接口**，不能用 ~0：隧道被 admin down 时
   * 这个包会走到 interface-output 的下行分支，再进 interface-drop，而
   * interface_drop_punt 拿 vnet_buffer(b)->sw_if_index[VLIB_RX] 直接当
   * 数组下标去加每接口计数器 —— ~0 就是越界。26.06 的 ip4-lookup 只用
   * RX 找 FIB、不会把 ~0 写回 RX，所以只有这一侧有这个约束（2026-10-24
   * 实测过一次：`set interface state wg0 down` 后 VPP 立刻 SIGSEGV
   * 在 interface_drop_fn_x86_64_v4）。填探针自己的隧道口，既真实（探针
   * 就是从这张口发起的），也让丢包计数落在有意义的地方。
   *
   * TX = ~0 是让 ip4-lookup 去查 **RX 那张接口所在的表**。隧道接口由 agent
   * 显式挪进 overlay 表（`set interface ip table <if> 250`），所以这一下
   * 命中的就是 overlay 表里的对端 /32，探针和业务走同一条封装路径。
   *
   * 这里不能填 0。26.06 的约定是 TX 存的是 **fib_index**（0、1、…），既不是
   * sw_if_index 也不是 VRF id（ip4_forward.c 开头那段 buffer 约定写得很明
   * 白）；填 0 等于点名查 main table。overlay /32 在 main table 里也可能存在
   * （指向 WAN 网关），于是探针从 table 0 走、**不下隧道**：2026-11-05 实测
   * CPE1 上 table 0 那条 10.250.0.2/32 的计数器按探针速率增长（11s 里
   * 37 包 / 3344 字节，对上 probe_tx +43 × 76B），两端 echoed 恒 0、丢包
   * 钉在 100% —— 探针明文漏到了 LAN 上。~0 之外没有第二个能表达"跟着这张
   * 隧道的表走"的写法，插件里拿不到 overlay 表的 fib_index，硬编码 1 会在
   * 表顺序变化时静默查错表。
   */
  vnet_buffer (b)->sw_if_index[VLIB_RX] = s->sw_if_index;
  vnet_buffer (b)->sw_if_index[VLIB_TX] = ~0;

  f = vlib_get_frame_to_node (vm, ip4_lookup_node.index);
  to_next = vlib_frame_vector_args (f);
  to_next[0] = bi;
  f->n_vectors = 1;
  vlib_put_frame_to_node (vm, ip4_lookup_node.index, f);

  sr_probe_window_put (s, s->seq, now);
  s->seq++;
  s->tx++;
}

int
sr_probe_send_now (sr_probe_main_t *pm, vlib_main_t *vm, u32 sw_if_index)
{
  sr_probe_session_t *s = sr_probe_get (pm, sw_if_index);

  if (!s || !s->enabled)
    return 0;

  sr_probe_send_one (vm, s, vlib_time_now (vm));
  return 1;
}

/* ==================== 回包处理 ==================== */

static void
sr_probe_handle_reply (sr_probe_main_t *pm, const sr_probe_hdr_t *h,
                       f64 now, u64 now_ns)
{
  u16 sw_if_index = clib_net_to_host_u16 (h->tx_sw_if_index);
  u32 seq = clib_net_to_host_u32 (h->seq);
  u64 t1 = clib_net_to_host_u64 (h->t1_ns);
  u64 t2 = clib_net_to_host_u64 (h->t2_ns);
  u64 t3 = clib_net_to_host_u64 (h->t3_ns);
  sr_probe_session_t *s = sr_probe_get (pm, sw_if_index);
  int i;

  if (!s || !s->enabled)
    {
      pm->unknown_replies++;
      return;
    }

  /* 在窗口里找这条 outstanding 的 seq；找不到就是迟到（早已判丢）或伪造 */
  for (i = 0; i < SR_PROBE_WINDOW; i++)
    {
      sr_probe_slot_t *slot = &s->window[i];
      f64 rtt_ms;

      if (slot->state != SR_PROBE_SLOT_OUTSTANDING || slot->seq != seq)
        continue;

      slot->state = SR_PROBE_SLOT_REPLIED;
      s->rx++;

      /* 时钟异常（对端没盖 t2/t3、t3<t2、t4<=t1）时只记 RTT 计数外的回包 */
      if (t1 == 0 || t2 == 0 || t3 < t2 || now_ns <= t1)
        return;

      rtt_ms = ((f64) (now_ns - t1) - (f64) (t3 - t2)) / 1e6;
      if (rtt_ms < 0)
        rtt_ms = 0;

      s->last_rtt_ms = rtt_ms;
      if (s->n_rtt == 0 || rtt_ms < s->min_rtt_ms)
        s->min_rtt_ms = rtt_ms;
      if (s->n_rtt == 0 || rtt_ms > s->max_rtt_ms)
        s->max_rtt_ms = rtt_ms;
      s->sum_rtt_ms += rtt_ms;
      s->n_rtt++;

      if (s->n_rtt > 1)
        {
          f64 d = rtt_ms - s->prev_rtt_ms;
          if (d < 0)
            d = -d;
          /* RFC3550 式 EWMA（增益 1/16），对单次抖动尖峰不敏感 */
          s->jitter_ms += (d - s->jitter_ms) / 16.0;
        }
      s->prev_rtt_ms = rtt_ms;

      sr_probe_expire (s, now);
      return;
    }

  s->late_replies++;
}

/* ==================== 收包节点 ==================== */

VLIB_NODE_FN (sr_wg4_probe_node) (vlib_main_t *vm, vlib_node_runtime_t *node,
                                  vlib_frame_t *frame)
{
  u32 n_left = frame->n_vectors;
  u32 *from = vlib_frame_vector_args (frame);
  vlib_buffer_t *bufs[VLIB_FRAME_SIZE];
  u16 nexts[VLIB_FRAME_SIZE];
  u32 to_free[VLIB_FRAME_SIZE];
  vlib_buffer_t **buf_ptr = bufs;
  u32 *bi_ptr = from;
  u32 n_to_next = 0;
  u32 n_to_free = 0;
  sr_wg_main_t *sm = &sr_wg_main;
  f64 now = vlib_time_now (vm);
  u64 now_ns = (u64) (now * 1e9);

  vlib_get_buffers (vm, from, bufs, n_left);

  while (n_left > 0)
    {
      u32 bi = *bi_ptr;
      vlib_buffer_t *b = buf_ptr[0];
      sr_probe_hdr_t *h = (sr_probe_hdr_t *) vlib_buffer_get_current (b);
      u32 next_index = SR_WG4_PROBE_NEXT_DROP;
      u32 seq = 0;
      u8 flags = 0;
      u8 is_request = 0;
      u8 is_reply = 0;
      u8 error = SR_WG4_PROBE_ERROR_NONE;

      if (PREDICT_FALSE (b->current_length < SR_PROBE_HDR_LEN))
        {
          error = SR_WG4_PROBE_ERROR_BAD_LEN;
          goto done;
        }
      if (PREDICT_FALSE (clib_net_to_host_u32 (h->magic) != SR_PROBE_MAGIC))
        {
          error = SR_WG4_PROBE_ERROR_BAD_MAGIC;
          goto done;
        }
      if (PREDICT_FALSE (h->version != SR_PROBE_VERSION))
        {
          error = SR_WG4_PROBE_ERROR_BAD_VER;
          goto done;
        }

      flags = h->flags;
      seq = clib_net_to_host_u32 (h->seq);

      if (flags & SR_PROBE_FLAG_REPLY)
        {
          /*
           * 本机是发送方：配对、算指标，包到此为止。
           *
           * 这里**直接 free**，不走 error-drop：error-drop 的终点是
           * null-node，会让 `null-node blackholed` 以探针速率增长 ——
           * 那个计数器是 allowed-ips 配置错误（#26）的信号，不能被正常
           * 的探针回包污染。
           */
          sr_probe_handle_reply (&sm->probe, h, now, now_ns);
          is_reply = 1;
          goto done;
        }

      /*
       * 本机是对端：折返。
       *
       * udp-local 把 current_data 推过了内层 IP+UDP 头（和我们自己的
       * sr-wg4-transit 一样按 28 字节回退；本插件生成的探针没有 IPv4
       * 选项，回退后校验 0x45，带选项的一律当坏包丢掉）。
       */
      if (PREDICT_FALSE (b->current_data <
                         sizeof (ip4_header_t) + sizeof (udp_header_t)))
        {
          error = SR_WG4_PROBE_ERROR_BAD_LEN;
          goto done;
        }

      vlib_buffer_advance (b, -(int) (sizeof (ip4_header_t) +
                                      sizeof (udp_header_t)));

      {
        ip4_header_t *ip = (ip4_header_t *) vlib_buffer_get_current (b);
        udp_header_t *udp = (udp_header_t *) (ip + 1);

        if (PREDICT_FALSE (ip->ip_version_and_header_length != 0x45) ||
            PREDICT_FALSE (clib_net_to_host_u16 (ip->length) !=
                           SR_PROBE_PKT_LEN) ||
            PREDICT_FALSE (clib_net_to_host_u16 (udp->dst_port) !=
                           SR_PROBE_PORT))
          {
            error = SR_WG4_PROBE_ERROR_BAD_IP;
            goto done;
          }

        {
          u32 tmp = ip->src_address.as_u32;
          ip->src_address.as_u32 = ip->dst_address.as_u32;
          ip->dst_address.as_u32 = tmp;
        }
        {
          u16 tmp = udp->src_port;
          udp->src_port = udp->dst_port;
          udp->dst_port = tmp;
        }

        h->flags = SR_PROBE_FLAG_REPLY;
        h->t2_ns = clib_host_to_net_u64 (now_ns);
        h->t3_ns = clib_host_to_net_u64 (now_ns);

        ip->checksum = 0;
        ip->checksum = ip4_header_checksum (ip);
        udp->checksum = 0; /* IPv4 允许 UDP 校验和为零 */

        sm->probe.echoed++;
        is_request = 1;
        next_index = SR_WG4_PROBE_NEXT_IP4_LOOKUP;
      }

    done:
      b->error = error ? node->errors[error] : 0;

      if (PREDICT_FALSE (b->flags & VLIB_BUFFER_IS_TRACED))
        {
          sr_wg4_probe_trace_t *tr =
            vlib_add_trace (vm, node, b, sizeof (*tr));
          tr->next_index = next_index;
          tr->sw_if_index = vnet_buffer (b)->sw_if_index[VLIB_RX];
          tr->seq = seq;
          tr->flags = flags;
          tr->is_request = is_request;
          tr->error = error;
        }

      /*
       * 回包已消费 -> 直接 free；其余（折返、坏包）照常按 next 发。
       * 两者混在一个 frame 里，所以转发的那部分就地压紧到 from/nexts。
       */
      if (PREDICT_TRUE (is_reply))
        to_free[n_to_free++] = bi;
      else
        {
          from[n_to_next] = bi;
          nexts[n_to_next++] = next_index;
        }

      n_left--;
      buf_ptr++;
      bi_ptr++;
    }

  if (n_to_next)
    vlib_buffer_enqueue_to_next (vm, node, from, nexts, n_to_next);
  if (n_to_free)
    vlib_buffer_free (vm, to_free, n_to_free);

  return frame->n_vectors;
}

/* ==================== 定时发送 process ==================== */

static uword
sr_wg_probe_process (vlib_main_t *vm, vlib_node_runtime_t *node,
                     vlib_frame_t *frame)
{
  sr_wg_main_t *sm = &sr_wg_main;
  sr_probe_main_t *pm = &sm->probe;

  while (1)
    {
      f64 now = vlib_time_now (vm);
      f64 next_wake = now + 1.0; /* 会话表变动最多 1s 内被看到 */
      sr_probe_session_t *s;
      u32 i;

      /* *INDENT-OFF* */
      pool_foreach_index (i, pm->sessions)
        {
          s = pool_elt_at_index (pm->sessions, i);
          if (!s->enabled)
            continue;

          sr_probe_expire (s, now);

          /* 主线程长时间被占后不要补发一批历史探针 */
          if (s->next_send + 1.0 < now)
            s->next_send = now;

          while (now >= s->next_send)
            {
              sr_probe_send_one (vm, s, now);
              s->next_send += (f64) s->interval_ms / 1000.0;
            }

          if (s->next_send < next_wake)
            next_wake = s->next_send;
        }
      /* *INDENT-ON* */

      if (next_wake <= now)
        next_wake = now + 0.001;
      vlib_process_suspend (vm, next_wake - now);
    }

  return 0;
}

VLIB_REGISTER_NODE (sr_wg_probe_process_node) = {
  .name = "sr-wg-probe-process",
  .type = VLIB_NODE_TYPE_PROCESS,
  /* 26.06 的 node 注册只有一个 function 字段；process 节点也填在 .function
   * （老版本里的 .process 字段已删除） */
  .function = sr_wg_probe_process,
};
