/*
 * sr_header.h - SR 路径头线格式定义（Single Source of Truth）
 *
 * 本文件是整个系统（controller / vpp-plugin / 测试）对 SR Header
 * 线格式（wire format）的唯一权威定义。
 *
 * 布局（参见 docs/sr-header-format.md）：
 *
 *   0        1        2        3        4        5        6      8      16
 *   +--------+--------+--------+--------+--------+--------+------+--------+
 *   | magic  |ver_flag|hdr_len |seg_left|seg_cnt | action | rsvd |hmac_id |
 *   +--------+--------+--------+--------+--------+--------+------+--------+
 *   | path_id        | timestamp                  | segments[0..N-1]      |
 *   +----------------+----------------------------+-----------------------+
 *   | ... segments 每段 6B (IPv4 + Port) ...       | hmac (8B)             |
 *   +----------------------------------------------+-----------------------+
 *
 *   hdr_len = 16 + seg_count * 6 + 8 = 24 + seg_count * 6
 *
 * 所有多字节字段均为网络序（big-endian），以 uint8_t 数组存储，
 * 通过下方辅助函数（内部使用 htons/htonl）访问。
 *
 * HMAC 覆盖范围：ver_flags, hdr_len, seg_count, reserved,
 *                hmac_id, path_id, timestamp, segments[0..N-1]
 * 排除字段：magic（固定值）, seg_left（每跳变）, action（每跳可能变）
 */
#ifndef SR_HEADER_H
#define SR_HEADER_H

#include <stdint.h>
#include <stddef.h>
#include <string.h>
#include <arpa/inet.h>

#ifdef __cplusplus
extern "C" {
#endif

/* ==================== 常量 ==================== */

#define SR_MAGIC            0x53    /* 'S' */
#define SR_VERSION          0x01
#define SR_VER_FLAGS        ((SR_VERSION << 4) | 0x00)  /* 版本1，标志位0 */
#define SR_VER_MASK         0xF0    /* ver_flags 高 4 位为版本号 */

#define SR_FIXED_HDR_LEN    16      /* 固定部分(8B) + 可扩展部分(8B) */
#define SR_HMAC_LEN         8       /* HMAC-SHA256 截断 64 位 */
#define SR_SEG_SIZE         6       /* 每段：IPv4(4) + Port(2) */
#define SR_MAX_SEGS         8       /* 最大段数 */

/* hdr_len = 24 + seg_count * 6 */
#define SR_HDR_LEN(segs)    (SR_FIXED_HDR_LEN + (segs) * SR_SEG_SIZE + SR_HMAC_LEN)

/* 线格式最大总长度（seg_count == SR_MAX_SEGS 时） */
#define SR_HDR_MAX_LEN      SR_HDR_LEN(SR_MAX_SEGS)     /* 24 + 48 = 72 */

/* Action 码 */
#define SR_ACTION_FORWARD   0x00
#define SR_ACTION_DELIVER   0x01    /* 预留 */

/* 默认端口 */
#define SR_PORT_VPN         51820   /* Encap/Decap */
#define SR_PORT_TRANSIT     51821   /* Transit */

/* ==================== 数据结构 ==================== */

/* 路径段：一个中转/终点的 locator（IPv4 + UDP Port），网络序 */
typedef struct __attribute__((packed)) sr_segment {
    uint8_t  ip[4];     /* IPv4 地址，网络序 */
    uint8_t  port[2];   /* UDP 端口，网络序 */
} sr_segment_t;

/*
 * SR Header - 线格式（wire format）
 *
 * 总长度 = SR_FIXED_HDR_LEN + seg_count * SR_SEG_SIZE + SR_HMAC_LEN
 *
 * 注意：segments 是固定大小数组（SR_MAX_SEGS），仅用于本地内存布局；
 * 实际线上传输只包含 seg_count 个段，HMAC 紧跟在最后一个段之后。
 * 请使用 sr_hdr_get_hmac() / sr_hdr_serialize_len() 访问和序列化。
 */
typedef struct __attribute__((packed)) sr_header {
    /* 固定部分 (8 bytes) */
    uint8_t  magic;         /* 0x53 ('S') */
    uint8_t  ver_flags;     /* 高4位版本，低4位标志 */
    uint8_t  hdr_len;       /* SR Header 总长度（字节） */
    uint8_t  seg_left;      /* 剩余段数（每跳递减，不在 HMAC 覆盖内） */
    uint8_t  seg_count;     /* 路径总段数 */
    uint8_t  action;        /* 动作码（不在 HMAC 覆盖内） */
    uint8_t  reserved[2];   /* 保留，置零 */

    /* 可扩展部分 (8 bytes) */
    uint8_t  hmac_id[2];    /* HMAC 密钥标识（网络序） */
    uint8_t  path_id[2];    /* 路径标识（网络序） */
    uint8_t  timestamp[4];  /* 发送时间戳，秒级（网络序） */

    /* 段列表 (seg_count * 6 bytes) */
    sr_segment_t segments[SR_MAX_SEGS];  /* 实际使用 seg_count 个 */

    /*
     * HMAC (8 bytes)。
     * 线格式中 HMAC 紧跟在最后一个「已使用」段之后（偏移 16 + seg_count*6），
     * 请统一通过 sr_hdr_get_hmac() 访问；此处的 hmac[] 字段用于保证
     * 结构体尺寸足以容纳 seg_count == SR_MAX_SEGS 时的最大帧（含 HMAC），
     * 避免栈上声明 sr_header_t 时签名写入越界。
     */
    uint8_t  hmac[SR_HMAC_LEN];
} sr_header_t;

/* 编译期断言：确保 packed 布局无填充，偏移符合线格式 */
_Static_assert(sizeof(sr_segment_t) == SR_SEG_SIZE, "sr_segment_t must be 6 bytes");
_Static_assert(offsetof(sr_header_t, segments) == SR_FIXED_HDR_LEN,
               "segments must start at offset 16");
_Static_assert(offsetof(sr_header_t, hmac) == SR_FIXED_HDR_LEN + SR_MAX_SEGS * SR_SEG_SIZE,
               "hmac must follow the max-size segment array");
_Static_assert(sizeof(sr_header_t) == SR_HDR_MAX_LEN,
               "sr_header_t must be able to hold a max-size frame including HMAC");

/* ==================== 辅助函数 ==================== */

/* 计算给定段数的 SR Header 总长度（= 序列化字节数） */
static inline uint16_t sr_hdr_total_len(uint8_t seg_count)
{
    return (uint16_t)SR_HDR_LEN(seg_count);
}

/* 序列化后实际字节数（用于 memcpy 到发送 buffer），与 sr_hdr_total_len 等价 */
static inline uint16_t sr_hdr_serialize_len(uint8_t seg_count)
{
    return (uint16_t)SR_HDR_LEN(seg_count);
}

/* 获取 HMAC 指针：HMAC 紧跟在 seg_count 个段之后 */
static inline uint8_t *sr_hdr_get_hmac(sr_header_t *h)
{
    return (uint8_t *)h->segments + (uint16_t)h->seg_count * SR_SEG_SIZE;
}

static inline const uint8_t *sr_hdr_get_hmac_c(const sr_header_t *h)
{
    return (const uint8_t *)h->segments + (uint16_t)h->seg_count * SR_SEG_SIZE;
}

/* 获取第 index 段（0 <= index < seg_count），越界返回 NULL */
static inline sr_segment_t *sr_hdr_get_segment(sr_header_t *h, int index)
{
    if (index < 0 || index >= h->seg_count)
        return NULL;
    return &h->segments[index];
}

/*
 * 初始化 SR Header：
 *   magic/ver_flags 置固定值，seg_left = seg_count，
 *   action = FORWARD，其余字段（含 reserved 和段列表）清零。
 * 返回 0 成功，-1 参数非法。
 */
static inline int sr_hdr_init(sr_header_t *h, uint8_t seg_count)
{
    if (!h || seg_count == 0 || seg_count > SR_MAX_SEGS)
        return -1;

    memset(h, 0, sizeof(*h));
    h->magic     = SR_MAGIC;
    h->ver_flags = SR_VER_FLAGS;
    h->hdr_len   = (uint8_t)SR_HDR_LEN(seg_count);
    h->seg_left  = seg_count;
    h->seg_count = seg_count;
    h->action    = SR_ACTION_FORWARD;
    return 0;
}

/*
 * 设置第 index 段。ip_net / port_net 必须已是网络序
 * （本函数按字节原样拷贝，不做转换）。
 * 返回 0 成功，-1 参数非法。
 */
static inline int sr_hdr_set_segment(sr_header_t *h, int index,
                                     uint32_t ip_net, uint16_t port_net)
{
    if (!h || index < 0 || index >= h->seg_count)
        return -1;

    memcpy(h->segments[index].ip, &ip_net, 4);
    memcpy(h->segments[index].port, &port_net, 2);
    return 0;
}

/* 设置 hmac_id（主机序入参，内部转网络序） */
static inline void sr_hdr_set_hmac_id(sr_header_t *h, uint16_t id)
{
    uint16_t net = htons(id);
    memcpy(h->hmac_id, &net, 2);
}

/* 设置 path_id（主机序入参，内部转网络序） */
static inline void sr_hdr_set_path_id(sr_header_t *h, uint16_t id)
{
    uint16_t net = htons(id);
    memcpy(h->path_id, &net, 2);
}

/* 设置 timestamp（主机序入参，内部转网络序） */
static inline void sr_hdr_set_timestamp(sr_header_t *h, uint32_t ts)
{
    uint32_t net = htonl(ts);
    memcpy(h->timestamp, &net, 4);
}

/* 读取 hmac_id（网络序转主机序） */
static inline uint16_t sr_hdr_get_hmac_id(const sr_header_t *h)
{
    uint16_t net;
    memcpy(&net, h->hmac_id, 2);
    return ntohs(net);
}

/* 读取 path_id（网络序转主机序） */
static inline uint16_t sr_hdr_get_path_id(const sr_header_t *h)
{
    uint16_t net;
    memcpy(&net, h->path_id, 2);
    return ntohs(net);
}

/* 读取 timestamp（网络序转主机序） */
static inline uint32_t sr_hdr_get_timestamp(const sr_header_t *h)
{
    uint32_t net;
    memcpy(&net, h->timestamp, 4);
    return ntohl(net);
}

/*
 * 基础验证（不做 HMAC 校验）：
 *   - magic == 0x53
 *   - version == SR_VERSION
 *   - 1 <= seg_count <= SR_MAX_SEGS
 *   - hdr_len 与 seg_count 一致
 *   - seg_left <= seg_count
 *   - buf_len 足以容纳完整头部
 * 返回 0 通过，-1 失败。
 */
static inline int sr_hdr_validate(const sr_header_t *h, uint16_t buf_len)
{
    if (!h)
        return -1;
    if (buf_len < SR_FIXED_HDR_LEN)
        return -1;
    if (h->magic != SR_MAGIC)
        return -1;
    if ((h->ver_flags & SR_VER_MASK) != (SR_VERSION << 4))
        return -1;
    if (h->seg_count == 0 || h->seg_count > SR_MAX_SEGS)
        return -1;
    if (h->hdr_len != (uint8_t)SR_HDR_LEN(h->seg_count))
        return -1;
    if (h->seg_left > h->seg_count)
        return -1;
    if (buf_len < h->hdr_len)
        return -1;
    return 0;
}

/*
 * 获取下一跳段指针（不修改 seg_left）。
 *
 * 段列表语义：segments[0] = 最终目的节点，
 *            segments[seg_count-1] = 第一跳中转（外层 UDP dst）。
 * 中转处理时下一跳为 segments[seg_left - 1]。
 *
 * 返回段指针；seg_left == 0（已到终点）或参数非法时返回 NULL。
 */
static inline sr_segment_t *sr_hdr_get_next_hop(const sr_header_t *h)
{
    if (!h || h->seg_left == 0 || h->seg_left > h->seg_count)
        return NULL;
    return (sr_segment_t *)&h->segments[h->seg_left - 1];
}

/*
 * 弹出一段（中转处理）：将当前下一跳 segments[seg_left-1]
 * 拷贝到 out，然后 seg_left--。
 * 返回 0 成功，-1 失败（seg_left == 0，段已耗尽）。
 * 注意：seg_left 不在 HMAC 覆盖范围内，弹出后无需重签。
 */
static inline int sr_hdr_pop_segment(sr_header_t *h, sr_segment_t *out)
{
    sr_segment_t *next;

    if (!h || !out)
        return -1;

    next = sr_hdr_get_next_hop(h);
    if (!next)
        return -1;

    *out = *next;
    h->seg_left--;
    return 0;
}

#ifdef __cplusplus
}
#endif

#endif /* SR_HEADER_H */
