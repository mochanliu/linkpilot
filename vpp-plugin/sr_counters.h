/*
 * sr_counters.h - 数据面计数器定义
 *
 * 用于统计 Encap / Transit / Decap 各阶段的成功与各类失败原因，
 * 便于观测和排障。计数器为纯数据结构，由数据面代码负责递增。
 */
#ifndef SR_COUNTERS_H
#define SR_COUNTERS_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/*
 * 数据面计数器
 *
 * 注意：字段顺序即内存布局，新增字段一律追加到对应分组末尾，
 * 不要插入到中间，以免打乱已有的偏移和 API 兼容性。
 */
typedef struct sr_counters {
    /* Encap */
    uint64_t encap_ok;              /* 成功封装 */
    uint64_t encap_no_path;         /* 已绑定隧道但路径缺失/未激活 */
    uint64_t encap_hmac_fail;       /* HMAC 计算失败 */
    uint64_t encap_bypass;          /* 未绑定路径或非 SR-WG 流量，直通 */
    uint64_t encap_no_headroom;     /* headroom 不足，无法插入 SR Header */

    /* Transit */
    uint64_t transit_ok;            /* 成功中转 */
    uint64_t transit_hmac_fail;     /* HMAC 验证失败 */
    uint64_t transit_seg_exhausted; /* seg_left == 0 但不是终点 */
    uint64_t transit_bad_magic;     /* magic 不匹配 */
    uint64_t transit_bad_ver;       /* 版本不支持 */
    uint64_t transit_bad_len;       /* 长度不一致 */

    /* Decap */
    uint64_t decap_ok;              /* 成功解封装 */
    uint64_t decap_hmac_fail;       /* HMAC 验证失败 */
    uint64_t decap_seg_not_zero;    /* seg_left != 0（未到终点） */
    uint64_t decap_bypass;          /* 非 SR 包（WG 直连模式），直通 wg4-input */

    /* General */
    uint64_t drop_total;            /* 总丢弃 */
} sr_counters_t;

#ifdef __cplusplus
}
#endif

#endif /* SR_COUNTERS_H */
