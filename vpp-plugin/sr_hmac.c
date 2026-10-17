/*
 * sr_hmac.c - SR Header 完整性签名（SipHash-2-4）
 *
 * 覆盖范围按线格式字段顺序拼成连续字节缓冲后一次性哈希，
 * 显式跳过 magic(offset 0)、seg_left(offset 3)、action(offset 5)。
 *
 * 覆盖字节序列：
 *   ver_flags(1) || hdr_len(1) || seg_count(1) || reserved(2) ||
 *   hmac_id(2) || path_id(2) || timestamp(4) || segments[0..N-1](N*6)
 * 最大长度 = 13 + SR_MAX_SEGS * SR_SEG_SIZE = 61
 *
 * tag 为大端（网络序）写入 8 字节 HMAC 字段，与头部其余多字节字段一致。
 */
#include "sr_hmac.h"

#include "sr_siphash.h"

/* 覆盖字段固定部分字节数 */
#define SR_HMAC_COVER_FIXED  13
#define SR_HMAC_COVER_MAX    (SR_HMAC_COVER_FIXED + SR_MAX_SEGS * SR_SEG_SIZE)

static int sr_hmac_compute(const sr_header_t *h,
                           const uint8_t pkey[SR_PKEY_LEN],
                           uint64_t *tag_out)
{
    uint8_t buf[SR_HMAC_COVER_MAX];
    size_t off = 0;

    if (!h || !pkey || !tag_out)
        return -1;
    if (h->seg_count == 0 || h->seg_count > SR_MAX_SEGS)
        return -1;

    buf[off++] = h->ver_flags;
    buf[off++] = h->hdr_len;
    buf[off++] = h->seg_count;
    memcpy(buf + off, h->reserved, 2);  off += 2;
    memcpy(buf + off, h->hmac_id, 2);   off += 2;
    memcpy(buf + off, h->path_id, 2);   off += 2;
    memcpy(buf + off, h->timestamp, 4); off += 4;
    memcpy(buf + off, (const uint8_t *)h->segments,
           (size_t)h->seg_count * SR_SEG_SIZE);
    off += (size_t)h->seg_count * SR_SEG_SIZE;

    *tag_out = sr_siphash24(buf, off, pkey);
    return 0;
}

/* 64 位 tag 按大端写入 */
static void sr_hmac_store_tag(uint8_t dst[SR_HMAC_LEN], uint64_t tag)
{
    int i;
    for (i = 0; i < SR_HMAC_LEN; i++)
        dst[i] = (uint8_t)(tag >> (56 - i * 8));
}

int sr_hmac_sign(sr_header_t *h, const uint8_t pkey[SR_PKEY_LEN])
{
    uint64_t tag;

    if (!h)
        return -1;
    if (sr_hmac_compute(h, pkey, &tag) != 0)
        return -1;

    sr_hmac_store_tag(sr_hdr_get_hmac(h), tag);
    return 0;
}

int sr_hmac_verify(const sr_header_t *h, const uint8_t pkey[SR_PKEY_LEN])
{
    uint8_t expected[SR_HMAC_LEN];
    const uint8_t *actual;
    uint64_t tag;
    uint8_t diff = 0;
    int i;

    if (!h)
        return -1;
    if (sr_hmac_compute(h, pkey, &tag) != 0)
        return -1;

    sr_hmac_store_tag(expected, tag);

    /* 常量时间比较，防止 timing attack */
    actual = sr_hdr_get_hmac_c(h);
    for (i = 0; i < SR_HMAC_LEN; i++)
        diff |= (uint8_t)(expected[i] ^ actual[i]);

    return (diff == 0) ? 0 : -1;
}
