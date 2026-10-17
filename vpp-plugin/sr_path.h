/*
 * sr_path.h - 路径管理数据结构（控制面 -> 数据面）
 *
 * 路径表最多 SR_MAX_PATHS 条，每条路径包含完整的段列表、
 * 独立的 P-Key，以及 active 标志。
 */
#ifndef SR_PATH_H
#define SR_PATH_H

#include "sr_header.h"
#include "sr_hmac.h"

#ifdef __cplusplus
extern "C" {
#endif

/* 路径配置（控制面下发给数据面） */
typedef struct sr_path_config {
    uint16_t     path_id;                   /* 路径标识 */
    uint16_t     hmac_id;                   /* HMAC 密钥标识 */
    uint8_t      seg_count;                 /* 段数 (1..SR_MAX_SEGS) */
    sr_segment_t segments[SR_MAX_SEGS];     /* 段列表（网络序） */
    uint8_t      pkey[SR_PKEY_LEN];         /* 该路径使用的 P-Key */
    uint8_t      active;                    /* 是否激活 */
} sr_path_config_t;

/* 路径表（最大 64 条路径） */
#define SR_MAX_PATHS  64

typedef struct sr_path_table {
    sr_path_config_t paths[SR_MAX_PATHS];
    int              count;                 /* 当前使用的槽位数 */
} sr_path_table_t;

/* 初始化路径表（清零）。返回 0 成功，-1 失败 */
int sr_path_table_init(sr_path_table_t *t);

/*
 * 添加/更新路径。若 path_id 已存在则覆盖，否则占用空槽。
 * 返回 0 成功，-1 失败（参数非法或表已满）。
 */
int sr_path_add(sr_path_table_t *t, const sr_path_config_t *cfg);

/*
 * 删除指定 path_id 的路径。
 * 返回 0 成功，-1 未找到或参数非法。
 */
int sr_path_del(sr_path_table_t *t, uint16_t path_id);

/*
 * 按 path_id 查找路径配置。
 * 返回配置指针；未找到返回 NULL。
 */
sr_path_config_t *sr_path_lookup(sr_path_table_t *t, uint16_t path_id);

/*
 * 统计引用了该 hmac_id 的路径条数（含 active=0）。
 * 路径里存的是 P-Key 的**副本**，所以"删了密钥但路径还在用旧副本签名"
 * 是控制面看不见的状态 —— 删密钥前必须先问这个。
 */
int sr_path_count_using_hmac_id(const sr_path_table_t *t, uint16_t hmac_id);

/*
 * 把新密钥刷进所有引用该 hmac_id 的路径副本，返回刷新条数。
 * 密钥轮换靠它生效：否则 `key set` 之后旧路径仍按旧密钥签名，
 * 对端验签失败，现象是"密钥换了隧道就断了"。
 */
int sr_path_replace_pkey(sr_path_table_t *t, uint16_t hmac_id,
                         const uint8_t *pkey);

/*
 * 根据路径配置填充 SR Header（初始化 + 设置各字段 + HMAC 签名）。
 * timestamp 为主机序秒级时间戳。
 * 返回 0 成功，-1 失败。
 */
int sr_path_fill_header(const sr_path_config_t *cfg, sr_header_t *h,
                        uint32_t timestamp);

#ifdef __cplusplus
}
#endif

#endif /* SR_PATH_H */
