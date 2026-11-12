/*
 * sr_qos.c - 隧道出口 QoS：分类 + 4 队列调度 + 整形
 *
 * 设计边界见 sr_qos.h 头注。这里只强调三条实现约束：
 *
 * 1) 本节点挂在 **ip4-unicast（入口）弧**，看到的是**明文内层包**：既没加密也没加
 *    外层头，DSCP 在这里读；"出哪条隧道"要自己查一次 FIB（入口弧上还没有出接口）。
 *    为了不被"弧上到底带不带 L2"这种布局细节坑到，sr_qos_ip4() 两种布局都探
 *    （先 current，再 +14），探不到就按 DSCP 0 处理并计 non_ip —— 分类器出错可以
 *    查，改包不行。
 *
 * 2) 令牌桶的单位是**字节**，速率来自配置（shaper_kbps）。shaper_kbps == 0
 *    时整个排队路径不进入，节点退化成"计数 + 透传"。这条不变量保证"先挂钩子"
 *    这个动作永远不掉包。
 *
 * 3) 出队由 process 节点做（VLIB_NODE_TYPE_PROCESS）：包压在队列里，定时把
 *    令牌允许的量通过 vlib_get_frame_to_node/vlib_put_frame_to_node 直接递给
 *    入口弧的终端节点 **ip4-lookup** —— 等于让包把原来的路（查表→重写→加密）
 *    重走一遍。缓冲区元数据（入接口、header）全程没动过，所以不需要任何修补。
 */
#include "sr_qos.h"

#include <vlib/vlib.h>
#include <vnet/feature/feature.h>
#include <vnet/interface_funcs.h>
#include <vnet/ip/ip4_packet.h>
#include <vnet/ip/ip4.h>
#include <vnet/ip/format.h>
#include <vnet/fib/ip4_fib.h>
#include <vnet/adj/adj.h>
#include <vnet/dpo/dpo.h>
#include <vnet/dpo/load_balance.h>
#include <vnet/api_errno.h>

sr_qos_main_t sr_qos_main;

/*
 * process 是否已经跑起来。**故意放在结构体外面**：sr_qos_init 会 memset 整个
 * sr_qos_main，而 process 节点的启动与 init 函数的先后不由我们决定。放在
 * 结构体里就可能被 memset 抹掉，结果是"整形静默失效"——这种失效没人看得出来。
 */
static volatile u32 sr_qos_proc_ready;

/* ==================== 小工具 ==================== */

/*
 * 取内层 IP4 头。ip4-rewrite 在进弧之前可能已经写过 L2（在 underlay 口上确实
 * 如此，见 sr_wg4_encap.c 头注），也可能没有（隧道口这侧）。两种都探一次，
 * 探不到返回 0 —— 调用方按 DSCP 0 处理，绝不按猜测读偏移。
 */
always_inline ip4_header_t *
sr_qos_ip4 (vlib_buffer_t *b)
{
  u8 *p = vlib_buffer_get_current (b);
  u32 len = b->current_length;

  if (PREDICT_TRUE (len >= sizeof (ip4_header_t) && (p[0] >> 4) == 4))
    return (ip4_header_t *) p;
  if (len >= sizeof (ip4_header_t) + 14 && (p[14] >> 4) == 4)
    return (ip4_header_t *) (p + 14);
  return 0;
}

/* 整形按整条链的长度算（af_packet 上通常单缓冲区，但别假设） */
always_inline u32
sr_qos_pkt_len (vlib_main_t *vm, vlib_buffer_t *b)
{
  if (PREDICT_TRUE (!(b->flags & VLIB_BUFFER_NEXT_PRESENT)))
    return b->current_length;
  return vlib_buffer_length_in_chain (vm, b);
}

/* 队列环：容量取 2 的幂，索引用 mask，热路径不做除法 */
static inline u32
sr_qos_round_pow2 (u32 n)
{
  u32 p = 1;
  while (p < n)
    p <<= 1;
  return p;
}

always_inline void
sr_qos_q_push (sr_qos_queue_t *q, u32 bi)
{
  q->ring[(q->head + q->count) & q->mask] = bi;
  q->count++;
  q->enqueued++;
}

always_inline u32
sr_qos_q_peek (sr_qos_queue_t *q)
{
  return q->ring[q->head & q->mask];
}

always_inline u32
sr_qos_q_pop (sr_qos_queue_t *q)
{
  u32 bi = q->ring[q->head & q->mask];
  q->head++;
  q->count--;
  return bi;
}

/* 攒够一批再 free，避免每个丢包一次调用 */
typedef struct
{
  u32 v[32];
  u32 n;
} sr_qos_free_t;

always_inline void
sr_qos_free_add (vlib_main_t *vm, sr_qos_free_t *f, u32 bi)
{
  f->v[f->n++] = bi;
  if (f->n == ARRAY_LEN (f->v))
    {
      vlib_buffer_free (vm, f->v, f->n);
      f->n = 0;
    }
}

always_inline void
sr_qos_free_flush (vlib_main_t *vm, sr_qos_free_t *f)
{
  if (f->n)
    {
      vlib_buffer_free (vm, f->v, f->n);
      f->n = 0;
    }
}

/* 出队用的帧暂存：一个隧道一条，满了就投递再续 */
typedef struct
{
  vlib_frame_t *f;
  u32 *to_next;
  u32 n;
} sr_qos_out_t;

always_inline void
sr_qos_out_flush (vlib_main_t *vm, sr_qos_out_t *o, u32 to_node)
{
  if (o->n == 0)
    return;
  o->f->n_vectors = o->n;
  vlib_put_frame_to_node (vm, to_node, o->f);
  o->f = 0;
  o->to_next = 0;
  o->n = 0;
}

always_inline void
sr_qos_out_put (vlib_main_t *vm, sr_qos_out_t *o, u32 to_node, u32 bi)
{
  if (o->f == 0)
    {
      o->f = vlib_get_frame_to_node (vm, to_node);
      o->to_next = vlib_frame_vector_args (o->f);
      o->n = 0;
    }
  o->to_next[o->n++] = bi;
  if (o->n == VLIB_FRAME_SIZE)
    sr_qos_out_flush (vm, o, to_node);
}

/* ==================== 隧道状态 ==================== */

static inline sr_qos_tunnel_t *
sr_qos_tunnel_at (sr_qos_main_t *qm, u32 sw_if_index)
{
  u32 v;

  if (sw_if_index >= vec_len (qm->tunnel_by_sw_if))
    return 0;
  v = qm->tunnel_by_sw_if[sw_if_index];
  if (v == 0)
    return 0;
  return pool_elt_at_index (qm->tunnels, v - 1);
}

/*
 * 入口弧上还没有"出接口"：查一次 FIB，把"这个包会从哪条隧道出去"找出来。
 *
 * 走的是 FIB 里公开的那条路：`ip4_fib_lookup` 取等价多路径（load-balance）索引
 * → 取第一个桶的 DPO → **先确认它真的是邻接**，再 `adj_get`。这一步不能省：
 * 本机收包（DPO_RECEIVE）、丢弃、组播在这里是别的 DPO 类型，直接 `adj_get`
 * 等于拿别人的索引去邻接池里取值，是越界读，不是"分类不准"。
 *
 * 只认 `IP_LOOKUP_NEXT_MIDCHAIN`：WG 隧道的邻接就是 midchain（外层 dst 要等加密
 * 后才解析）。其它一律返回 ~0 —— 那些流量本来就不过隧道，不该被排队。
 *
 * 代价是每包一次 mtrie 查表，换的是"内层 DSCP 与隧道判别同时可得"。别为省这一次
 * 查表把它挪回出口弧：出口弧看不到转发流量（docs/qos-path-c-plan.md §10.1）。
 *
 * 已知限制：等价多路径只按**第一个桶**归隧道。本场景一条隧道一条路由，
 * 真出现"同一前缀 ECMP 到两条隧道"时要改成按流哈希选桶。
 */
always_inline u32
sr_qos_tunnel_sw_if (vlib_buffer_t *b, ip4_header_t *ip4)
{
  u32 rx = vnet_buffer (b)->sw_if_index[VLIB_RX];
  u32 lbi;
  const load_balance_t *lb;
  const dpo_id_t *dpo;
  ip_adjacency_t *adj;

  if (ip4 == 0 || rx == ~0u)
    return ~0u;

  lbi = ip4_fib_lookup (&ip4_main, rx, (ip4_address_t *) &ip4->dst_address);
  if (lbi == ~0u)
    return ~0u;

  lb = load_balance_get (lbi);
  if (lb == 0 || lb->lb_n_buckets == 0)
    return ~0u;
  dpo = load_balance_get_bucket_i (lb, 0);
  if (dpo == 0 || dpo->dpoi_type != DPO_ADJACENCY)
    return ~0u;

  adj = adj_get (dpo->dpoi_index);
  if (adj->lookup_next_index != IP_LOOKUP_NEXT_MIDCHAIN)
    return ~0u;

  return adj->rewrite_header.sw_if_index;
}

/* 清空积压：要么关整形，要么隧道没了。缓冲区归我们，必须还回去。 */static void
sr_qos_tunnel_flush (vlib_main_t *vm, sr_qos_tunnel_t *t)
{
  sr_qos_free_t fr = { {0}, 0 };

  for (u32 qi = 0; qi < SR_QOS_NQ; qi++)
    {
      sr_qos_queue_t *q = &t->q[qi];
      while (q->count)
        sr_qos_free_add (vm, &fr, sr_qos_q_pop (q));
      q->head = 0;
      q->count = 0;
      t->deficit[qi] = 0;
    }
  sr_qos_free_flush (vm, &fr);
  t->tokens = 0;
  t->llq_tokens = 0;
  t->last_refill = vlib_time_now (vm);
}

static sr_qos_tunnel_t *
sr_qos_tunnel_create (vlib_main_t *vm, sr_qos_main_t *qm, u32 sw_if_index)
{
  sr_qos_tunnel_t *t;
  u32 ti;

  pool_get (qm->tunnels, t);
  ti = t - qm->tunnels;
  clib_memset (t, 0, sizeof (*t));
  t->sw_if_index = sw_if_index;
  t->llq_percent = qm->llq_percent_default;
  t->last_refill = vlib_time_now (vm);

  for (u32 qi = 0; qi < SR_QOS_NQ; qi++)
    {
      u32 cap = sr_qos_round_pow2 (qm->depth[qi] ? qm->depth[qi] : SR_QOS_DEF_DEPTH);
      vec_validate (t->q[qi].ring, cap - 1);
      t->q[qi].mask = cap - 1;
      t->q[qi].depth = cap;
    }

  vec_validate (qm->tunnel_by_sw_if, sw_if_index);
  qm->tunnel_by_sw_if[sw_if_index] = ti + 1;
  return t;
}

int
sr_qos_set_rate (u32 sw_if_index, u32 kbps)
{
  sr_qos_main_t *qm = &sr_qos_main;
  sr_qos_tunnel_t *t = sr_qos_tunnel_at (qm, sw_if_index);

  if (kbps == 0)
    {
      if (t == 0)
        return 0; /* 幂等：本来就没开 */
      if (t->shaper_kbps)
        qm->shaped_count--;
      t->shaper_kbps = 0;
      sr_qos_tunnel_flush (qm->vlib_main, t);
      return 0;
    }

  if (kbps > 100000000) /* 100 Gbps，明显是填错了 */
    return VNET_API_ERROR_INVALID_VALUE;

  if (t == 0)
    {
      /*
       * 隧道状态不再要求"这个口挂着钩子"：钩子是**入接口**的事，速率是**隧道**
       * 的事，两者现在不是同一个接口。只校验接口本身存在，防止把速率配给一个
       * 打错的索引 —— 那种隧道永远不会有包，也就永远不会被发现配错了。
       */
      if (sw_if_index == ~0u
          || !vnet_sw_interface_is_valid (vnet_get_main (), sw_if_index))
        return VNET_API_ERROR_INVALID_SW_IF_INDEX;
      t = sr_qos_tunnel_create (qm->vlib_main, qm, sw_if_index);
    }

  if (t->shaper_kbps == 0)
    qm->shaped_count++;
  t->shaper_kbps = kbps;
  return 0;
}

int
sr_qos_set_llq_percent (u32 sw_if_index, u16 pct)
{
  sr_qos_main_t *qm = &sr_qos_main;
  sr_qos_tunnel_t *t;

  if (pct == 0 || pct > 100)
    return VNET_API_ERROR_INVALID_VALUE;
  t = sr_qos_tunnel_at (qm, sw_if_index);
  if (t == 0)
    return VNET_API_ERROR_INVALID_SW_IF_INDEX;
  t->llq_percent = pct;
  return 0;
}

/* ==================== 出队（process） ==================== */

/*
 * 一条隧道一次 drain：
 *   1. 补令牌（总桶按 shaper，LLQ 桶按 llq_percent）；
 *   2. 队列 0 严格优先，但受 LLQ 桶约束（超了就留在队列里，下一 tick 再来）；
 *   3. 队列 1..3 做 DRR：赤字按 weight 补，谁的赤字够发谁发。
 *
 * 队列 0 不走 DRR 是故意的：LLQ 的语义是"优先 + 限速"，不是"按权重分"。
 * 它被限速卡住时，剩下的带宽正好让给后面三类 —— 这就是 LLQ 该有的行为。
 */
static void
sr_qos_drain_tunnel (vlib_main_t *vm, sr_qos_tunnel_t *t, f64 now,
                     sr_qos_out_t *o, sr_qos_free_t *fr)
{
  sr_qos_main_t *qm = &sr_qos_main;
  f64 dt, rate, llq_rate, gcap, lcap;
  u32 to_node = t->drain_next;

  if (to_node == 0 || to_node == ~0u)
    return; /* 还没抓到绝对下一跳（没进过包）或解析失败，不能凭空投递 */

  dt = now - t->last_refill;
  if (dt < 0)
    dt = 0;
  /* 主线程被占很久后不补发历史额度：最多按 100ms 补，否则一次吐一大坨 */
  if (dt > 0.1)
    dt = 0.1;
  t->last_refill = now;

  rate = (f64) t->shaper_kbps * 125.0; /* kbps -> B/s */
  llq_rate = rate * (f64) t->llq_percent / 100.0;

  gcap = rate * SR_QOS_BURST_SEC;
  if (gcap < SR_QOS_MIN_BURST)
    gcap = SR_QOS_MIN_BURST;
  lcap = llq_rate * (SR_QOS_BURST_SEC * 2);
  if (lcap < SR_QOS_MIN_BURST)
    lcap = SR_QOS_MIN_BURST;

  t->tokens += rate * dt;
  if (t->tokens > gcap)
    t->tokens = gcap;
  t->llq_tokens += llq_rate * dt;
  if (t->llq_tokens > lcap)
    t->llq_tokens = lcap;

  /* ---- 1) LLQ ---- */
  while (t->q[0].count > 0)
    {
      u32 bi = sr_qos_q_peek (&t->q[0]);
      vlib_buffer_t *b = vlib_get_buffer (vm, bi);
      u32 len = sr_qos_pkt_len (vm, b);

      if (t->tokens < (f64) len || t->llq_tokens < (f64) len)
        {
          t->llq_throttled++;
          break;
        }

      sr_qos_q_pop (&t->q[0]);
      t->tokens -= len;
      t->llq_tokens -= len;
      t->tx_pkts[0]++;
      t->tx_bytes[0] += len;
      t->q[0].sent++;
      t->q[0].sent_bytes += len;
      sr_qos_out_put (vm, o, to_node, bi);
    }

  /* ---- 2) DRR over 1..3 ---- */
  for (u32 round = 0; round < 64; round++)
    {
      u32 progressed = 0, token_blocked = 0;

      if (t->tokens < SR_QOS_MIN_PKT)
        break;

      for (u32 qi = 1; qi < SR_QOS_NQ; qi++)
        {
          sr_qos_queue_t *q = &t->q[qi];
          u32 guard = 0;

          while (q->count && guard++ < 64)
            {
              u32 bi = sr_qos_q_peek (q);
              vlib_buffer_t *b = vlib_get_buffer (vm, bi);
              u32 len = sr_qos_pkt_len (vm, b);

              if (t->tokens < (f64) len)
                {
                  token_blocked = 1;
                  break;
                }
              if (t->deficit[qi] < (i32) len)
                break;

              sr_qos_q_pop (q);
              t->tokens -= len;
              t->deficit[qi] -= len;
              t->tx_pkts[qi]++;
              t->tx_bytes[qi] += len;
              q->sent++;
              q->sent_bytes += len;
              sr_qos_out_put (vm, o, to_node, bi);
              progressed = 1;
            }
        }

      if (token_blocked)
        break;

      if (!progressed)
        {
          /* 本轮谁都不够赤字：补一轮再试（DRR 的定义，不是重试） */
          u32 any = 0;
          for (u32 qi = 1; qi < SR_QOS_NQ; qi++)
            {
              if (!t->q[qi].count)
                continue;
              t->deficit[qi] += (i32) qm->weight[qi] * SR_QOS_QUANTUM;
              if (t->deficit[qi] > SR_QOS_DEFICIT_CAP)
                t->deficit[qi] = SR_QOS_DEFICIT_CAP;
              any = 1;
            }
          if (!any)
            break;
        }
    }

  /* 积压峰值：用来判断 depth 是不是定小了 */
  for (u32 qi = 0; qi < SR_QOS_NQ; qi++)
    if (t->q[qi].count > t->max_backlog[qi])
      t->max_backlog[qi] = t->q[qi].count;
}

static void
sr_qos_drain_all (vlib_main_t *vm)
{
  sr_qos_main_t *qm = &sr_qos_main;
  f64 now;
  sr_qos_free_t fr = { {0}, 0 };
  sr_qos_out_t o = { 0 };
  sr_qos_tunnel_t *t;
  u32 i;

  if (pool_elts (qm->tunnels) == 0)
    return;

  now = vlib_time_now (vm);

  /* *INDENT-OFF* */
  pool_foreach_index (i, qm->tunnels)
    {
      t = pool_elt_at_index (qm->tunnels, i);
      if (t->shaper_kbps == 0)
        continue;
      sr_qos_drain_tunnel (vm, t, now, &o, &fr);
      if (o.n)
        sr_qos_out_flush (vm, &o, t->drain_next);
    }
  /* *INDENT-ON* */

  sr_qos_free_flush (vm, &fr);
}

static uword
sr_qos_process (vlib_main_t *vm, vlib_node_runtime_t *node, vlib_frame_t *frame)
{
  sr_qos_main_t *qm = &sr_qos_main;
  uword *events = 0;

  sr_qos_proc_ready = 1;

  while (1)
    {
      f64 tick;

      sr_qos_drain_all (vm);

      /* 没有启整形的隧道就把唤醒周期放长，别在空闲盒子上 1ms 空转 */
      tick = qm->shaped_count ? SR_QOS_TICK : SR_QOS_IDLE_TICK;
      vlib_process_wait_for_event_or_clock (vm, tick);
      vlib_process_get_events (vm, &events);
      vec_reset_length (events);
    }

  return 0;
}

VLIB_REGISTER_NODE (sr_qos_process_node) = {
  .name = "sr-wg-qos-process",
  .type = VLIB_NODE_TYPE_PROCESS,
  /* 26.06 的 node 注册只有一个 function 字段（.process 已删除） */
  .function = sr_qos_process,
};

/* ==================== 入队（feature 节点） ==================== */

VLIB_NODE_FN (sr_wg4_qos_node) (vlib_main_t *vm, vlib_node_runtime_t *node,
                                vlib_frame_t *frame)
{
  sr_qos_main_t *qm = &sr_qos_main;
  u32 n_left = frame->n_vectors;
  u32 *from = vlib_frame_vector_args (frame);
  vlib_buffer_t *bufs[VLIB_FRAME_SIZE];
  u16 nexts[VLIB_FRAME_SIZE];
  u32 to_free[VLIB_FRAME_SIZE];
  u32 n_pass = 0, n_free = 0, wake = 0;

  vlib_get_buffers (vm, from, bufs, n_left);

  for (u32 i = 0; i < n_left; i++)
    {
      vlib_buffer_t *b = bufs[i];
      u32 bi = from[i];
      /* 入口弧：这里的 sw_if_index[VLIB_RX] 是"从哪进来"，出接口还没定 */
      u32 rx = vnet_buffer (b)->sw_if_index[VLIB_RX];
      u32 tun_sw_if;
      ip4_header_t *ip4 = sr_qos_ip4 (b);
      u8 dscp = ip4 ? (ip4->tos >> 2) & 0x3f : 0;
      sr_qos_tunnel_t *t;
      sr_qos_ifc_t *ic;

      /* 计数（按**入接口** × DSCP）：判断"这个口的包有没有被看到"就看它 */
      vec_validate (qm->ifs, rx);
      ic = vec_elt_at_index (qm->ifs, rx);
      ic->pkts[dscp]++;
      ic->bytes[dscp] += b->current_length;
      ic->total++;
      if (ip4 == 0)
        ic->non_ip++;
      qm->total_pkts++;

      /*
       * 走弧：每包一次，前进弧位置，拿到的是**节点内相对 next 索引**，不是绝对
       * 节点号。VPP 给 feature 节点注册时自动补一格"弧的下一个"（出口弧那次实测是
       * next-index 1 -> interface-output 783），所以这个值只能原样交给
       * vlib_buffer_enqueue_to_next。一旦把它当绝对索引用，就投给了 index=1 的
       * 节点 —— 恰好是我们自己（sr-wg4-qos 的 index 就是 1），实测到过自循环：
       * 5 个包在 2.5s 内被"转发"了 10793 次，网卡一个都没出去。
       */
      u32 rel = 0;
      vnet_feature_next (&rel, b);

      /* 隧道判别：查一次 FIB，不是读 sw_if_index[VLIB_TX]（入口弧上它还是空的） */
      tun_sw_if = sr_qos_tunnel_sw_if (b, ip4);
      t = (tun_sw_if == ~0u) ? 0 : sr_qos_tunnel_at (qm, tun_sw_if);
      if (t == 0)
        ic->unclassified++;

      /* 直通条件：没配速率、process 没起来、或弧的下一跳解析不出来 */
      if (t == 0 || t->shaper_kbps == 0 || sr_qos_proc_ready == 0
          || t->drain_next == ~0u)
        {
          from[n_pass] = bi;
          nexts[n_pass] = (u16) rel;
          n_pass++;
          continue;
        }

      /* 绝对节点号只在第一次入队时解析一次，之后按隧道缓存 */
      if (t->drain_next == 0)
        {
          /* 用运行时 node_index，而不是注册结构体：函数体比 VLIB_REGISTER_NODE
           * 先出现，引用全局符号还得前向声明，没必要。 */
          vlib_node_t *self = vlib_get_node (vm, node->node_index);
          u32 abs = ~0u;

          if (self && rel < vec_len (self->next_nodes))
            abs = self->next_nodes[rel];
          if (abs == node->node_index)
            abs = ~0u; /* 解析成自己就绝不排队：那等于让包原地转圈 */
          t->drain_next = abs;

          if (abs == ~0u)
            {
              from[n_pass] = bi;
              nexts[n_pass] = (u16) rel;
              n_pass++;
              continue;
            }
        }

      {
        u32 qi = qm->dscp_map[dscp];
        sr_qos_queue_t *q = &t->q[qi];
        u32 was_empty = (q->count == 0);

        if (q->count >= q->depth)
          {
            /* 队满：尾丢。不挤掉已排队的包 —— 保序比"最新优先"更重要 */
            q->dropped++;
            q->drop_bytes += b->current_length;
            t->drop_pkts[qi]++;
            qm->total_drops++;
            to_free[n_free++] = bi;
            continue;
          }

        sr_qos_q_push (q, bi);
        if (was_empty)
          wake = 1;
      }
    }

  if (n_pass)
    vlib_buffer_enqueue_to_next (vm, node, from, nexts, n_pass);
  if (n_free)
    vlib_buffer_free (vm, to_free, n_free);

  /* 只在"队列从空变非空"时叫醒 process：已经在排的队列下一 tick 自然会看 */
  if (wake)
    vlib_process_signal_event (vm, sr_qos_process_node.index, 0, 0);

  return frame->n_vectors;
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
 * 挂点声明：**入口弧**，排在弧尾（紧挨终端 `ip4-lookup`）。
 *
/*
 * 挂点声明：**入口弧**，不写排序约束 —— 这是三个选项里唯一可预测的那个。
 *
 * 实测（容器内 VPP 26.06，`show features` 里在 ip4-unicast 弧中的序号，共 66 个）：
 *
 *   | 声明 | 落点 |
 *   | --- | --- |
 *   | 无约束 | 第 1 位（紧接 `ip4-unicast:`） |
 *   | `runs_before ip4-lookup` | 第 1 位（约束满足了，但"在 lookup 之前"允许任意位置） |
 *   | `runs_after ip4-not-enabled` | 第 3 位（排序器只做了最小移动，不是第 62 位） |
 *
 * 第 1 位意味着本节点早于 uRPF/NAT/ACL/ABF。**这不影响正确性**：
 *   - 出队是把包送回**弧里的下一个 feature**（不是重投到入口），所以后续的
 *     urpf/ACL/NAT 照样会执行，被它们丢的包不会因为经过我们就活下来；
 *   - NAT44 的 in2out 只改源地址/端口，我们的 FIB 查询看的是**目的地址**；
 *   - 唯一会不一致的是 ABF（策略路由会改 FIB 索引）。本盒**没有用 ABF**
 *     （`show abf` 为空）；将来要用 ABF 就必须回来把顺序钉死，否则会按
 *     "默认表"把包归到错的隧道。
 *
 * 别挪回 `ip4-output`：那条弧对**转发**流量是死的（§10.1 的对照实验），
 * 只有本机自发流量经过它。
 */
VNET_FEATURE_INIT (sr_wg4_qos_feat, static) = {
  .arc_name = SR_QOS_ARC,
  .node_name = SR_QOS_NODE,
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
  sr_qos_main_t *qm = &sr_qos_main;
  int rv;

  rv = vnet_feature_enable_disable (SR_QOS_ARC, SR_QOS_NODE, sw_if_index,
                                    enable ? 1 : 0, 0, 0);
  if (rv != 0)
    return rv;

  if (enable)
    {
      if (!clib_bitmap_get (sr_qos_enabled, sw_if_index))
        qm->enabled_count++;
      sr_qos_enabled = clib_bitmap_set (sr_qos_enabled, sw_if_index, 1);
    }
  else
    {
      /*
       * 摘钩子**不动**任何隧道状态：出队目标是 ip4-lookup，与哪个入口挂着钩子
       * 无关，积压会按配置的速率继续排空。以前这里会把隧道一起关掉并 free 掉
       * 积压 —— 那是出口弧时代的语义（钩子挂的就是隧道本身），现在钩的是入口，
       * 共用一个隧道，摘一个口不该把别人的包丢掉。
       */
      if (clib_bitmap_get (sr_qos_enabled, sw_if_index))
        qm->enabled_count--;
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
      else if (unformat (input, "%U", unformat_vnet_sw_interface, vnm,
                         &sw_if_index))
        ;
      else if (unformat (input, "%u", &sw_if_index))
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

  vlib_cli_output (vm, "%s %s on %U (sw_if_index=%u)：%s",
                   enable ? "Enabled" : "Disabled", SR_QOS_NODE,
                   format_vnet_sw_if_index_name, vnm, sw_if_index, sw_if_index,
                   enable ? "该口进来的 IPv4 包开始参与调度（隧道由每包查表决定，"
                            "速率用 `set sr-wg qos rate <wg 口>` 配）"
                          : "该口不再参与调度，已有积压仍按原速率排空");
  return 0;
}

VLIB_CLI_COMMAND (sr_wg_set_qos_hook_command, static) = {
  .path = "set sr-wg qos-hook",
  .short_help = "set sr-wg qos-hook enable|disable <interface>",
  .function = sr_wg_set_qos_hook_command_fn,
};

static clib_error_t *
sr_wg_set_qos_rate_command_fn (vlib_main_t *vm, unformat_input_t *input,
                               vlib_cli_command_t *cmd)
{
  vnet_main_t *vnm = vnet_get_main ();
  u32 sw_if_index = (u32) ~0;
  u32 kbps = 0;
  u8 have_kbps = 0;
  int rv;

  while (unformat_check_input (input) != UNFORMAT_END_OF_INPUT)
    {
      if (unformat (input, "rate"))
        ;
      else if (unformat (input, "%U", unformat_vnet_sw_interface, vnm,
                         &sw_if_index))
        ;
      else if (unformat (input, "%u", &kbps))
        have_kbps = 1;
      else
        break;
    }

  if (sw_if_index == (u32) ~0 || !have_kbps)
    return clib_error_return (0, "usage: set sr-wg qos rate <interface> <kbps>");

  rv = sr_qos_set_rate (sw_if_index, kbps);
  if (rv != 0)
    return clib_error_return (0, "failed to set rate on %U (rv=%d)",
                              format_vnet_sw_if_index_name, vnm, sw_if_index,
                              rv);

  if (kbps == 0)
    vlib_cli_output (vm, "%U: 整形关闭，回到直通（积压已清空）。",
                     format_vnet_sw_if_index_name, vnm, sw_if_index);
  else
    {
      vlib_cli_output (vm, "%U: shaper %u kbps（约 %u pps @1500B）。",
                       format_vnet_sw_if_index_name, vnm, sw_if_index, kbps,
                       (u32) (((f64) kbps * 1000.0 / 8.0) / 1500.0));
      if (sr_qos_main.enabled_count == 0)
        vlib_cli_output (vm,
                         "   !! 还没有任何**入接口**挂钩子，这条隧道现在收不到包："
                         "set sr-wg qos-hook enable <客户口|隧道口>");
    }
  return 0;
}

VLIB_CLI_COMMAND (sr_wg_set_qos_rate_command, static) = {
  .path = "set sr-wg qos rate",
  .short_help = "set sr-wg qos rate <interface> <kbps | 0>",
  .function = sr_wg_set_qos_rate_command_fn,
};

static clib_error_t *
sr_wg_set_qos_llq_command_fn (vlib_main_t *vm, unformat_input_t *input,
                              vlib_cli_command_t *cmd)
{
  vnet_main_t *vnm = vnet_get_main ();
  sr_qos_main_t *qm = &sr_qos_main;
  u32 sw_if_index = (u32) ~0;
  u32 pct = 0;
  int rv;

  while (unformat_check_input (input) != UNFORMAT_END_OF_INPUT)
    {
      if (unformat (input, "llq") || unformat (input, "percent"))
        ;
      else if (unformat (input, "%U", unformat_vnet_sw_interface, vnm,
                         &sw_if_index))
        ;
      else if (unformat (input, "%u", &pct))
        ;
      else
        break;
    }

  if (sw_if_index == (u32) ~0 || pct == 0)
    return clib_error_return (
      0, "usage: set sr-wg qos llq <interface> percent <1-100>");

  rv = sr_qos_set_llq_percent (sw_if_index, (u16) pct);
  if (rv != 0)
    {
      /* 隧道还没建（没配速率）时，只改默认值，等 rate 一配就带上 */
      qm->llq_percent_default = (u16) pct;
      vlib_cli_output (vm,
                       "%U: 还没有隧道状态（先配 rate），已改为**默认** LLQ %u%%。",
                       format_vnet_sw_if_index_name, vnm, sw_if_index, pct);
      return 0;
    }

  vlib_cli_output (vm, "%U: LLQ 上限 = shaper 的 %u%%。",
                   format_vnet_sw_if_index_name, vnm, sw_if_index, pct);
  return 0;
}

VLIB_CLI_COMMAND (sr_wg_set_qos_llq_command, static) = {
  .path = "set sr-wg qos llq",
  .short_help = "set sr-wg qos llq <interface> percent <1-100>",
  .function = sr_wg_set_qos_llq_command_fn,
};

/*
 * 改队列深度：只对**空队列**原地换环。非空队列不动（换环要搬包，还得保序），
 * 并且把"没生效"的数量如实报出来 —— CLI 说了不做，比没有这个选项更坏。
 */
static void
sr_qos_resize_queues (sr_qos_main_t *qm, u32 qi, u32 depth, u32 *changed,
                      u32 *skipped)
{
  sr_qos_tunnel_t *t;
  u32 i, cap = sr_qos_round_pow2 (depth);

  *changed = 0;
  *skipped = 0;

  /* *INDENT-OFF* */
  pool_foreach_index (i, qm->tunnels)
    {
      u32 *old;

      t = pool_elt_at_index (qm->tunnels, i);
      if (t->q[qi].count)
        {
          (*skipped)++;
          continue;
        }
      old = t->q[qi].ring;
      t->q[qi].ring = 0;
      vec_validate (t->q[qi].ring, cap - 1);
      t->q[qi].mask = cap - 1;
      t->q[qi].depth = cap;
      t->q[qi].head = 0;
      vec_free (old);
      (*changed)++;
    }
  /* *INDENT-ON* */
}

static clib_error_t *
sr_wg_set_qos_queue_command_fn (vlib_main_t *vm, unformat_input_t *input,
                                vlib_cli_command_t *cmd)
{
  sr_qos_main_t *qm = &sr_qos_main;
  u32 qi = (u32) ~0, weight = 0, depth = 0;
  u8 is_weight = 0, is_depth = 0;

  while (unformat_check_input (input) != UNFORMAT_END_OF_INPUT)
    {
      u32 n;

      if (unformat (input, "queue"))
        ;
      else if (unformat (input, "weight"))
        is_weight = 1;
      else if (unformat (input, "depth"))
        is_depth = 1;
      else if (unformat (input, "%u", &n))
        {
          /* 第一个数字是队列号，后面的才是 weight/depth 的值 */
          if (qi == (u32) ~0)
            qi = n;
          else if (is_weight)
            weight = n;
          else if (is_depth)
            depth = n;
          else
            break;
        }
      else
        break;
    }

  if (qi == (u32) ~0 || qi >= SR_QOS_NQ || (is_weight == is_depth))
    return clib_error_return (
      0, "usage: set sr-wg qos queue <0-3> weight <1-100> | depth <pkts>");

  if (is_weight)
    {
      if (weight == 0)
        return clib_error_return (0, "weight 必须 >= 1");
      qm->weight[qi] = weight;
      vlib_cli_output (vm, "队列 %u 权重 = %u（仅对已存在队列生效，下一轮 DRR 起用）。",
                       qi, weight);
    }
  else
    {
      if (depth == 0 || depth > 65536)
        return clib_error_return (0, "depth 必须在 1..65536");
      qm->depth[qi] = depth;
      {
        u32 changed = 0, skipped = 0;

        sr_qos_resize_queues (qm, qi, depth, &changed, &skipped);
        vlib_cli_output (vm,
                         "队列 %u 深度 = %u 包（实际容量 %u，取 2 的幂）："
                         "已改 %u 条隧道，%u 条因队列非空跳过（排空后再执行一次即可）。",
                         qi, depth, sr_qos_round_pow2 (depth), changed, skipped);
      }
    }
  return 0;
}

VLIB_CLI_COMMAND (sr_wg_set_qos_queue_command, static) = {
  .path = "set sr-wg qos queue",
  .short_help = "set sr-wg qos queue <0-3> weight <1-100> | depth <pkts>",
  .function = sr_wg_set_qos_queue_command_fn,
};

static clib_error_t *
sr_wg_set_qos_dscp_command_fn (vlib_main_t *vm, unformat_input_t *input,
                               vlib_cli_command_t *cmd)
{
  sr_qos_main_t *qm = &sr_qos_main;
  u32 dscp = (u32) ~0, qi = (u32) ~0;
  u8 saw_queue = 0;

  while (unformat_check_input (input) != UNFORMAT_END_OF_INPUT)
    {
      if (unformat (input, "dscp"))
        ;
      else if (unformat (input, "queue"))
        saw_queue = 1;
      else if (!saw_queue && unformat (input, "%u", &dscp))
        ;
      else if (saw_queue && unformat (input, "%u", &qi))
        ;
      else
        break;
    }

  if (dscp == (u32) ~0 || dscp >= SR_QOS_N_DSCP || qi == (u32) ~0
      || qi >= SR_QOS_NQ)
    return clib_error_return (
      0, "usage: set sr-wg qos dscp <0-63> queue <0-3>");

  qm->dscp_map[dscp] = (u8) qi;
  vlib_cli_output (vm, "dscp %u -> 队列 %u（全局，立即生效）。", dscp, qi);
  return 0;
}

VLIB_CLI_COMMAND (sr_wg_set_qos_dscp_command, static) = {
  .path = "set sr-wg qos dscp",
  .short_help = "set sr-wg qos dscp <0-63> queue <0-3>",
  .function = sr_wg_set_qos_dscp_command_fn,
};

static const char *sr_qos_qname[SR_QOS_NQ] = {
  "LLQ ", /* 0 */
  "INT ", /* 1 交互 */
  "DEF ", /* 2 默认 */
  "BULK", /* 3 批量 */
};

static clib_error_t *
sr_wg_show_qos_command_fn (vlib_main_t *vm, unformat_input_t *input,
                           vlib_cli_command_t *cmd)
{
  sr_qos_main_t *qm = &sr_qos_main;
  vnet_main_t *vnm = vnet_get_main ();
  sr_qos_tunnel_t *t;
  u32 i;

  if (pool_elts (qm->tunnels) == 0)
    {
      vlib_cli_output (vm, "没有任何隧道配了速率（全部直通）。");
      vlib_cli_output (vm, "  用法：set sr-wg qos-hook enable <入接口>  "
                           "（客户口 / 隧道口，决定哪些口进来的包参与调度）");
      vlib_cli_output (vm, "        set sr-wg qos rate <wg 接口> <kbps>  "
                           "（每条隧道的整形速率）");
      return 0;
    }

  /* *INDENT-OFF* */
  pool_foreach_index (i, qm->tunnels)
    {
      u32 backlog = 0, maxb = 0;
      t = pool_elt_at_index (qm->tunnels, i);

      for (u32 qi = 0; qi < SR_QOS_NQ; qi++)
        {
          backlog += t->q[qi].count;
          maxb += t->max_backlog[qi];
        }

      vlib_cli_output (
        vm, "%U  shaper %u kbps  llq %u%%  %s  积压 %u 包（峰值 %u）",
        format_vnet_sw_if_index_name, vnm, t->sw_if_index, t->shaper_kbps,
        t->llq_percent,
        t->shaper_kbps ? "整形中" : "直通（rate=0）", backlog, maxb);

      for (u32 qi = 0; qi < SR_QOS_NQ; qi++)
        {
          sr_qos_queue_t *q = &t->q[qi];
          vlib_cli_output (
            vm,
            "   q%u %s  tx %llu 包 / %llu B   drop %llu   backlog %u/%u%s",
            qi, sr_qos_qname[qi], (unsigned long long) t->tx_pkts[qi],
            (unsigned long long) t->tx_bytes[qi],
            (unsigned long long) t->drop_pkts[qi], q->count, q->depth,
            qi ? "" : "  （严格优先，上限 = llq%）");
        }

      if (t->llq_throttled)
        vlib_cli_output (vm,
                         "   LLQ 被上限压住 %llu 次（包留在队列里，没丢）",
                         (unsigned long long) t->llq_throttled);

      /* 出队目标必须是绝对节点号；这里是排查"投给自己"那类事故的第一个地方 */
      if (t->drain_next == ~0u)
        vlib_cli_output (vm, "   !! 弧的下一跳解析失败，本隧道实际直通");
      else if (t->drain_next)
        vlib_cli_output (vm, "   出队目标节点 %u (%s)", t->drain_next,
                         (char *) vlib_get_node (vm, t->drain_next)->name);
    }
  /* *INDENT-ON* */

  vlib_cli_output (vm, "累计丢包 %llu；DRR 权重 q1=%u q2=%u q3=%u。",
                   (unsigned long long) qm->total_drops, qm->weight[1],
                   qm->weight[2], qm->weight[3]);
  return 0;
}

VLIB_CLI_COMMAND (sr_wg_show_qos_command, static) = {
  .path = "show sr-wg qos",
  .short_help = "show sr-wg qos   （每隧道 4 队列的速率、收发、丢包与积压）",
  .function = sr_wg_show_qos_command_fn,
};

static clib_error_t *
sr_wg_show_qos_hook_command_fn (vlib_main_t *vm, unformat_input_t *input,
                                vlib_cli_command_t *cmd)
{
  sr_qos_main_t *qm = &sr_qos_main;
  vnet_main_t *vnm = vnet_get_main ();
  uword sw_if_index;

  vlib_cli_output (vm,
                   "节点 %s：弧 %s（入口），终端 %s；累计 %llu 包，挂在 %u 个口。",
                   SR_QOS_NODE, SR_QOS_ARC, SR_QOS_ARC_END,
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
      vlib_cli_output (vm, "  %U: total=%llu  未归隧道=%llu%s",
                       format_vnet_sw_if_index_name, vnm, (u32) sw_if_index,
                       (unsigned long long) ic->total,
                       (unsigned long long) ic->unclassified,
                       ic->non_ip ? "  (有 non_ip，头部布局需复核)" : "");
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

/*
 * 默认 DSCP → 队列：把 Cisco 那 8 类塌成 4 类，规则是"按服务意图归组"，
 * 不是按编号均分。改这里的理由只能是客户真的有一条流需要单独一类。
 *
 *   40..47 CS5/EF/VA、48..63 CS6/CS7  -> 0  LLQ（语音 + 网络控制）
 *   24..39 CS3..CS4、AF3x/AF4x        -> 1  交互
 *   0..7 DF、16..23 CS2/AF2x          -> 2  默认
 *   8..15  CS1/AF1x                   -> 3  批量（scavenger，能等）
 */
static void
sr_qos_defaults (sr_qos_main_t *qm)
{
  for (u32 d = 0; d < SR_QOS_N_DSCP; d++)
    qm->dscp_map[d] = 2;

  for (u32 d = 8; d <= 15; d++)
    qm->dscp_map[d] = 3;
  for (u32 d = 24; d <= 39; d++)
    qm->dscp_map[d] = 1;
  for (u32 d = 40; d <= 63; d++)
    qm->dscp_map[d] = 0;

  qm->weight[0] = 0; /* 队列 0 不走 DRR，份额由 llq_percent 决定 */
  qm->weight[1] = SR_QOS_DEF_W1;
  qm->weight[2] = SR_QOS_DEF_W2;
  qm->weight[3] = SR_QOS_DEF_W3;

  for (u32 qi = 0; qi < SR_QOS_NQ; qi++)
    qm->depth[qi] = SR_QOS_DEF_DEPTH;

  qm->llq_percent_default = SR_QOS_DEF_LLQ_PCT;
}

static clib_error_t *
sr_qos_init (vlib_main_t *vm)
{
  sr_qos_main_t *qm = &sr_qos_main;

  clib_memset (qm, 0, sizeof (*qm));
  qm->vlib_main = vm;
  sr_qos_defaults (qm);

  sr_qos_enabled = 0;
  clib_bitmap_alloc (sr_qos_enabled, 1024);
  return 0;
}

VLIB_INIT_FUNCTION (sr_qos_init);
