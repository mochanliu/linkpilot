/*
 * sr_qos.h - 物理口统一 QoS：入口标记 + 出向调度 + SR 头类别传递
 *
 * ==================== 方案（owner 2026-11-13 拍板，docs/qos-path-c-plan.md §13） ====================
 *
 * QoS 的对象是**出方向物理口**，不是隧道：物理口出口是共享资源，上面还有非
 * WG 流量，只给 WG 调度没有意义。四步：
 *
 *   1) 入口标记   sr-wg4-qos（ip4-unicast 弧，按入接口 enable）
 *                 读内层 DSCP → 3bit 类别 → vnet_buffer2(b)->qos。
 *                 **只标记，不排队**。旧版在这里做的 FIB 隧道解析（rt_cache、
 *                 三版判据踩坑）整体删除 —— 类别经 buffer 元数据走到封装点，
 *                 隧道身份在封装点天然已知，不需要任何查表。
 *
 *   2) 封装传递   sr-wg4-encap 把类别写进两处：
 *                 ① SR 头 action 高 3bit（action 在 HMAC 排除集里，不用重签；
 *                    数据路径没人读；validate 不检查 —— §12.2 已核）
 *                 ② 最外层 IP ToS（类别代表 DSCP，见 class_dscp 表）
 *                 内层头此刻已进密文，encap 不能重新分类，只信标记器。
 *
 *   3) 物理口调度 sr-wg4-qos-out（interface-output 弧，按出接口 enable，
 *                 set rate 时自动挂）按**最外层 IP DSCP** 进 4 队列
 *                 （LLQ + DRR + 令牌桶 + 尾丢，复用旧隧道版实现）。
 *                 挂点必须用 interface-output 弧：underlay 的 ip4-output 弧上
 *                 同一条流被经过 ~3.5 次（§10.1），排队会重复排队；
 *                 interface-output 在一切封装完成后每包恰好一次。
 *                 速率口径 = **外层字节直配**（含加密 + SR 头 + L2 开销）。
 *
 *   4) 逐跳恢复   中转（sr-wg4-transit）与隧道对端解封装（sr-wg4-input）都从
 *                 action 恢复类别 —— **必须步骤，不是保险**：运营商可能重标记
 *                 外层 DSCP，端到端权威类别锚在 SR 头 action 3bit 上
 *                 （UDP 载荷运营商不动）。恢复点重写外层 ToS / 写 qos 元数据。
 *
 * ==================== 队列模型（owner 2026-11-12 拍板，沿用） ====================
 *
 *   队列 0  LLQ   严格优先，自带速率上限（llq_percent × shaper）
 *   队列 1        交互
 *   队列 2        默认
 *   队列 3        批量（scavenger）
 *
 * DSCP→类别默认表（owner 2026-11-13 拍板，CLI 可逐项改）：
 *   EF(46) → 0；AF4x(32..39) → 1；AF3x(24..31) → 2；其余 → 3
 * 类别→DSCP（封装时写外层 ToS 用）取每类的代表值：0→46，1→34，2→26，3→0。
 * 往返一致性：外层 DSCP 再过一次 dscp_map 得回同一类别。
 *
 * ==================== 两条安全不变量（沿用） ====================
 *
 * 1) shaper_kbps == 0（没 set rate）⇒ 该物理口完全直通，只计数不排队。
 * 2) process 没跑起来（proc_ready == 0）⇒ 不入队，直通。
 *    宁可没限速，也不能把包吸进没人排空的队列。
 */
#ifndef SR_QOS_H
#define SR_QOS_H

#include "sr_wg.h"

#ifdef __cplusplus
extern "C" {
#endif

/* ---------- 挂点 ---------- */

/* 标记器：入口弧（内层 DSCP 只在这里可读），按入接口 enable */
#define SR_QOS_MARK_ARC  "ip4-unicast"
#define SR_QOS_MARK_NODE "sr-wg4-qos"

/* 出向调度：interface-output 弧（一切封装完成后、进驱动前，每包恰一次），
 * 按出方向物理口 enable。弧的终点节点是 interface-output-arc-end（容器实测），
 * 出队重注入目标 = 本 feature 的 next（O3 同款模式，见 sr_qos.c）。 */
#define SR_QOS_OUT_ARC   "interface-output"
#define SR_QOS_OUT_NODE  "sr-wg4-qos-out"

/* ---------- 类别 <-> SR 头 action / DSCP ---------- */

#define SR_QOS_N_DSCP 64
#define SR_QOS_NQ     4

/* 没被标记器碰过的包（本机自发的握手/BFD 等）落批量类，等价 ToS=0 直通 */
#define SR_QOS_CLASS_UNMARKED 3

/* 类别存放在 SR 头 action 字节的高 3bit（bit7..5），低 5 位保持 FORWARD=0 */
#define SR_QOS_ACTION_SHIFT 5

/* 每类的代表 DSCP：封装写外层 ToS、中转恢复都用它。
 * 0→EF 46、1→AF41 34、2→AF31 26、3→DF 0 —— 与默认 dscp_map 往返一致。 */
static const u8 sr_qos_class_dscp[SR_QOS_NQ] = { 46, 34, 26, 0 };

always_inline u8
sr_qos_action_to_class (u8 action)
{
  return (action >> SR_QOS_ACTION_SHIFT) & 7;
}

always_inline u8
sr_qos_class_to_action (u8 cls)
{
  return (u8) ((cls & 7) << SR_QOS_ACTION_SHIFT);
}

always_inline u8
sr_qos_dscp_of_class (u8 cls)
{
  return cls < SR_QOS_NQ ? sr_qos_class_dscp[cls] : 0;
}

/* 标记器写入、其它节点读取的 buffer 元数据（VPP 标准 QoS opaque2 字段） */
always_inline u8
sr_qos_class_of_buffer (vlib_buffer_t *b)
{
  u8 cls = vnet_buffer2 (b)->qos.bits;
  return cls < SR_QOS_NQ ? cls : SR_QOS_CLASS_UNMARKED;
}

/* ---------- 队列参数 ---------- */

#define SR_QOS_DEF_LLQ_PCT  20
#define SR_QOS_DEF_DEPTH    128
#define SR_QOS_DEF_W1       30
#define SR_QOS_DEF_W2       50
#define SR_QOS_DEF_W3       20

#define SR_QOS_QUANTUM      1500
#define SR_QOS_DEFICIT_CAP  (4 * SR_QOS_QUANTUM)
#define SR_QOS_MIN_PKT      64

#define SR_QOS_BURST_SEC    0.005
#define SR_QOS_MIN_BURST    (2 * 1500)

#define SR_QOS_TICK         0.001
#define SR_QOS_IDLE_TICK    0.050

/* 一个**入接口**的标记计数（"这个口的包有没有被标记"就看它） */
typedef struct
{
  u64 pkts[SR_QOS_N_DSCP]; /* 按内层 DSCP 分桶 */
  u64 bytes[SR_QOS_N_DSCP];
  u64 total;
  u64 non_ip;              /* 头部不像 IP4（布局假设被打破的信号） */
} sr_qos_ifc_t;

/* 一条队列：定长环 + 计数（沿用旧实现） */
typedef struct
{
  u32 *ring;
  u32 depth;
  u32 mask;
  u32 head;
  u32 count;
  u64 enqueued;
  u64 sent;
  u64 dropped;
  u64 sent_bytes;
  u64 drop_bytes;
} sr_qos_queue_t;

/*
 * 一个被整形的**出方向物理口**（旧版是隧道，语义换了实现没换）。
 * drain_next = 出队重注入的绝对节点号 = 本 feature 在弧上的下一个节点
 * （唯一 feature 时就是 interface-output-arc-end），第一次入队时按运行时
 * next 表解析一次，防"投给自己"自循环（O3 时代实测踩过的坑）。
 */
typedef struct
{
  u32 sw_if_index;
  u32 shaper_kbps; /* 0 = 直通（安全默认） */
  u16 llq_percent;
  u16 pad;

  u32 drain_next;
  f64 tokens;
  f64 llq_tokens;
  f64 last_refill;

  i32 deficit[SR_QOS_NQ];
  sr_qos_queue_t q[SR_QOS_NQ];

  u64 tx_pkts[SR_QOS_NQ];
  u64 tx_bytes[SR_QOS_NQ];
  u64 drop_pkts[SR_QOS_NQ];
  u32 max_backlog[SR_QOS_NQ];
  u64 llq_throttled;
} sr_qos_shaper_t;

typedef struct
{
  vlib_main_t *vlib_main;

  /* 标记器：按入接口索引的计数 vec（稀疏） */
  sr_qos_ifc_t *ifs;
  u64 total_pkts;
  u32 enabled_count;

  /* 出向调度：按出接口索引的 shaper pool */
  sr_qos_shaper_t *shapers;   /* pool */
  u32 *shaper_by_sw_if;       /* vec：pool index + 1，0 = 没有 */
  u32 shaped_count;

  /* 全局配置 */
  u8 dscp_map[SR_QOS_N_DSCP]; /* DSCP → 队列/类别（CLI 可改） */
  u32 weight[SR_QOS_NQ];
  u32 depth[SR_QOS_NQ];
  u16 llq_percent_default;

  u64 total_drops;
} sr_qos_main_t;

extern sr_qos_main_t sr_qos_main;
extern vlib_node_registration_t sr_qos_process_node;

/* 标记器挂/摘（入接口）。旧 CLI `set sr-wg qos-hook` 保留同名。 */
int sr_qos_hook_enable_disable (u32 sw_if_index, int enable);
int sr_qos_hook_enabled (u32 sw_if_index);

/*
 * 设置某**出方向物理口**的整形速率（kbps，外层字节口径）。
 * kbps > 0：自动把 sr-wg4-qos-out 挂到该口的 interface-output 弧上；
 * kbps == 0：摘 feature、清积压、删 shaper（完全直通）。
 */
int sr_qos_set_rate (u32 sw_if_index, u32 kbps);

/* 设置某口的 LLQ 上限（百分比 1..100） */
int sr_qos_set_llq_percent (u32 sw_if_index, u16 pct);

#ifdef __cplusplus
}
#endif

#endif /* SR_QOS_H */
