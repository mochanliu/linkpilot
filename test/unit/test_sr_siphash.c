/*
 * test_sr_siphash.c - SipHash-2-4 原语单元测试
 *
 * 权威向量取自论文《SipHash: a fast short-input PRF》附录（附录 A）：
 *   key  = 00 01 02 ... 0f
 *   msg  = 00 01 02 ... 0e            (15 字节)
 *   tag  = a129ca6149be45e5           (u64)
 *          字节序输出为 e5 45 be 49 61 ca 29 a1
 */
#include "test_framework.h"
#include "sr_siphash.h"

void test_suite_sr_siphash(void);

/* 论文附录测试密钥：00 01 02 ... 0f */
static void make_paper_key(uint8_t key[SR_SIPHASH_KEY_LEN])
{
    int i;
    for (i = 0; i < SR_SIPHASH_KEY_LEN; i++)
        key[i] = (uint8_t)i;
}

/* ------------------------------------------------------------------ */
/* KAT：论文附录向量（15 字节输入）                                     */
/* ------------------------------------------------------------------ */
static void test_siphash_known_answer(void)
{
    uint8_t key[SR_SIPHASH_KEY_LEN], msg[15];
    uint64_t tag;
    int i;

    make_paper_key(key);
    for (i = 0; i < 15; i++)
        msg[i] = (uint8_t)i;

    tag = sr_siphash24(msg, sizeof(msg), key);
    TEST_ASSERT_EQ(tag, 0xA129CA6149BE45E5ULL,
                   "论文附录向量（15B 输入）应得到 a129ca6149be45e5");
}

/* ------------------------------------------------------------------ */
/* KAT：官方 len 0..63 全表                                             */
/*                                                                     */
/* 来源：SipHash 上游参考实现随附的 vectors_sip64（key=00..0f，          */
/* msg=00..(len-1)）。整表钉死而不是只钉 len=15 一条：v3 init 常量抄错   */
/* 这类错误会让**所有长度**都错，单条向量虽能抓到，但任何"只修好一个     */
/* 长度"的取巧实现抓不到。                                               */
/* ------------------------------------------------------------------ */
static const uint64_t kSip64Official[64] = {
    0x726fdb47dd0e0e31ULL, 0x74f839c593dc67fdULL, 0x0d6c8009d9a94f5aULL, 0x85676696d7fb7e2dULL,
    0xcf2794e0277187b7ULL, 0x18765564cd99a68dULL, 0xcbc9466e58fee3ceULL, 0xab0200f58b01d137ULL,
    0x93f5f5799a932462ULL, 0x9e0082df0ba9e4b0ULL, 0x7a5dbbc594ddb9f3ULL, 0xf4b32f46226bada7ULL,
    0x751e8fbc860ee5fbULL, 0x14ea5627c0843d90ULL, 0xf723ca908e7af2eeULL, 0xa129ca6149be45e5ULL,
    0x3f2acc7f57c29bdbULL, 0x699ae9f52cbe4794ULL, 0x4bc1b3f0968dd39cULL, 0xbb6dc91da77961bdULL,
    0xbed65cf21aa2ee98ULL, 0xd0f2cbb02e3b67c7ULL, 0x93536795e3a33e88ULL, 0xa80c038ccd5ccec8ULL,
    0xb8ad50c6f649af94ULL, 0xbce192de8a85b8eaULL, 0x17d835b85bbb15f3ULL, 0x2f2e6163076bcfadULL,
    0xde4daaaca71dc9a5ULL, 0xa6a2506687956571ULL, 0xad87a3535c49ef28ULL, 0x32d892fad841c342ULL,
    0x7127512f72f27cceULL, 0xa7f32346f95978e3ULL, 0x12e0b01abb051238ULL, 0x15e034d40fa197aeULL,
    0x314dffbe0815a3b4ULL, 0x027990f029623981ULL, 0xcadcd4e59ef40c4dULL, 0x9abfd8766a33735cULL,
    0x0e3ea96b5304a7d0ULL, 0xad0c42d6fc585992ULL, 0x187306c89bc215a9ULL, 0xd4a60abcf3792b95ULL,
    0xf935451de4f21df2ULL, 0xa9538f0419755787ULL, 0xdb9acddff56ca510ULL, 0xd06c98cd5c0975ebULL,
    0xe612a3cb9ecba951ULL, 0xc766e62cfcadaf96ULL, 0xee64435a9752fe72ULL, 0xa192d576b245165aULL,
    0x0a8787bf8ecb74b2ULL, 0x81b3e73d20b49b6fULL, 0x7fa8220ba3b2eceaULL, 0x245731c13ca42499ULL,
    0xb78dbfaf3a8d83bdULL, 0xea1ad565322a1a0bULL, 0x60e61c23a3795013ULL, 0x6606d7e446282b93ULL,
    0x6ca4ecb15c5f91e1ULL, 0x9f626da15c9625f3ULL, 0xe51b38608ef25f57ULL, 0x958a324ceb064572ULL
};

static void test_siphash_official_vectors_len0_63(void)
{
    uint8_t key[SR_SIPHASH_KEY_LEN];
    uint8_t msg[64];
    int len, i, bad = 0, first_bad = -1;

    make_paper_key(key);
    for (i = 0; i < 64; i++)
        msg[i] = (uint8_t)i;

    for (len = 0; len < 64; len++) {
        if (sr_siphash24(msg, (size_t)len, key) != kSip64Official[len]) {
            if (first_bad < 0)
                first_bad = len;
            bad++;
        }
    }

    TEST_ASSERT_EQ(bad, 0, "len 0..63 必须全部命中官方向量");
    (void)first_bad;
}

/* ------------------------------------------------------------------ */
/* KAT：与注入脚本 inject-sr-packet.py 的跨实现一致性                    */
/*                                                                     */
/* 覆盖缓冲 = ver_flags||hdr_len||seg_count||reserved||hmac_id||        */
/*            path_id||timestamp||segments                              */
/* key  = 00112233445566778899aabbccddeeff                              */
/* cover= 1024020000000100016553f1000a0a0202ca6c0a0a0103ca6d (25B)      */
/* tag  = 0x55d187584c944f15                                            */
/* 这条不是自证：由 Python 独立实现算出，改 C 侧任何字段顺序都会红。      */
/* ------------------------------------------------------------------ */
static void test_siphash_cross_impl_vector(void)
{
    static const uint8_t key[16] = {
        0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
        0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff
    };
    static const uint8_t cover[25] = {
        0x10, 0x24, 0x02, 0x00, 0x00, 0x00, 0x01, 0x00,
        0x01, 0x65, 0x53, 0xf1, 0x00, 0x0a, 0x0a, 0x02,
        0x02, 0xca, 0x6c, 0x0a, 0x0a, 0x01, 0x03, 0xca,
        0x6d
    };

    TEST_ASSERT_EQ(sr_siphash24(cover, sizeof(cover), key),
                   0x55D187584C944F15ULL,
                   "跨实现向量（与 inject-sr-packet.py 同输入）应得 55d187584c944f15");
}

/* ------------------------------------------------------------------ */
/* KAT：空输入（长度 0，仅长度块参与）                                  */
/* ------------------------------------------------------------------ */
static void test_siphash_empty_input_deterministic(void)
{
    uint8_t key[SR_SIPHASH_KEY_LEN];
    uint64_t a, b;

    make_paper_key(key);
    a = sr_siphash24(NULL, 0, key);
    b = sr_siphash24((const uint8_t *)"", 0, key);
    TEST_ASSERT_EQ(a, b, "空输入的 tag 应与传入指针无关");
    TEST_ASSERT_NEQ(a, 0u, "空输入不应退化为全零 tag");
}

/* ------------------------------------------------------------------ */
/* 分组边界：8 字节整块 / 尾部 1..7 字节 / 跨越多个块都能覆盖            */
/* ------------------------------------------------------------------ */
static void test_siphash_block_boundaries(void)
{
    uint8_t key[SR_SIPHASH_KEY_LEN];
    uint8_t msg[24];
    uint64_t prev[3];
    int i;

    make_paper_key(key);
    for (i = 0; i < 24; i++)
        msg[i] = (uint8_t)(i * 7 + 1);

    /* 长度 7/8/9 分别落在「仅尾块」「恰好整块」「整块 + 尾块」三种路径 */
    prev[0] = sr_siphash24(msg, 7, key);
    prev[1] = sr_siphash24(msg, 8, key);
    prev[2] = sr_siphash24(msg, 9, key);

    TEST_ASSERT_NEQ(prev[0], prev[1], "长度 7 与 8 的 tag 应不同");
    TEST_ASSERT_NEQ(prev[1], prev[2], "长度 8 与 9 的 tag 应不同");

    /* 长度写入尾块高字节：同一前缀加长必须改变结果 */
    TEST_ASSERT_NEQ(sr_siphash24(msg, 16, key), sr_siphash24(msg, 24, key),
                    "前缀相同但长度不同应得到不同 tag");
}

/* ------------------------------------------------------------------ */
/* 确定性与雪崩：同输入同结果；单比特翻转改变结果                        */
/* ------------------------------------------------------------------ */
static void test_siphash_determinism_and_avalanche(void)
{
    uint8_t key[SR_SIPHASH_KEY_LEN];
    uint8_t msg[13];
    uint64_t base, again, flipped;
    int i, bits = 0;
    uint64_t x;

    make_paper_key(key);
    for (i = 0; i < 13; i++)
        msg[i] = (uint8_t)(0x10 + i);

    base = sr_siphash24(msg, sizeof(msg), key);
    again = sr_siphash24(msg, sizeof(msg), key);
    TEST_ASSERT_EQ(base, again, "同一输入应得到同一 tag");

    msg[0] ^= 0x01;
    flipped = sr_siphash24(msg, sizeof(msg), key);
    TEST_ASSERT_NEQ(base, flipped, "单比特输入变化必须改变 tag");

    x = base ^ flipped;
    for (i = 0; i < 64; i++)
        bits += (int)((x >> i) & 1u);
    TEST_ASSERT(bits >= 16, "输入单比特变化应在输出翻转至少 16 个比特（雪崩）");
}

/* ------------------------------------------------------------------ */
/* 密钥敏感性：换密钥必须改变结果                                       */
/* ------------------------------------------------------------------ */
static void test_siphash_key_sensitivity(void)
{
    uint8_t k1[SR_SIPHASH_KEY_LEN], k2[SR_SIPHASH_KEY_LEN];
    uint8_t msg[13];
    int i;

    for (i = 0; i < SR_SIPHASH_KEY_LEN; i++) {
        k1[i] = (uint8_t)i;
        k2[i] = (uint8_t)(i + 1);
    }
    for (i = 0; i < 13; i++)
        msg[i] = (uint8_t)i;

    TEST_ASSERT_NEQ(sr_siphash24(msg, sizeof(msg), k1),
                    sr_siphash24(msg, sizeof(msg), k2),
                    "不同密钥应得到不同 tag");
}

/* ------------------------------------------------------------------ */
/* 套件入口                                                             */
/* ------------------------------------------------------------------ */
void test_suite_sr_siphash(void)
{
    TEST_SUITE_BEGIN("SipHash-2-4 (sr_siphash.c)");
    RUN_TEST(test_siphash_known_answer);
    RUN_TEST(test_siphash_official_vectors_len0_63);
    RUN_TEST(test_siphash_cross_impl_vector);
    RUN_TEST(test_siphash_empty_input_deterministic);
    RUN_TEST(test_siphash_block_boundaries);
    RUN_TEST(test_siphash_determinism_and_avalanche);
    RUN_TEST(test_siphash_key_sensitivity);
    TEST_SUITE_END();
}
