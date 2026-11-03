/*
 * sr_path.h - 路径管理数据结构（控制面 -> 数据面）
 *
 * 路径表最多 SR_MAX_PATHS 条，每条路径存的是**中间转发跳**、该路径签名用的
 * P-Key 与 active 标志。终点不在这里：见下面"路径 = 只有中间跳"。
 */
#ifndef SR_PATH_H
#define SR_PATH_H

#include "sr_header.h"
#include "sr_hmac.h"

#ifdef __cplusplus
extern "C" {
#endif

/*
 * 路径配置（控制面下发给数据面）。
 *
 * 路径 = 只有中间跳。
 *
 * SR 负责的是"这一条隧道要经过哪几台中转"，对端终点是谁由 WG 自己决定：
 * encap 时终点段直接取自**即将发出的那个包**的外层 dst（IP + 端口），再和
 * 中转跳拼成线格式里的段列表。所以 endpoint 的地址与端口怎么变（NAT 重绑、
 * 动态学习、对端换端口）都不用改 SR 路径 —— 那正是把终点写进路径表会留下的
 * 缺陷：地址一变，路径表里就存着一个没人再听的老地址。
 *
 * 顺序沿用 sr_header.h 的约定（segments[0] = 最终目的，末位 = 第一跳），
 * 只是把终点那一位交给报文：relays[0] = 离终点最近的那台中转，
 * relays[relay_count-1] = 第一跳。encap 填出来的线格式是
 *   header.segments[0]      = 报文里的外层 dst（终点）
 *   header.segments[1 + i]  = relays[i]
 * 手敲时 `via` 的书写顺序不变，只是不再写对端那一段。
 */
typedef struct sr_path_config {
    uint16_t     path_id;                   /* 路径标识 */
    uint16_t     hmac_id;                   /* HMAC 密钥标识 */
    uint8_t      relay_count;               /* 中转跳数 (0..SR_MAX_RELAYS) */
    sr_segment_t relays[SR_MAX_SEGS];       /* 中转跳列表（网络序） */
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

/*
 * 一条路径最多几台中转。
 *
 * 线格式里的段总数上限是 SR_MAX_SEGS（头部长度字段只有 8 位，且 hdr_len
 * 要能装下），而终点那一段永远要占掉一位（它来自报文，不是来自配置），
 * 所以能配置的只有 SR_MAX_SEGS - 1 跳。
 */
#define SR_MAX_RELAYS  (SR_MAX_SEGS - 1)

/* path_id 是 16 位，所以定位表可以定长：一次数组访问就能拿到槽位。 */
#define SR_PATH_ID_SPAN  65536

/* sr_path_add / sr_path_del 的返回值 */
#define SR_PATH_OK          0
#define SR_PATH_ERR_PARAM  (-1)   /* 参数非法（NULL / relay_count 越界 / 不存在） */
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
 *
 * endpoint 是**终点段**，由调用方从当前报文的外层 dst 取来（IP 与端口都必须是
 * 网络序，路径表里的中转跳同样是网络序）。它不是可选参数：终点段参与 HMAC
 * 覆盖范围，必须在签名**之前**就位，签完再改就是必然验签失败。
 *
 * 线格式的段总数 = relay_count + 1（终点那一位），所以
 * seg_left 由调用方覆盖为 relay_count（"首跳免费"那条规则不变）。
 *
 * timestamp 为主机序秒级时间戳。返回 0 成功，-1 失败。
 */
int sr_path_fill_header(const sr_path_config_t *cfg, sr_header_t *h,
                        uint32_t timestamp, const sr_segment_t *endpoint);

#ifdef __cplusplus
}
#endif

#endif /* SR_PATH_H */
