/*
 * sr_siphash.c - SipHash-2-4 参考算法实现（Aumasson / Bernstein）
 *
 * 全部按字节显式组装/拆分，不做整字强转，因此：
 *   - 无对齐要求，可安全用于 packed 线格式字段；
 *   - 输出与主机字节序无关（大端/小端主机结果一致），可上线。
 */
#include "sr_siphash.h"

#define SR_SIPROTATE(x, b) (((x) << (b)) | ((x) >> (64 - (b))))

#define SR_SIPROUND()                                                      \
    do {                                                                   \
        v0 += v1; v1 = SR_SIPROTATE(v1, 13); v1 ^= v0; v0 = SR_SIPROTATE(v0, 32); \
        v2 += v3; v3 = SR_SIPROTATE(v3, 16); v3 ^= v2;                     \
        v0 += v3; v3 = SR_SIPROTATE(v3, 21); v3 ^= v0;                     \
        v2 += v1; v1 = SR_SIPROTATE(v1, 17); v1 ^= v2; v2 = SR_SIPROTATE(v2, 32); \
    } while (0)

/* 小端读 8 字节 */
static uint64_t sr_u64_le(const uint8_t *p)
{
    uint64_t v = 0;
    int i;
    for (i = 7; i >= 0; i--)
        v = (v << 8) | p[i];
    return v;
}

uint64_t sr_siphash24(const uint8_t *msg, size_t msg_len,
                      const uint8_t key[SR_SIPHASH_KEY_LEN])
{
    uint64_t k0 = sr_u64_le(key);
    uint64_t k1 = sr_u64_le(key + 8);
    uint64_t v0 = k0 ^ 0x736f6d6570736575ULL;
    uint64_t v1 = k1 ^ 0x646f72616e646f6dULL;
    uint64_t v2 = k0 ^ 0x6c7967656e657261ULL;
    uint64_t v3 = k1 ^ 0x7465646279746573ULL;  /* "tedbytes" */
    const uint8_t *end = msg + (msg_len & ~7UL);
    uint64_t b = (uint64_t)msg_len << 56;
    const uint8_t *p;

    if (!msg && msg_len)
        return 0;

    for (p = msg; p < end; p += 8) {
        uint64_t m = sr_u64_le(p);
        v3 ^= m;
        SR_SIPROUND();
        SR_SIPROUND();
        v0 ^= m;
    }

    for (p = end; p < msg + msg_len; p++)
        b |= (uint64_t)*p << ((size_t)(p - end) * 8);

    v3 ^= b;
    SR_SIPROUND();
    SR_SIPROUND();
    v0 ^= b;

    v2 ^= 0xff;
    SR_SIPROUND();
    SR_SIPROUND();
    SR_SIPROUND();
    SR_SIPROUND();

    return v0 ^ v1 ^ v2 ^ v3;
}
