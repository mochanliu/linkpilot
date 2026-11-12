/*
 * sr_qos.h - 隧道出口 QoS：挂点、分类骨架与状态
 *
 * ==================== 为什么挂在这里 ====================
 *
 * QoS 的对象是**隧道**，不是物理网卡（docs/qos-path-c-plan.md §9）。
 * 但 WG 的 wg4-output-tun 把 next 硬编码成 adj-midchain-tx、全程不调
 * vnet_feature_next()，所以挂在它**之后**的 feature 永远拿不到包（0 vectors，
 * sr_wg4_encap.c 头注里的实测）。挂在它**之前**才是明文、逐隧道、加密前：
 *
 *   内层包 → ip4-lookup → ip4-output 弧
 *           → sr-wg4-qos（本节点：分类/排队/整形，内层 DSCP 可读）
 *           → wg4-output-tun（加密）
 *           → adj-midchain-tx → … → underlay 网卡
 *
 * `runs_before = VNET_FEATURES("wg4-output-tun")` 是这条弧上唯一的正确声明，
 * 不要改成 interface-output（那等于把自己排到加密之后，就是那个死点）。
 *
 * ==================== 当前阶段 ====================
 *
 * 第一阶段只做**分类骨架**：按 (出口 sw_if_index, 内层 DSCP) 计数后原样透传。
 * 目的有两个：
 *   1) 验挂点——挂上后 ping 10 个包，计数必须正好 10（§9.4 的门槛）；
 *   2) 拿到真实流量的类分布，8 个队列怎么分、LLQ 给谁，用数据定而不是拍脑袋。
 *
 * 第二阶段（下一步）把"计数 + 透传"换成"入队 + 定时 drain + token bucket"，
 * 节点、弧、CLI、配置下发路径全部复用，不重写。
 */
#ifndef SR_QOS_H
#define SR_QOS_H

#include "sr_wg.h"

#ifdef __cplusplus
extern "C" {
#endif

#define SR_QOS_ARC      "ip4-output"
#define SR_QOS_NODE     "sr-wg4-qos"
/* 本节点必须排在它之前；改这个名字等于改挂点语义，见文件头 */
#define SR_QOS_PRE_NODE "wg4-output-tun"

#define SR_QOS_N_DSCP 64

/* 一条隧道（= 一个 wg 接口）的分类计数 */
typedef struct
{
  u64 pkts[SR_QOS_N_DSCP]; /* 按内层 DSCP 分桶 */
  u64 bytes[SR_QOS_N_DSCP];
  u64 total;               /* 该接口进入本节点的总包数 */
} sr_qos_ifc_t;

typedef struct
{
  /* 按 sw_if_index 索引的 vec（稀疏，用 vec_validate 长出来） */
  sr_qos_ifc_t *ifs;
  u64 total_pkts; /* 全插件累计，用来一眼判断"挂上了没" */
  u32 enabled_count;
} sr_qos_main_t;

extern sr_qos_main_t sr_qos_main;

/* 挂/摘本节点在某接口上的 ip4-output feature。返回 vnet_feature_enable_disable 的 rv */
int sr_qos_hook_enable_disable (u32 sw_if_index, int enable);

/* 该接口当前是否挂着本节点（影子位图，仅在本次 VPP 生命周期内有效） */
int sr_qos_hook_enabled (u32 sw_if_index);

#ifdef __cplusplus
}
#endif

#endif /* SR_QOS_H */
