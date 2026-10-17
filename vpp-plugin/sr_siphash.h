/*
 * sr_siphash.h - SipHash-2-4 原语（128-bit key → 64-bit tag）
 *
 * 用于 SR Header 完整性保护，替代 HMAC-SHA256 截断方案：
 * 无 OpenSSL 依赖、纯 C11、单消息 64 位输出与线上 8 字节 HMAC 字段等宽。
 */
#ifndef SR_SIPHASH_H
#define SR_SIPHASH_H

#include <stdint.h>
#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

#define SR_SIPHASH_KEY_LEN  16   /* 128-bit key */
#define SR_SIPHASH_TAG_LEN   8   /*  64-bit tag */

/*
 * 计算 SipHash-2-4。
 *
 * key 按字节小端组装为 k0/k1（与主机字节序无关），输入消息按字节小端
 * 分块，因此同一 (key, msg) 在任何平台上得到同一 tag。
 */
uint64_t sr_siphash24(const uint8_t *msg, size_t msg_len,
                      const uint8_t key[SR_SIPHASH_KEY_LEN]);

#ifdef __cplusplus
}
#endif

#endif /* SR_SIPHASH_H */
