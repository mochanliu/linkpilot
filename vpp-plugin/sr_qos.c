/*
 * sr_qos.c - 物理口统一 QoS：入口标记 + 出向 4 队列调度 + 整形
 *
 * 设计与不变量见 sr_qos.h 头注（docs/qos-path-c-plan.md §13）。三条实现约束：
 *
 * 1) 标记器（sr-wg4-qos，ip4-unicast 弧）只读内层 DSCP、写 buffer 元数据，
 *    **绝不排队**。为了不被"弧上到底带不带 L2"这种布局细节坑到，
 *    sr_qos_ip4() 两种布局都探（先 current，再 +14），探不到按 DSCP 0 处理
 *    并计 non_ip —— 分类器出错可以查，改包不行。
 *
 * 2) 出向调度器（sr-wg4-qos-out，interface-output 弧）按**最外层 IP DSCP**
 *    分类：buffer 元数据里有标记器/恢复点写的类别（QOS_DATA_VALID 置位）
 *    就直接用，否则读包头 ToS 再过一次 dscp_map（未封装流量的路径）。
 *    令牌桶单位是字节，速率来自配置（外层字节口径）。shaper_kbps == 0 时
 *    整个排队路径不进入。
 *
 * 3) 出队由 process 节点做：vlib_get_frame_to_node/vlib_put_frame_to_node
 *    直接递给**弧上的下一个节点**（唯一 feature 时就是
 *    interface-output-arc-end）。缓冲区元数据（VLIB_TX 等）全程没动过，
 *    出队后包按原样进驱动 —— 排队不改变转发结果，只改时机。
 */
#include "sr_qos.h"

#include <vlib/vlib.h>
#include <vnet/feature/feature.h>
#include <vnet/interface_funcs.h>
#include <vnet/ip/ip4_packet.h>
#include <vnet/ip/ip4.h>
#include <vnet/ip/format.h>
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
 * 取 IP4 头，两种布局都探：
 *  - 标记器（入口弧）：current 通常就是 IP4；个别路径前面有 L2；
 *  - 出向调度器（interface-output 弧）：ip4-rewrite 已把 L2 写好，
 *    current 指向以太网头，IP4 在 +14。
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

/* 整形按整条链的长度算（外层字节口径，af_packet 上通常单缓冲区，但别假设） */
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

/* 出队用的帧暂存：一个口一条，满了就投递再续 */
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

/* ==================== shaper 状态 ==================== */

static inline sr_qos_shaper_t *
sr_qos_shaper_at (sr_qos_main_t *qm, u32 sw_if_index)
{
  u32 v;

  if (sw_if_index >= vec_len (qm->shaper_by_sw_if))
    return 0;
  v = qm->shaper_by_sw_if[sw_if_index];
  if (v == 0)
    return 0;
  return pool_elt_at_index (qm->shapers, v - 1);
}

/* 清空积压：关整形时用。缓冲区归我们，必须还回去。 */
static void
sr_qos_shaper_flush (vlib_main_t *vm, sr_qos_shaper_t *t)
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

static sr_qos_shaper_t *
sr_qos_shaper_create (vlib_main_t *vm, sr_qos_main_t *qm, u32 sw_if_index)
{
  sr_qos_shaper_t *t;
  u32 ti;

  pool_get (qm->shapers, t);
  ti = t - qm->shapers;
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

  vec_validate (qm->shaper_by_sw_if, sw_if_index);
  qm->shaper_by_sw_if[sw_if_index] = ti + 1;
  return t;
}

int
sr_qos_set_rate (u32 sw_if_index, u32 kbps)
{
  sr_qos_main_t *qm = &sr_qos_main;
  sr_qos_shaper_t *t = sr_qos_shaper_at (qm, sw_if_index);
  int rv;

  if (kbps == 0)
    {
      if (t == 0)
        return 0; /* 幂等：本来就没开 */
      if (t->shaper_kbps)
        qm->shaped_count--;
      t->shaper_kbps = 0;
      sr_qos_shaper_flush (qm->vlib_main, t);
      /* feature 也摘掉：口回到完全直通，计数都不做 */
      vnet_feature_enable_disable (SR_QOS_OUT_ARC, SR_QOS_OUT_NODE,
                                   sw_if_index, 0, 0, 0);
      {
        u32 ti = t - qm->shapers;
        for (u32 qi = 0; qi < SR_QOS_NQ; qi++)
          vec_free (t->q[qi].ring);
        qm->shaper_by_sw_if[sw_if_index] = 0;
        pool_put_index (qm->shapers, ti);
      }
      return 0;
    }

  if (kbps > 100000000) /* 100 Gbps，明显是填错了 */
    return VNET_API_ERROR_INVALID_VALUE;
  if (sw_if_index == ~0u ||
      !vnet_sw_interface_is_valid (vnet_get_main (), sw_if_index))
    return VNET_API_ERROR_INVALID_SW_IF_INDEX;

  /* 先挂 feature 再建 shaper：feature 挂不上就不许有"配了速率却没人排队"的状态 */
  if (t == 0)
    {
      rv = vnet_feature_enable_disable (SR_QOS_OUT_ARC, SR_QOS_OUT_NODE,
                                        sw_if_index, 1, 0, 0);
      if (rv != 0)
        return rv;
      t = sr_qos_shaper_create (qm->vlib_main, qm, sw_if_index);
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
  sr_qos_shaper_t *t;

  if (pct == 0 || pct > 100)
    return VNET_API_ERROR_INVALID_VALUE;
  t = sr_qos_shaper_at (qm, sw_if_index);
  if (t == 0)
    return VNET_API_ERROR_INVALID_SW_IF_INDEX;
  t->llq_percent = pct;
  return 0;
}

/* ==================== 出队（process） ==================== */

/*
 * 一个口一次 drain：
 *   1. 补令牌（总桶按 shaper，LLQ 桶按 llq_percent）；
 *   2. 队列 0 严格优先，但受 LLQ 桶约束（超了就留在队列里，下一 tick 再来）；
 *   3. 队列 1..3 做 DRR：赤字按 weight 补，谁的赤字够发谁发。
 *
 * 队列 0 不走 DRR 是故意的：LLQ 的语义是"优先 + 限速"，不是"按权重分"。
 */
static void
sr_qos_drain_shaper (vlib_main_t *vm, sr_qos_shaper_t *t, f64 now,
                     sr_qos_out_t *o)
{
  sr_qos_main_t *qm = &sr_qos_main;
  f64 dt, rate, llq_rate, gcap, lcap;
  u32 to_node = t->drain_next;

  if (to_node == 0 || to_node == ~0u)
    return; /* 还没解析出重注入目标，不能凭空投递 */

  dt = now - t->last_refill;
  if (dt < 0)
    dt = 0;
  /* 主线程被占很久后不补发历史额度：最多按 100ms 补，否则一次吐一大坨 */
  if (dt > 0.1)
    dt = 0.1;
  t->last_refill = now;

  rate = (f64) t->shaper_kbps * 125.0; /* kbps -> B/s（外层字节） */
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
  sr_qos_out_t o = { 0 };
  sr_qos_shaper_t *t;
  u32 i;

  if (pool_elts (qm->shapers) == 0)
    return;

  now = vlib_time_now (vm);

  /* *INDENT-OFF* */
  pool_foreach_index (i, qm->shapers)
    {
      t = pool_elt_at_index (qm->shapers, i);
      if (t->shaper_kbps == 0)
        continue;
      sr_qos_drain_shaper (vm, t, now, &o);
      if (o.n)
        sr_qos_out_flush (vm, &o, t->drain_next);
    }
  /* *INDENT-ON* */
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

      /* 没有启用整形的口就把唤醒周期放长，别在空闲盒子上 1ms 空转 */
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

/* ==================== 入口标记器（ip4-unicast 弧） ==================== */

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
      u32 rx = vnet_buffer (b)->sw_if_index[VLIB_RX];
      ip4_header_t *ip4 = sr_qos_ip4 (b);
      u8 dscp = ip4 ? (ip4->tos >> 2) & 0x3f : 0;
      u8 cls;
      sr_qos_ifc_t *ic;
      u32 rel = 0;

      /* 计数（按入接口 × DSCP）：判断"这个口的包有没有被标记"就看它 */
      vec_validate (qm->ifs, rx);
      ic = vec_elt_at_index (qm->ifs, rx);
      ic->pkts[dscp]++;
      ic->bytes[dscp] += b->current_length;
      ic->total++;
      if (ip4 == 0)
        ic->non_ip++;
      qm->total_pkts++;

      /*
       * 只标记，不排队（§13 步骤 1）：类别写进 VPP 标准 QoS buffer 元数据，
       * 同一个 buffer 走到封装点，encap 从这里取类别写外层 ToS + SR 头 action。
       * 旧的 FIB 隧道解析（三版判据踩坑那套）已整体删除 —— 隧道身份在封装点
       * 天然已知，入口弧上不需要它。
       */
      cls = qm->dscp_map[dscp];
      vnet_buffer2 (b)->qos.bits = cls;
      b->flags |= VNET_BUFFER_F_QOS_DATA_VALID;

      vnet_feature_next (&rel, b);
      nexts[i] = (u16) rel;
    }

  vlib_buffer_enqueue_to_next (vm, node, from, nexts, n_left);
  return frame->n_vectors;
}

VLIB_REGISTER_NODE (sr_wg4_qos_node) = {
  .name = SR_QOS_MARK_NODE,
  .vector_size = sizeof (u32),
  .type = VLIB_NODE_TYPE_INTERNAL,
  .n_next_nodes = 1,
  .next_nodes = {
    [0] = "error-drop",
  },
};

/*
 * 挂点声明：入口弧，不写排序约束（O3 时代实测过三种声明都落第 1 位）。
 * 只标记不改包不排队，早于 uRPF/NAT/ACL 无任何正确性影响。
 */
VNET_FEATURE_INIT (sr_wg4_qos_feat, static) = {
  .arc_name = SR_QOS_MARK_ARC,
  .node_name = SR_QOS_MARK_NODE,
};

/* ==================== 出向调度器（interface-output 弧） ==================== */

VLIB_NODE_FN (sr_wg4_qos_out_node) (vlib_main_t *vm,
                                    vlib_node_runtime_t *node,
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
      u32 tx = vnet_buffer (b)->sw_if_index[VLIB_TX];
      sr_qos_shaper_t *t = sr_qos_shaper_at (qm, tx);
      u32 rel = 0;

      vnet_feature_next (&rel, b);

      /* 直通条件：这个口没配速率、process 没起来 */
      if (t == 0 || t->shaper_kbps == 0 || sr_qos_proc_ready == 0)
        {
          from[n_pass] = bi;
          nexts[n_pass] = (u16) rel;
          n_pass++;
          continue;
        }

      /*
       * 分类（§13 步骤 3）：优先用 buffer 元数据里的类别（标记器/中转/decap
       * 写的，权威）；没标记过的（普通转发、本机自发）读最外层 IP ToS 过
       * dscp_map。两条路最终同一个类别空间。
       */
      {
        u8 cls;
        u32 qi;
        sr_qos_queue_t *q;
        u32 was_empty;

        if (b->flags & VNET_BUFFER_F_QOS_DATA_VALID)
          cls = sr_qos_class_of_buffer (b);
        else
          {
            ip4_header_t *ip4 = sr_qos_ip4 (b);
            u8 dscp = ip4 ? (ip4->tos >> 2) & 0x3f : 0;
            cls = qm->dscp_map[dscp];
          }
        qi = cls;
        q = &t->q[qi];
        was_empty = (q->count == 0);

        /* 重注入目标只在第一次入队时解析：本 feature 在弧上的下一个节点。
         * 解析成自己就绝不排队 —— 那等于让包原地转圈（O3 时代实测的坑）。 */
        if (t->drain_next == 0)
          {
            vlib_node_t *self = vlib_get_node (vm, node->node_index);
            u32 abs = ~0u;

            if (self && rel < vec_len (self->next_nodes))
              abs = self->next_nodes[rel];
            if (abs == node->node_index)
              abs = ~0u;
            t->drain_next = abs;

            if (abs == ~0u)
              {
                from[n_pass] = bi;
                nexts[n_pass] = (u16) rel;
                n_pass++;
                continue;
              }
          }

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

VLIB_REGISTER_NODE (sr_wg4_qos_out_node) = {
  .name = SR_QOS_OUT_NODE,
  .vector_size = sizeof (u32),
  .type = VLIB_NODE_TYPE_INTERNAL,
  .n_next_nodes = 1,
  .next_nodes = {
    [0] = "error-drop",
  },
};

/*
 * 挂点声明：interface-output 弧（一切封装/加密/重写完成后、进驱动之前，
 * 每包恰好一次 —— WG 封装包与普通转发包在这里统一按最终 DSCP 分类）。
 * 不写排序约束：容器实测该弧现有 features 是 stats-collect-tx / ipsec-if-output /
 * span-output，我们与它们互不依赖。
 */
VNET_FEATURE_INIT (sr_wg4_qos_out_feat, static) = {
  .arc_name = SR_QOS_OUT_ARC,
  .node_name = SR_QOS_OUT_NODE,
};

/* ==================== 标记器启用状态 ==================== */

/*
 * 影子位图：只记录我们自己成功调用过 enable 的接口（同 O3 时代的理由）。
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

  rv = vnet_feature_enable_disable (SR_QOS_MARK_ARC, SR_QOS_MARK_NODE,
                                    sw_if_index, enable ? 1 : 0, 0, 0);
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
                              enable ? "enable" : "disable", SR_QOS_MARK_NODE,
                              format_vnet_sw_if_index_name, vnm, sw_if_index,
                              rv, SR_QOS_MARK_ARC);

  vlib_cli_output (vm, "%s %s on %U (sw_if_index=%u)：%s",
                   enable ? "Enabled" : "Disabled", SR_QOS_MARK_NODE,
                   format_vnet_sw_if_index_name, vnm, sw_if_index, sw_if_index,
                   enable ? "该口进来的 IPv4 包开始被标记类别"
                          "（内层 DSCP→3bit，随封装传到外层 ToS 与 SR 头 action）"
                          : "该口不再标记（已出去的包不受影响）");
  return 0;
}

VLIB_CLI_COMMAND (sr_wg_set_qos_hook_command, static) = {
  .path = "set sr-wg qos-hook",
  .short_help = "set sr-wg qos-hook enable|disable <入接口>  （标记器）",
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
    return clib_error_return (0, "usage: set sr-wg qos rate <出方向物理口> <kbps>");

  rv = sr_qos_set_rate (sw_if_index, kbps);
  if (rv != 0)
    return clib_error_return (0, "failed to set rate on %U (rv=%d)",
                              format_vnet_sw_if_index_name, vnm, sw_if_index,
                              rv);

  if (kbps == 0)
    vlib_cli_output (vm, "%U: 整形关闭，%s 摘除，完全直通（积压已清空）。",
                     format_vnet_sw_if_index_name, vnm, sw_if_index,
                     SR_QOS_OUT_NODE);
  else
    {
      vlib_cli_output (vm,
                       "%U: shaper %u kbps（外层字节口径，约 %u pps @1500B）。"
                       "%s 已挂到 interface-output 弧，按最外层 DSCP 进 4 队列。",
                       format_vnet_sw_if_index_name, vnm, sw_if_index, kbps,
                       (u32) (((f64) kbps * 1000.0 / 8.0) / 1500.0),
                       SR_QOS_OUT_NODE);
      if (sr_qos_main.enabled_count == 0)
        vlib_cli_output (vm,
                         "   !! 还没有任何**入接口**挂标记器，隧道流量不会被分类："
                         "set sr-wg qos-hook enable <客户口>");
    }
  return 0;
}

VLIB_CLI_COMMAND (sr_wg_set_qos_rate_command, static) = {
  .path = "set sr-wg qos rate",
  .short_help = "set sr-wg qos rate <出方向物理口> <kbps | 0>",
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
      /* 口还没建 shaper（没配速率）时，只改默认值，等 rate 一配就带上 */
      qm->llq_percent_default = (u16) pct;
      vlib_cli_output (vm,
                       "%U: 还没有 shaper 状态（先配 rate），已改为**默认** LLQ %u%%。",
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
 * 并且把"没生效"的数量如实报出来。
 */
static void
sr_qos_resize_queues (sr_qos_main_t *qm, u32 qi, u32 depth, u32 *changed,
                      u32 *skipped)
{
  sr_qos_shaper_t *t;
  u32 i, cap = sr_qos_round_pow2 (depth);

  *changed = 0;
  *skipped = 0;

  /* *INDENT-OFF* */
  pool_foreach_index (i, qm->shapers)
    {
      u32 *old;

      t = pool_elt_at_index (qm->shapers, i);
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
                         "已改 %u 个口，%u 个因队列非空跳过（排空后再执行一次即可）。",
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
  sr_qos_shaper_t *t;
  u32 i;

  if (pool_elts (qm->shapers) == 0)
    {
      vlib_cli_output (vm, "没有任何出方向物理口配了速率（全部直通）。");
      vlib_cli_output (vm, "  用法：set sr-wg qos-hook enable <入接口>  "
                           "（标记器：哪些口进来的包参与分类）");
      vlib_cli_output (vm, "        set sr-wg qos rate <出方向物理口> <kbps>  "
                           "（外层字节口径；自动挂 %s）", SR_QOS_OUT_NODE);
      return 0;
    }

  /* *INDENT-OFF* */
  pool_foreach_index (i, qm->shapers)
    {
      u32 backlog = 0, maxb = 0;
      t = pool_elt_at_index (qm->shapers, i);

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
        vlib_cli_output (vm, "   !! 弧的下一跳解析失败，本口实际直通");
      else if (t->drain_next)
        vlib_cli_output (vm, "   出队目标节点 %u (%s)", t->drain_next,
                         (char *) vlib_get_node (vm, t->drain_next)->name);
    }
  /* *INDENT-ON* */

  vlib_cli_output (vm, "累计丢包 %llu；DRR 权重 q1=%u q2=%u q3=%u；"
                   "默认表 EF→0 AF4x→1 AF3x→2 其余→3。",
                   (unsigned long long) qm->total_drops, qm->weight[1],
                   qm->weight[2], qm->weight[3]);
  return 0;
}

VLIB_CLI_COMMAND (sr_wg_show_qos_command, static) = {
  .path = "show sr-wg qos",
  .short_help = "show sr-wg qos   （每出方向物理口 4 队列的速率、收发、丢包与积压）",
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
                   "节点 %s：弧 %s（入口，只标记不排队）；累计 %llu 包，挂在 %u 个口。",
                   SR_QOS_MARK_NODE, SR_QOS_MARK_ARC,
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
      vlib_cli_output (vm, "  %U: total=%llu%s",
                       format_vnet_sw_if_index_name, vnm, (u32) sw_if_index,
                       (unsigned long long) ic->total,
                       ic->non_ip ? "  (有 non_ip，头部布局需复核)" : "");
      for (u32 d = 0; d < SR_QOS_N_DSCP; d++)
        if (ic->pkts[d])
          vlib_cli_output (vm, "    dscp %2u: %llu pkt / %llu B -> q%u", d,
                           (unsigned long long) ic->pkts[d],
                           (unsigned long long) ic->bytes[d],
                           qm->dscp_map[d]);
    }
  /* *INDENT-ON* */

  return 0;
}

VLIB_CLI_COMMAND (sr_wg_show_qos_hook_command, static) = {
  .path = "show sr-wg qos-hook",
  .short_help = "show sr-wg qos-hook   （标记器计数，按入接口与 DSCP 分桶）",
  .function = sr_wg_show_qos_hook_command_fn,
};

/* ==================== Init ==================== */

/*
 * 默认 DSCP → 类别（owner 2026-11-13 拍板）：EF→0，AF4x→1，AF3x→2，其余→3。
 * 封装侧的类别→DSCP（写外层 ToS）见 sr_qos.h 的 class_dscp 表，两张表
 * 往返一致：外层 DSCP 再过一次本表得回原类别。
 */
static void
sr_qos_defaults (sr_qos_main_t *qm)
{
  for (u32 d = 0; d < SR_QOS_N_DSCP; d++)
    qm->dscp_map[d] = 3;

  for (u32 d = 24; d <= 31; d++)
    qm->dscp_map[d] = 2; /* AF3x */
  for (u32 d = 32; d <= 39; d++)
    qm->dscp_map[d] = 1; /* AF4x */
  qm->dscp_map[46] = 0;  /* EF */

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
