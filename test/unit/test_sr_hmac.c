/*
 * test_sr_hmac.c - SipHash-2-4 签名/验证单元测试
 *
 * 覆盖：签名-验证往返、整帧跨实现向量、各字段篡改检测、非覆盖字段
 *       （seg_left/action）修改后仍应通过、错误密钥、seg_count 变更检测。
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
/* 跨实现固定向量：整帧 36 字节必须逐位等于注入脚本的产出                 */
/*                                                                     */
/* 由 test/scripts/inject-sr-packet.py（Python 独立实现）算出：          */
/*   key       = 00112233445566778899aabbccddeeff                      */
/*   2 段路径  = 10.10.2.2:51820 -> 10.10.1.3:51821                    */
/*   hmac_id=1 path_id=1 timestamp=1700000000 seg_left=1               */
/*   tag       = 55d187584c944f15（大端写入 HMAC 字段）                 */
/* 缺这条时 C 与注入脚本各算各的：集成测试会静默判 hmac_fail，            */
/* 而单测仍全绿（roundtrip 自洽发现不了原语/字段顺序漂移）。               */
/* ------------------------------------------------------------------ */
static void test_hmac_wire_vector_cross_impl(void)
{
    static const uint8_t key[SR_PKEY_LEN] = {
        0x00,0x11,0x22,0x33,0x44,0x55,0x66,0x77,
        0x88,0x99,0xaa,0xbb,0xcc,0xdd,0xee,0xff
    };
    static const uint8_t wire_exp[36] = {
        0x53,0x10,0x24,0x01,0x02,0x00,0x00,0x00,
        0x00,0x01,0x00,0x01,0x65,0x53,0xf1,0x00,
        0x0a,0x0a,0x02,0x02,0xca,0x6c,0x0a,0x0a,
        0x01,0x03,0xca,0x6d,0x55,0xd1,0x87,0x58,
        0x4c,0x94,0x4f,0x15
    };
    uint8_t wire[36];
    sr_header_t h;

    sr_hdr_init(&h, 2);
    sr_hdr_set_hmac_id(&h, 0x0001);
    sr_hdr_set_path_id(&h, 0x0001);
    sr_hdr_set_timestamp(&h, 1700000000u);
    sr_hdr_set_segment(&h, 0, htonl(0x0A0A0202), htons(51820));
    sr_hdr_set_segment(&h, 1, htonl(0x0A0A0103), htons(51821));
    h.seg_left = 1;

    TEST_ASSERT_EQ(sr_hmac_sign(&h, key), 0, "签名应成功");

    memcpy(wire, &h, 16);
    memcpy(wire + 16, h.segments, 2 * SR_SEG_SIZE);
    memcpy(wire + 16 + 2 * SR_SEG_SIZE, sr_hdr_get_hmac_c(&h), SR_HMAC_LEN);

    TEST_ASSERT_MEM_EQ(wire, wire_exp, sizeof(wire_exp),
                       "整帧 36 字节应逐位等于 Python 注入脚本的产出");
    TEST_ASSERT_EQ(sr_hmac_verify(&h, key), 0, "用自己签的名验证应通过");
}

/* ------------------------------------------------------------------ */
/* 套件入口                                                             */
/* ------------------------------------------------------------------ */
void test_suite_sr_hmac(void)
{
    TEST_SUITE_BEGIN("SR HMAC (sr_hmac.c)");
    RUN_TEST(test_hmac_sign_verify_roundtrip);
    RUN_TEST(test_hmac_wire_vector_cross_impl);
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
