/*
 * test_sr_hmac.c - HMAC-SHA256 签名/验证单元测试
 *
 * 覆盖：签名-验证往返、各字段篡改检测、非覆盖字段（seg_left/action）
 *       修改后仍应通过、错误密钥、seg_count 变更检测。
 *
 * HMAC 覆盖范围：ver_flags, hdr_len, seg_count, reserved,
 *                hmac_id, path_id, timestamp, segments[0..N-1]
 * 排除字段：magic, seg_left, action
 */
#include "test_framework.h"
#include "sr_header.h"
#include "sr_hmac.h"

void test_suite_sr_hmac(void);

/* 固定测试密钥 A：全 0x41 的 16 字节 */
static const uint8_t g_pkey_a[SR_PKEY_LEN] = {
    0x41,0x41,0x41,0x41,0x41,0x41,0x41,0x41,
    0x41,0x41,0x41,0x41,0x41,0x41,0x41,0x41
};

/* 固定测试密钥 B：全 0x42 的 16 字节（与 A 不同） */
static const uint8_t g_pkey_b[SR_PKEY_LEN] = {
    0x42,0x42,0x42,0x42,0x42,0x42,0x42,0x42,
    0x42,0x42,0x42,0x42,0x42,0x42,0x42,0x42
};

/*
 * 构造一个 3 段路径头（已设置各字段），但不签名。
 * 段列表：segments[0]=10.0.0.1, [1]=10.0.0.2, [2]=10.0.0.3
 */
static void build_hdr_3seg(sr_header_t *h)
{
    uint16_t pt = htons(SR_PORT_TRANSIT);
    sr_hdr_init(h, 3);
    sr_hdr_set_hmac_id(h, 0x0001);
    sr_hdr_set_path_id(h, 0x1234);
    sr_hdr_set_timestamp(h, 0x60000000u);
    sr_hdr_set_segment(h, 0, htonl(0x0A000001), pt);
    sr_hdr_set_segment(h, 1, htonl(0x0A000002), pt);
    sr_hdr_set_segment(h, 2, htonl(0x0A000003), pt);
}

/* ------------------------------------------------------------------ */
/* test_hmac_sign_verify_roundtrip：签名后用同一 P-Key 验证应成功         */
/* ------------------------------------------------------------------ */
static void test_hmac_sign_verify_roundtrip(void)
{
    sr_header_t h;
    build_hdr_3seg(&h);

    TEST_ASSERT_EQ(sr_hmac_sign(&h, g_pkey_a), 0, "签名应成功");

    /* 签名后 HMAC 字段应非全零（说明确实写入了摘要） */
    {
        const uint8_t zeros[SR_HMAC_LEN] = {0};
        const uint8_t *hm = sr_hdr_get_hmac_c(&h);
        TEST_ASSERT(memcmp(hm, zeros, SR_HMAC_LEN) != 0,
                    "签名后 HMAC 字段应非全零");
    }

    TEST_ASSERT_EQ(sr_hmac_verify(&h, g_pkey_a), 0, "同密钥验证应成功");

    /* 非法参数 */
    TEST_ASSERT_EQ(sr_hmac_sign(NULL, g_pkey_a), -1, "h=NULL 签名应失败");
    TEST_ASSERT_EQ(sr_hmac_verify(NULL, g_pkey_a), -1, "h=NULL 验证应失败");
}

/* ------------------------------------------------------------------ */
/* test_hmac_tamper_segment：签名后修改段 IP，验证应失败                 */
/* ------------------------------------------------------------------ */
static void test_hmac_tamper_segment(void)
{
    sr_header_t h;
    build_hdr_3seg(&h);
    TEST_ASSERT_EQ(sr_hmac_sign(&h, g_pkey_a), 0, "签名应成功");
    TEST_ASSERT_EQ(sr_hmac_verify(&h, g_pkey_a), 0, "篡改前验证应成功");

    /* 修改 segments[0] 的 IP（在覆盖范围内） */
    h.segments[0].ip[3] = (uint8_t)(h.segments[0].ip[3] ^ 0xFF);
    TEST_ASSERT_EQ(sr_hmac_verify(&h, g_pkey_a), -1,
                   "篡改段 IP 后验证应失败");
}

/* ------------------------------------------------------------------ */
/* test_hmac_tamper_path_id：签名后修改 path_id，验证应失败             */
/* ------------------------------------------------------------------ */
static void test_hmac_tamper_path_id(void)
{
    sr_header_t h;
    build_hdr_3seg(&h);
    TEST_ASSERT_EQ(sr_hmac_sign(&h, g_pkey_a), 0, "签名应成功");

    /* path_id 在覆盖范围内，修改后应失败 */
    sr_hdr_set_path_id(&h, 0x9999);
    TEST_ASSERT_EQ(sr_hmac_verify(&h, g_pkey_a), -1,
                   "篡改 path_id 后验证应失败");
}

/* ------------------------------------------------------------------ */
/* test_hmac_tamper_timestamp：签名后修改 timestamp，验证应失败         */
/* ------------------------------------------------------------------ */
static void test_hmac_tamper_timestamp(void)
{
    sr_header_t h;
    build_hdr_3seg(&h);
    TEST_ASSERT_EQ(sr_hmac_sign(&h, g_pkey_a), 0, "签名应成功");

    /* timestamp 在覆盖范围内 */
    sr_hdr_set_timestamp(&h, 0x60000001u);
    TEST_ASSERT_EQ(sr_hmac_verify(&h, g_pkey_a), -1,
                   "篡改 timestamp 后验证应失败");
}

/* ------------------------------------------------------------------ */
/* test_hmac_tamper_hmac_id：签名后修改 hmac_id，验证应失败             */
/* ------------------------------------------------------------------ */
static void test_hmac_tamper_hmac_id(void)
{
    sr_header_t h;
    build_hdr_3seg(&h);
    TEST_ASSERT_EQ(sr_hmac_sign(&h, g_pkey_a), 0, "签名应成功");
    TEST_ASSERT_EQ(sr_hmac_verify(&h, g_pkey_a), 0, "篡改前验证应成功");

    /* hmac_id 在覆盖范围内，修改后应失败 */
    sr_hdr_set_hmac_id(&h, 0x0002);
    TEST_ASSERT_EQ(sr_hmac_verify(&h, g_pkey_a), -1,
                   "篡改 hmac_id 后验证应失败");
}

/* ------------------------------------------------------------------ */
/* test_hmac_seg_left_not_covered：修改 seg_left 后验证应成功           */
/* ------------------------------------------------------------------ */
static void test_hmac_seg_left_not_covered(void)
{
    sr_header_t h;
    build_hdr_3seg(&h);
    TEST_ASSERT_EQ(sr_hmac_sign(&h, g_pkey_a), 0, "签名应成功");

    /* seg_left 不在覆盖范围内（每跳递减） */
    h.seg_left = 2;
    TEST_ASSERT_EQ(sr_hmac_verify(&h, g_pkey_a), 0,
                   "修改 seg_left 后验证仍应成功");
    h.seg_left = 1;
    TEST_ASSERT_EQ(sr_hmac_verify(&h, g_pkey_a), 0,
                   "seg_left=1 后验证仍应成功");
    h.seg_left = 0;
    TEST_ASSERT_EQ(sr_hmac_verify(&h, g_pkey_a), 0,
                   "seg_left=0 后验证仍应成功");
}

/* ------------------------------------------------------------------ */
/* test_hmac_action_not_covered：修改 action 后验证应成功               */
/* ------------------------------------------------------------------ */
static void test_hmac_action_not_covered(void)
{
    sr_header_t h;
    build_hdr_3seg(&h);
    TEST_ASSERT_EQ(sr_hmac_sign(&h, g_pkey_a), 0, "签名应成功");

    /* action 不在覆盖范围内（每跳可能变） */
    h.action = SR_ACTION_DELIVER;
    TEST_ASSERT_EQ(sr_hmac_verify(&h, g_pkey_a), 0,
                   "修改 action 后验证仍应成功");
    h.action = 0x7F;
    TEST_ASSERT_EQ(sr_hmac_verify(&h, g_pkey_a), 0,
                   "action 改任意值后验证仍应成功");
}

/* ------------------------------------------------------------------ */
/* test_hmac_wrong_key：key_A 签名，key_B 验证应失败                     */
/* ------------------------------------------------------------------ */
static void test_hmac_wrong_key(void)
{
    sr_header_t h;
    build_hdr_3seg(&h);
    TEST_ASSERT_EQ(sr_hmac_sign(&h, g_pkey_a), 0, "key_A 签名应成功");
    TEST_ASSERT_EQ(sr_hmac_verify(&h, g_pkey_a), 0, "key_A 验证应成功");
    TEST_ASSERT_EQ(sr_hmac_verify(&h, g_pkey_b), -1, "key_B 验证应失败");
}

/* ------------------------------------------------------------------ */
/* test_hmac_different_seg_count：签名后修改 seg_count，验证应失败       */
/* ------------------------------------------------------------------ */
static void test_hmac_different_seg_count(void)
{
    sr_header_t h;
    build_hdr_3seg(&h);
    TEST_ASSERT_EQ(sr_hmac_sign(&h, g_pkey_a), 0, "签名应成功");
    TEST_ASSERT_EQ(sr_hmac_verify(&h, g_pkey_a), 0, "修改前验证应成功");

    /* seg_count 在覆盖范围内；修改后覆盖数据与 HMAC 偏移都变化 → 失败 */
    h.seg_count = 4;
    TEST_ASSERT_EQ(sr_hmac_verify(&h, g_pkey_a), -1,
                   "篡改 seg_count 后验证应失败");
}

/* ------------------------------------------------------------------ */
/* 套件入口                                                             */
/* ------------------------------------------------------------------ */
void test_suite_sr_hmac(void)
{
    TEST_SUITE_BEGIN("SR HMAC (sr_hmac.c)");
    RUN_TEST(test_hmac_sign_verify_roundtrip);
    RUN_TEST(test_hmac_tamper_segment);
    RUN_TEST(test_hmac_tamper_path_id);
    RUN_TEST(test_hmac_tamper_timestamp);
    RUN_TEST(test_hmac_tamper_hmac_id);
    RUN_TEST(test_hmac_seg_left_not_covered);
    RUN_TEST(test_hmac_action_not_covered);
    RUN_TEST(test_hmac_wrong_key);
    RUN_TEST(test_hmac_different_seg_count);
    TEST_SUITE_END();
}
