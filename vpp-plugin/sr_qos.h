/*
 * sr_qos.h - 隧道出口 QoS：挂点、分类、4 队列调度与整形
 *
 * ==================== 为什么挂在这里 ====================
 *
 * QoS 的对象是**隧道**，不是物理网卡（docs/qos-path-c-plan.md §9）。
 *
 * 第一版把节点挂在这条**出口**弧上、排在 wg4-output-tun 之前，结果被实测推翻：
 * 转发流量进隧道走的是 `ip4-lookup → ip4-rewrite → ip4-midchain → wg4-output-tun`，
 * 那条链上没有任何 feature 弧入口，**只有本机自发流量才经过 wg 口的 ip4-output 弧**
 * （docs/qos-path-c-plan.md §10）。所以现在挂在**入口**弧：
 *
 *   内层包 → ip4-input → ip4-unicast 弧（按**入接口**生效）
 *           → sr-wg4-qos（本节点：查一次 FIB 得到"出哪条隧道"，再分类/排队/整形）
 *           → ip4-lookup → ip4-rewrite → ip4-midchain → wg4-output-tun（加密）
 *
 * 两个要点，改代码前先读：
 *
 * 1) **入口弧上还没有"出接口"**。隧道靠 `ip4_fib_table_get_index_for_sw_if_index`
 *    + `ip4_fib_forwarding_lookup` + `adj_get` 反查邻接的 `sw_if_index` 得到，
 *    只认 `IP_LOOKUP_NEXT_MIDCHAIN`（隧道邻接就是 midchain；其它类型一律当"不是
 *    隧道"透传）。这多花一次 mtrie 查表，换的是"内层 DSCP 与隧道同时可得"。
 * 2) **出队回注 `ip4-lookup`**（入口弧的终端），等于让包把原来的路重走一遍；
 *    不碰加密节点、不碰邻接，也就不需要任何"补元数据"的代码。
 *
 * 不要把它挪回 `ip4-output` 弧 —— 那条弧对转发流量是死的，§10.1 有对照实验。
 *
 * ==================== 队列模型（owner 2026-11-12 拍板） ====================
 *
 * 「四个队列够用」+「速率靠配置」+「LLQ 上限给默认值、可配」。
 *
 *   队列 0  LLQ     语音与网络控制：EF/CS5/CS6/CS7
 *                   严格优先，但**自带速率上限**（llq_percent × shaper），
 *                   否则一条打满的语音流会把其余三类饿死。
 *   队列 1  交互    CS3/CS4/AF3x/AF4x
 *   队列 2  默认    DF/CS2/AF2x（未标记流量落这里）
 *   队列 3  批量    CS1/AF1x（scavenger，能等）
 *
 * 调度：隧道总速率由 token bucket 卡死（shaper_kbps，来自配置）；
 * 队列 0 先按 LLQ 上限取，剩下的在 1..3 之间按权重做 DRR。
 * 只有 1..3 有「权重」，队列 0 的份额由 llq_percent 决定 —— 这与 Cisco
 * `priority` + `bandwidth percent` 的关系一致：LLQ 是优先且限速，不是按权重分。
 *
 * ==================== 两条安全不变量 ====================
 *
 * 1) shaper_kbps == 0 ⇒ **完全直通**（只计数、不排队、不丢包）。
 *    所以「先把钩子挂上」这个动作永远不会掉流量，限速是显式开的。
 * 2) process 节点没跑起来（proc_ready == 0）⇒ 不入队，直通。
 *    宁可没限速，也不能把包吸进一个没人排空的队列。
 *
 * 注意：本文件不含任何控制面/管理流量的特殊通道 —— 管理走 host-enp0s8/9，
 * 根本不进隧道，也就不过本节点（owner 已确认 LLQ 里没有管理流量）。
 */
#ifndef SR_QOS_H
#define SR_QOS_H

#include "sr_wg.h"

#ifdef __cplusplus
extern "C" {
#endif

#define SR_QOS_ARC      "ip4-unicast"
#define SR_QOS_NODE     "sr-wg4-qos"
/* 入口弧的终端节点：出队就往这里送（= 让包重走 ip4-lookup） */
#define SR_QOS_ARC_END  "ip4-lookup"

#define SR_QOS_N_DSCP 64

/* 队列数：owner 拍「四个够用」。改这个值要同时改 dscp_map 的默认语义。 */
#define SR_QOS_NQ 4

/* 默认值（全部可由 CLI 改；速率本身没有默认值 —— 不配就直通） */
#define SR_QOS_DEF_LLQ_PCT  20 /* LLQ 上限 = shaper 的 20% */
#define SR_QOS_DEF_DEPTH    128 /* 每队列最多压 128 包 */
#define SR_QOS_DEF_W1       30 /* 交互 */
#define SR_QOS_DEF_W2       50 /* 默认 */
#define SR_QOS_DEF_W3       20 /* 批量 */

/* DRR：每次 visit 给 weight × QUANTUM 字节的赤字 */
#define SR_QOS_QUANTUM   1500
#define SR_QOS_DEFICIT_CAP (4 * SR_QOS_QUANTUM)
/* drain 的退出闸：小于这个就没必要再试（不是 MTU，避免小包吃不饱） */
#define SR_QOS_MIN_PKT   64

/* 令牌桶：突发上限 = 速率的 0.5%（5ms），下限两个 MTU */
#define SR_QOS_BURST_SEC 0.005
#define SR_QOS_MIN_BURST (2 * 1500)

/* 空闲时的唤醒周期（没有启用整形的隧道就慢下来，别 1ms 空转） */
#define SR_QOS_TICK     0.001
#define SR_QOS_IDLE_TICK 0.050

/* 一个**入接口**的分类计数（挂点计数，用来判断"这个口的包有没有被看到"） */
typedef struct
{
  u64 pkts[SR_QOS_N_DSCP]; /* 按内层 DSCP 分桶 */
  u64 bytes[SR_QOS_N_DSCP];
  u64 total;               /* 该接口进入本节点的总包数 */
  u64 non_ip;              /* 头部不像 IP4 的包（布局假设被打破的信号） */
  u64 unclassified;        /* 查表后不是隧道（本机收包/直连路由），正常直通 */
} sr_qos_ifc_t;

/* 一条队列：定长环 + 计数。ring 在第一次入队时按 depth 分配。 */
typedef struct
{
  u32 *ring;  /* vec，长度 = depth（取 2 的幂，热路径用 mask 不用除法） */
  u32 depth;
  u32 mask;   /* depth - 1 */
  u32 head;   /* 环起点 */
  u32 count;  /* 当前积压包数 */
  u64 enqueued;
  u64 sent;
  u64 dropped;
  u64 sent_bytes;
  u64 drop_bytes;
} sr_qos_queue_t;

/* 一条被整形的隧道 */
typedef struct
{
  u32 sw_if_index;
  u32 shaper_kbps; /* 0 = 直通（安全默认） */
  u16 llq_percent;
  u16 pad;

  u32 drain_next; /* 弧上下一节点（= ip4-lookup），入队时缓存 */
  f64 tokens;     /* 隧道总令牌（字节） */
  f64 llq_tokens; /* LLQ 自己的令牌桶（字节） */
  f64 last_refill;

  i32 deficit[SR_QOS_NQ]; /* DRR 赤字，字节 */
  sr_qos_queue_t q[SR_QOS_NQ];

  u64 tx_pkts[SR_QOS_NQ];
  u64 tx_bytes[SR_QOS_NQ];
  u64 drop_pkts[SR_QOS_NQ];
  u32 max_backlog[SR_QOS_NQ]; /* 见过的最大积压，判断深度够不够用 */
  u64 llq_throttled;          /* LLQ 因上限被压住的次数（不是丢包） */
} sr_qos_tunnel_t;

typedef struct
{
  vlib_main_t *vlib_main; /* init 时存下来：flush 要它才能还缓冲区 */

  /* 按 sw_if_index 索引的 vec（稀疏，用 vec_validate 长出来） */
  sr_qos_ifc_t *ifs;
  u64 total_pkts; /* 全插件累计，用来一眼判断"挂上了没" */
  u32 enabled_count;

  /* 隧道状态 */
  sr_qos_tunnel_t *tunnels; /* pool */
  u32 *tunnel_by_sw_if;     /* vec：pool index + 1，0 = 没有 */
  u32 shaped_count;         /* shaper_kbps > 0 的隧道数 */

  /* 全局配置（队列模型本身，不随隧道变） */
  u8 dscp_map[SR_QOS_N_DSCP];
  u32 weight[SR_QOS_NQ];
  u32 depth[SR_QOS_NQ];
  u16 llq_percent_default;

  u64 total_drops;
} sr_qos_main_t;

extern sr_qos_main_t sr_qos_main;
extern vlib_node_registration_t sr_qos_process_node;

/*
 * 挂/摘本节点在某**入接口**上的 ip4-unicast feature。返回 vnet_feature_enable_disable 的 rv。
 * 语义：<if> 是"包从哪进来"，不是"从哪出去"。隧道由每包的 FIB 查表决定。
 */
int sr_qos_hook_enable_disable (u32 sw_if_index, int enable);

/* 该**入接口**当前是否挂着本节点（影子位图，仅在本次 VPP 生命周期内有效） */
int sr_qos_hook_enabled (u32 sw_if_index);

/*
 * 设置某条隧道的整形速率（kbps）。0 = 关掉整形、回直通并清空积压。
 * 这是**控制面唯一的入口**：不调用它，钩子只计数，永远不掉包。
 * 返回 0 成功；非 0 为 VNET_API_ERROR_*。
 */
int sr_qos_set_rate (u32 sw_if_index, u32 kbps);

/* 设置某条隧道的 LLQ 上限（百分比 1..100）。返回 0 成功，非 0 参数错。 */
int sr_qos_set_llq_percent (u32 sw_if_index, u16 pct);

#ifdef __cplusplus
}
#endif

#endif /* SR_QOS_H */
