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
#include <vnet/fib/fib_table.h>
#include <vnet/fib/fib_entry.h>
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
 * 入口弧上还没有"出接口"：这个包会从哪条隧道出去，必须自己问出来。
 *
 * **为什么不用"逐桶取邻接"那条路**（第一版实现，已被实测否掉）：CPE4 上客户
 * 前缀的转发链是 `load-balance → mpls-label[1000] → mpls via ... wg1014 →
 * stacked-on 192.168.1.102 host-enp0s9`。桶里的 DPO 是 mpls-label，dpoi_index
 * 指向标签栈/标签池，**根本不是邻接下标**；拿它去 `adj_is_valid`/`adj_get`
 * 侥幸命中别的邻接（实测命中 enp0s8 的 #1），只能靠"出接口必须在隧道表里"
 * 兜住。也就是说这条路在 MPLS over WG 的拓扑下**原理上取不到隧道口**。
 *
 * **换成控制面口径**（这才是 FIB 里唯一记着"从哪个口出去"的地方）：
 *   fib_table_lookup(fib_index, dst/32)   → fib_entry
 *   fib_entry_get_resolving_interface(fe) → 出口 sw_if_index
 * CPE4 实测：172.16.12.1→wg1014(7)、172.31.4.2→host-enp0s8(1)、
 * 8.8.8.8→host-enp0s9(3) —— 隧道/直连/WAN 三种形状全对。
 *
 * 代价是这两步是控制面量级的（池查、路径解析，可能递归），不能每包做。所以按
 * (fib_index, dst) 缓存，缓存由 process 节点每 2 秒整体失效一次（路由变了最多
 * 2 秒内自我纠正，代价是那一瞬间可能按老隧道排队 —— 排队不改变转发结果）。
 *
 * 已知限制：ECMP 到多条隧道时 `fib_entry_get_resolving_interface` 取主路径，
 * 不按流哈希。本场景一条前缀一条隧道；真要做 ECMP 得在这里补流哈希。
 */
static u32
sr_qos_rt_cp (u32 fib_index, ip4_address_t *dst)
{
  fib_prefix_t pfx;
  fib_node_index_t fei;

  clib_memset (&pfx, 0, sizeof (pfx));
  pfx.fp_proto = FIB_PROTOCOL_IP4;
  pfx.fp_addr.ip4 = *dst;
  pfx.fp_len = 32;

  fei = fib_table_lookup (fib_index, &pfx);
  if (fei == FIB_NODE_INDEX_INVALID)
    return ~0u;
  return fib_entry_get_resolving_interface (fei);
}

/* 带缓存的版本。命中就是一次 hash_get，热路径只走这里。 */
static inline u32
sr_qos_rt_lookup (u32 fib_index, ip4_address_t *dst)
{
  sr_qos_main_t *qm = &sr_qos_main;
  u64 key = ((u64) fib_index << 32) | (u64) dst->as_u32;
  uword *p;
  u32 sw;

  if (PREDICT_FALSE (qm->rt_cache == 0))
    qm->rt_cache = hash_create (0, sizeof (uword));

  p = hash_get (qm->rt_cache, key);
  if (p)
    return (u32) p[0];

  sw = sr_qos_rt_cp (fib_index, dst);
  hash_set (qm->rt_cache, key, sw);
  return sw;
}

always_inline u32
sr_qos_resolve (u32 rx, ip4_address_t *dst, u32 *why, u32 *out_type,
                u32 *out_lbi, u32 *out_next)
{
  sr_qos_main_t *qm = &sr_qos_main;
  const load_balance_t *lb;
  u32 lbi, fib_index, sw;

  *why = 0;
  *out_type = ~0u;
  *out_lbi = ~0u;
  *out_next = ~0u;
  if (dst == 0 || rx == ~0u
      || rx >= vec_len (ip4_main.fib_index_by_sw_if_index))
    {
      *why = 1;
      return ~0u;
    }

  fib_index = vec_elt (ip4_main.fib_index_by_sw_if_index, rx);

  /* 只为了诊断：把桶 0 的类型/目标节点记下来，真机上看这一栏定位问题 */
  lbi = ip4_fib_lookup (&ip4_main, rx, dst);
  *out_lbi = lbi;
  if (lbi != ~0u)
    {
      lb = load_balance_get (lbi);
      if (lb && lb->lb_n_buckets)
        {
          const dpo_id_t *d0 = load_balance_get_bucket_i (lb, 0);

          if (d0)
            {
              *out_type = (u32) d0->dpoi_type;
              *out_next = (u32) d0->dpoi_next_node;
            }
        }
    }
  if (lbi == ~0u)
    {
      *why = 1;
      return ~0u;
    }

  sw = sr_qos_rt_lookup (fib_index, dst);
  if (sw == ~0u)
    {
      *why = 1;
      return ~0u;
    }

  /*
   * 唯一的裁决：出接口必须是一条**配了速率**的隧道（我们自己的隧道表）。
   * 直连、WAN 默认路由、本机收包都不在那张表里，正常直通。隧道在但没配速率
   * 也直通 —— "先挂钩子"这个动作永远不掉包、不排队。
   */
  if (sr_qos_tunnel_at (qm, sw) != 0)
    return sw;

  *why = 2;
  return ~0u;
}

always_inline u32
sr_qos_tunnel_sw_if (vlib_buffer_t *b, ip4_header_t *ip4, u32 *why,
                     u32 *out_type, u32 *out_lbi, u32 *out_next)
{
  return sr_qos_resolve (vnet_buffer (b)->sw_if_index[VLIB_RX],
                         ip4 ? (ip4_address_t *) &ip4->dst_address : 0, why,
                         out_type, out_lbi, out_next);
}

/* 清空积压：要么关整形，要么隧道没了。缓冲区归我们，必须还回去。 */
static void
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
      f64 tick, now = vlib_time_now (vm);

      sr_qos_drain_all (vm);

      /*
       * 路由缓存整体失效：控制面解析（fib_table_lookup + resolving interface）
       * 只在未命中时走，命中就是一次 hash_get。2 秒一次的代价可以忽略，换的是
       * "路由改了最多 2 秒内跟上"。不清的话，隧道切换后会把包一直算到旧隧道头上。
       */
      if (now - qm->rt_cache_flush >= 2.0)
        {
          if (qm->rt_cache)
            {
              hash_free (qm->rt_cache);
              qm->rt_cache = 0;
            }
          qm->rt_cache_flush = now;
        }

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
      {
        u32 why = 0, dtype = ~0u, lbi = ~0u, nnext = ~0u;

        tun_sw_if = sr_qos_tunnel_sw_if (b, ip4, &why, &dtype, &lbi, &nnext);
        t = (tun_sw_if == ~0u) ? 0 : sr_qos_tunnel_at (qm, tun_sw_if);
        if (t == 0)
          {
            ic->unclassified++;
            ic->why[why <= 4 ? why : 0]++;
            ic->last_dpo_type = dtype;
            ic->last_lbi = lbi;
            ic->last_next_node = nnext;
          }
      }

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
      if (ic->unclassified)
        vlib_cli_output (vm,
                         "    未归原因: 查表无路=%llu 出口不是隧道/未配速率=%llu   "
                         "last_dpo_type=%u last_next_node=%u last_lbi=%u",
                         (unsigned long long) ic->why[1],
                         (unsigned long long) ic->why[2], ic->last_dpo_type,
                         ic->last_next_node, ic->last_lbi);
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

/*
 * 隧道判别自查：把"这个目的地址会被判成哪条隧道"的全过程摊开打印。
 * 真机上"未归隧道"只能看到一个数字，这一条能把 FIB 索引、load-balance 桶、
 * 每个桶的 DPO 类型都摊出来 —— 排查挂点问题全靠它。
 *
 *   show sr-wg qos resolve <ip> [from <interface>]
 */
static clib_error_t *
sr_wg_show_qos_resolve_command_fn (vlib_main_t *vm, unformat_input_t *input,
                                   vlib_cli_command_t *cmd)
{
  ip4_address_t addr;
  u32 rx = 0, fib_index, lbi, leaf;
  int have_addr = 0;

  while (unformat_check_input (input) != UNFORMAT_END_OF_INPUT)
    {
      if (unformat (input, "%U", unformat_ip4_address, &addr))
        have_addr = 1;
      else if (unformat (input, "from %U", unformat_vnet_sw_interface,
                         vnet_get_main (), &rx))
        ;
      else
        return clib_error_return (0, "unknown input `%U'",
                                  format_unformat_error, input);
    }
  if (!have_addr)
    return clib_error_return (0, "usage: show sr-wg qos resolve <ip> [from <if>]");

  vlib_cli_output (vm, "目的 %U  from sw_if_index=%u", format_ip4_address,
                   &addr, rx);
  if (rx >= vec_len (ip4_main.fib_index_by_sw_if_index))
    {
      vlib_cli_output (vm, "  !! rx 超出 fib_index_by_sw_if_index（len=%u）",
                       vec_len (ip4_main.fib_index_by_sw_if_index));
      return 0;
    }
  fib_index = vec_elt (ip4_main.fib_index_by_sw_if_index, rx);
  lbi = ip4_fib_lookup (&ip4_main, rx, &addr);
  leaf = ip4_fib_forwarding_lookup (fib_index, &addr);
  vlib_cli_output (vm, "  fib_index=%u  ip4_fib_lookup(load-balance)=%u  "
                   "forwarding_lookup(leaf)=%u",
                   fib_index, lbi, leaf);
  /*
   * 控制面口径的"这个目的地址从哪个口出去"：FIB 表最长匹配拿 fib_entry，
   * 再问它解析到哪个接口。MPLS/SR 场景下转发表里是 mpls-label DPO，数据面
   * 看不出接口，只有这条路能拿到（wg1014 这种）。
   */
  {
    fib_prefix_t pfx = { .fp_proto = FIB_PROTOCOL_IP4,
                         .fp_addr = { .ip4 = addr },
                         .fp_len = 32 };
    fib_node_index_t fei = fib_table_lookup (fib_index, &pfx);
    u32 rsw = (fei != FIB_NODE_INDEX_INVALID)
                ? fib_entry_get_resolving_interface (fei)
                : ~0u;

    vlib_cli_output (vm, "  控制面: fib_table_lookup=%d resolving_interface=%u"
                     "  %U", (int) fei, rsw, format_vnet_sw_if_index_name,
                     vnet_get_main (), rsw);
  }

  /*
   * 先跑一次 resolver：它顺带把"目的地址→出接口"落进缓存，下面的解释性输出
   * 与判定用的是同一条路 —— 早先这里先打印后解析，打出来的永远是 0。
   */
  {
    u32 why = 0, dtype = ~0u, lbi2 = ~0u, nnext = ~0u;
    u32 sw = sr_qos_resolve (rx, &addr, &why, &dtype, &lbi2, &nnext);

    if (lbi != ~0u)
      {
        const load_balance_t *lb = load_balance_get (lbi);

        vlib_cli_output (vm, "  lb: buckets=%u inline=%u proto=%u hash_cfg=0x%x",
                         lb->lb_n_buckets,
                         lb->lb_n_buckets <= LB_NUM_INLINE_BUCKETS ? 1 : 0,
                         lb->lb_proto, lb->lb_hash_config);
        for (u32 i = 0; i < lb->lb_n_buckets && i < 8; i++)
          {
            const dpo_id_t *d = load_balance_get_bucket_i (lb, i);
            u8 *name = (u8 *) "?";

            if (d)
              {
                vlib_node_t *n = vlib_get_node (vm, d->dpoi_next_node);

                if (n && n->name)
                  name = n->name;
              }
            vlib_cli_output (vm,
                             "    bucket[%u]: type=%u proto=%u next_node=%u "
                             "(%v) index=%u raw=0x%016llx",
                             i, d ? (u32) d->dpoi_type : ~0u,
                             d ? (u32) d->dpoi_proto : ~0u,
                             d ? (u32) d->dpoi_next_node : ~0u, name,
                             d ? d->dpoi_index : ~0u,
                             d ? (unsigned long long) d->as_u64 : 0ULL);
            if (d == 0)
              continue;
            if (!adj_is_valid (d->dpoi_index))
              {
                vlib_cli_output (vm, "        ↳ dpoi_index 不在邻接池里"
                                 "（MPLS/标签/LB 这类 DPO，出接口要看控制面那行）");
                continue;
              }
            {
              ip_adjacency_t *a = adj_get (d->dpoi_index);

              vlib_cli_output (vm,
                               "        ↳ 若当成邻接看: lookup_next_index=%u "
                               "flags=0x%x rewrite.sw_if_index=%u 隧道表里%s",
                               (u32) a->lookup_next_index, (u32) a->ia_flags,
                               a->rewrite_header.sw_if_index,
                               (a->rewrite_header.sw_if_index != ~0u
                                && sr_qos_tunnel_at (&sr_qos_main,
                                                     a->rewrite_header
                                                       .sw_if_index) != 0)
                                 ? "有（会被排队）"
                                 : "没有（不排队）");
            }
          }
      }
    vlib_cli_output (vm,
                     "  本插件编译期枚举（仅构建环境自测有效）: "
                     "IP_LOOKUP_NEXT_MIDCHAIN=%d DPO_ADJACENCY=%d "
                     "DPO_ADJACENCY_MIDCHAIN=%d",
                     IP_LOOKUP_NEXT_MIDCHAIN, DPO_ADJACENCY,
                     DPO_ADJACENCY_MIDCHAIN);
    vlib_cli_output (vm,
                     "  => 判定 sw_if_index=%u %U  why=%u（0 正常/1 查表无路/"
                     "2 出口不是配了速率的隧道）  bucket0_type=%u bucket0_next=%u",
                     sw, format_vnet_sw_if_index_name, vnet_get_main (), sw, why,
                     dtype, nnext);
  }
  return 0;
}

VLIB_CLI_COMMAND (sr_wg_show_qos_resolve_command, static) = {
  .path = "show sr-wg qos resolve",
  .short_help = "show sr-wg qos resolve <ip> [from <if>]（隧道判别自查）",
  .function = sr_wg_show_qos_resolve_command_fn,
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
