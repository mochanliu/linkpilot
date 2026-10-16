/*
 * sr_hmac.h - SR Header HMAC 签名/验证接口
 *
 * 使用 HMAC-SHA256 截断为 64 位保护 SR Header 完整性。
 * P-Key：128-bit 预共享路径密钥（每条路径独立）。
 *
 * HMAC 覆盖范围：
 *   ver_flags, hdr_len, seg_count, reserved,
 *   hmac_id, path_id, timestamp, segments[0..N-1]
 * 排除字段：
 *   magic（固定值 0x53）, seg_left（每跳递减）, action（每跳可变）
 */
#ifndef SR_HMAC_H
#define SR_HMAC_H

#include "sr_header.h"

#ifdef __cplusplus
extern "C" {
#endif

#define SR_PKEY_LEN  16   /* 128-bit 预共享路径密钥 */

/*
 * 计算并写入 HMAC（签名）。
 * 覆盖范围：ver_flags, hdr_len, seg_count, reserved, hmac_id, path_id,
 *           timestamp, segments[0..seg_count-1]
 * 排除：magic, seg_left, action
 *
 * 返回：0 成功，-1 失败（参数非法或 OpenSSL 错误）
 */
int sr_hmac_sign(sr_header_t *h, const uint8_t pkey[SR_PKEY_LEN]);

/*
 * 验证 HMAC。
 *
 * 返回：0 验证通过，-1 验证失败
 */
int sr_hmac_verify(const sr_header_t *h, const uint8_t pkey[SR_PKEY_LEN]);

#ifdef __cplusplus
}
#endif

#endif /* SR_HMAC_H */
