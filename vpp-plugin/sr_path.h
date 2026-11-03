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

/*
 * 路径表容量。
 *
 * 一条隧道占一条路径（path_id 由本端节点号 + 对端节点号算出来），所以这个数
 * 就是"一台盒子能同时终结多少条 VPN 隧道"。运营口径是单盒 3000 条隧道，
 * 软件层留到 4096（不是每台硬件都得跑到这个数，是软件不许先于硬件封顶）。
 */
#define SR_MAX_PATHS  4096

/* path_id 是 16 位，所以定位表可以定长：一次数组访问就能拿到槽位。 */
#define SR_PATH_ID_SPAN  65536

/* sr_path_add / sr_path_del 的返回值 */
#define SR_PATH_OK          0
#define SR_PATH_ERR_PARAM  (-1)   /* 参数非法（NULL / seg_count 越界 / 不存在） */
#define SR_PATH_ERR_FULL   (-2)   /* 表已满，装不下新 path_id（覆盖已有条目不受影响） */

typedef struct sr_path_table {
    sr_path_config_t paths[SR_MAX_PATHS];
    /*
     * index[path_id] = 槽位下标 + 1，0 表示"这个 path_id 不在表里"。
     * 存 +1 是因为槽 0 是合法位置，而 0 必须留给"没有"。
     *
     * 这张表存在的理由是**热路径**：encap 每个出站包都要按 path_id 取一次配置，
     * 4096 条线性扫在最坏情况下要比较 4096 次。paths[] 仍保持紧凑
     * （[0..count-1] 有效，删除用末位搬移），所以 dump/统计这类遍历照旧走 count。
     */
    uint16_t         index[SR_PATH_ID_SPAN];
    int              count;                 /* 当前使用的槽位数 */
} sr_path_table_t;

/* 初始化路径表（清空条目与索引表）。返回 0 成功，-1 失败 */
int sr_path_table_init(sr_path_table_t *t);

/*
 * 添加/更新路径。若 path_id 已存在则覆盖，否则占用空槽。
 * 返回 SR_PATH_OK；参数非法返回 SR_PATH_ERR_PARAM；
 * 表里已有 SR_MAX_PATHS 条且 path_id 是新的，返回 SR_PATH_ERR_FULL
 * —— 覆盖已有路径不受容量限制，调用方要能分辨这两种失败。
 */
int sr_path_add(sr_path_table_t *t, const sr_path_config_t *cfg);

/*
 * 删除指定 path_id 的路径。
 * 返回 SR_PATH_OK，未找到或参数非法返回 SR_PATH_ERR_PARAM。
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
