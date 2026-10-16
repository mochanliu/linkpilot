/*
 * sr_hmac.c - SR Header HMAC-SHA256（截断 64 位）实现
 *
 * 覆盖范围按线格式字段顺序，使用 HMAC_Update 分段输入，
 * 显式跳过 magic(offset 0)、seg_left(offset 3)、action(offset 5)。
 *
 * 覆盖字节序列：
 *   ver_flags(1) || hdr_len(1) || seg_count(1) || reserved(2) ||
 *   hmac_id(2) || path_id(2) || timestamp(4) || segments[0..N-1](N*6)
 */
#include "sr_hmac.h"

#include <openssl/hmac.h>
#include <openssl/evp.h>

/* HMAC-SHA256 完整输出长度 */
#define SR_HMAC_SHA256_LEN  32

/*
 * 对头部覆盖字段计算 HMAC-SHA256，输出完整 32 字节到 out。
 * 返回 0 成功，-1 失败。
 */
static int sr_hmac_compute(const sr_header_t *h,
                           const uint8_t pkey[SR_PKEY_LEN],
                           uint8_t out[SR_HMAC_SHA256_LEN])
{
    HMAC_CTX *ctx = NULL;
    unsigned int out_len = 0;
    int ret = -1;

    if (!h || !pkey || !out)
        return -1;
    if (h->seg_count == 0 || h->seg_count > SR_MAX_SEGS)
        return -1;

    ctx = HMAC_CTX_new();
    if (!ctx)
        goto done;

    if (HMAC_Init_ex(ctx, pkey, SR_PKEY_LEN, EVP_sha256(), NULL) != 1)
        goto done;

    /* 逐字段输入覆盖范围，跳过 magic / seg_left / action */
    if (HMAC_Update(ctx, &h->ver_flags, 1) != 1)   /* ver_flags */
        goto done;
    if (HMAC_Update(ctx, &h->hdr_len, 1) != 1)      /* hdr_len */
        goto done;
    if (HMAC_Update(ctx, &h->seg_count, 1) != 1)    /* seg_count */
        goto done;
    if (HMAC_Update(ctx, h->reserved, 2) != 1)      /* reserved */
        goto done;
    if (HMAC_Update(ctx, h->hmac_id, 2) != 1)       /* hmac_id */
        goto done;
    if (HMAC_Update(ctx, h->path_id, 2) != 1)       /* path_id */
        goto done;
    if (HMAC_Update(ctx, h->timestamp, 4) != 1)     /* timestamp */
        goto done;
    /* segments[0..seg_count-1]，仅覆盖实际使用的段 */
    if (HMAC_Update(ctx, (const uint8_t *)h->segments,
                    (size_t)h->seg_count * SR_SEG_SIZE) != 1)
        goto done;

    if (HMAC_Final(ctx, out, &out_len) != 1)
        goto done;
    if (out_len != SR_HMAC_SHA256_LEN)
        goto done;

    ret = 0;

done:
    HMAC_CTX_free(ctx);
    return ret;
}

int sr_hmac_sign(sr_header_t *h, const uint8_t pkey[SR_PKEY_LEN])
{
    uint8_t digest[SR_HMAC_SHA256_LEN];
    uint8_t *hmac_field;

    if (!h)
        return -1;

    if (sr_hmac_compute(h, pkey, digest) != 0)
        return -1;

    /* 截断：取前 SR_HMAC_LEN(8) 字节写入头部 */
    hmac_field = sr_hdr_get_hmac(h);
    memcpy(hmac_field, digest, SR_HMAC_LEN);
    return 0;
}

int sr_hmac_verify(const sr_header_t *h, const uint8_t pkey[SR_PKEY_LEN])
{
    uint8_t digest[SR_HMAC_SHA256_LEN];
    const uint8_t *hmac_field;
    uint8_t diff = 0;
    int i;

    if (!h)
        return -1;

    if (sr_hmac_compute(h, pkey, digest) != 0)
        return -1;

    /* 常量时间比较，防止 timing attack */
    hmac_field = sr_hdr_get_hmac_c(h);
    for (i = 0; i < SR_HMAC_LEN; i++)
        diff |= (uint8_t)(digest[i] ^ hmac_field[i]);

    return (diff == 0) ? 0 : -1;
}
