/*
 * test_sr_path.c - 路径表管理（sr_path.c）单元测试
 *
 * 覆盖：初始化、添加、覆盖、删除、删除不存在、fill_header 生成、
 *       表满溢出。
 */
#include "test_framework.h"
#include "sr_header.h"
#include "sr_hmac.h"
#include "sr_path.h"

void test_suite_sr_path(void);

/* 固定测试 P-Key：全 0x41 的 16 字节 */
static const uint8_t g_pkey[SR_PKEY_LEN] = {
    0x41,0x41,0x41,0x41,0x41,0x41,0x41,0x41,
    0x41,0x41,0x41,0x41,0x41,0x41,0x41,0x41
};

/*
 * 构造一条 seg_count 段的路径配置。
 * 段 i 的 IP = 10.0.0.(i+1)，端口 = SR_PORT_TRANSIT，均为网络序。
 */
static void make_path_cfg(sr_path_config_t *cfg, uint16_t path_id,
                          uint8_t seg_count)
{
    int i;
    memset(cfg, 0, sizeof(*cfg));
    cfg->path_id   = path_id;
    cfg->hmac_id   = 0x0007;
    cfg->seg_count = seg_count;
    cfg->active    = 1;
    memcpy(cfg->pkey, g_pkey, SR_PKEY_LEN);
    for (i = 0; i < seg_count; i++) {
        uint32_t ip = htonl(0x0A000001u + (uint32_t)i);
        uint16_t pt = htons(SR_PORT_TRANSIT);
        memcpy(cfg->segments[i].ip, &ip, 4);
        memcpy(cfg->segments[i].port, &pt, 2);
    }
}

/* ------------------------------------------------------------------ */
/* test_path_table_init：初始化后 count == 0                            */
/* ------------------------------------------------------------------ */
static void test_path_table_init(void)
{
    sr_path_table_t t;
    TEST_ASSERT_EQ(sr_path_table_init(&t), 0, "table_init 应成功");
    TEST_ASSERT_EQ(t.count, 0, "初始化后 count 应为 0");
    TEST_ASSERT_EQ(sr_path_table_init(NULL), -1, "t=NULL 应返回 -1");
}

/* ------------------------------------------------------------------ */
/* test_path_add：添加路径后 lookup 能找到                              */
/* ------------------------------------------------------------------ */
static void test_path_add(void)
{
    sr_path_table_t t;
    sr_path_config_t cfg;
    sr_path_config_t *found;

    TEST_ASSERT_EQ(sr_path_table_init(&t), 0, "table_init 应成功");
    make_path_cfg(&cfg, 0x1001, 3);

    TEST_ASSERT_EQ(sr_path_add(&t, &cfg), 0, "add 应成功");
    TEST_ASSERT_EQ(t.count, 1, "add 后 count 应为 1");

    found = sr_path_lookup(&t, 0x1001);
    TEST_ASSERT_NOT_NULL(found, "lookup 应能找到刚添加的路径");
    TEST_ASSERT_EQ(found->path_id, 0x1001, "找到路径的 path_id 应匹配");
    TEST_ASSERT_EQ(found->seg_count, 3, "找到路径的 seg_count 应为 3");

    /* 未添加的 path_id 应查不到 */
    TEST_ASSERT_NULL(sr_path_lookup(&t, 0x2002), "不存在的 path_id 应返回 NULL");

    /* 非法参数 */
    TEST_ASSERT_EQ(sr_path_add(NULL, &cfg), -1, "t=NULL add 应失败");
    TEST_ASSERT_EQ(sr_path_add(&t, NULL), -1, "cfg=NULL add 应失败");
    /* seg_count == 0 非法 */
    make_path_cfg(&cfg, 0x1002, 1);
    cfg.seg_count = 0;
    TEST_ASSERT_EQ(sr_path_add(&t, &cfg), -1, "seg_count=0 add 应失败");
}

/* ------------------------------------------------------------------ */
/* test_path_add_overwrite：同 path_id 再添加应覆盖                     */
/* ------------------------------------------------------------------ */
static void test_path_add_overwrite(void)
{
    sr_path_table_t t;
    sr_path_config_t cfg;
    sr_path_config_t *found;

    TEST_ASSERT_EQ(sr_path_table_init(&t), 0, "table_init 应成功");

    make_path_cfg(&cfg, 0x1001, 2);
    TEST_ASSERT_EQ(sr_path_add(&t, &cfg), 0, "首次 add 应成功");
    TEST_ASSERT_EQ(t.count, 1, "首次 add 后 count 应为 1");

    /* 同 path_id，改为 4 段再添加 → 应覆盖，count 不增 */
    make_path_cfg(&cfg, 0x1001, 4);
    TEST_ASSERT_EQ(sr_path_add(&t, &cfg), 0, "覆盖 add 应成功");
    TEST_ASSERT_EQ(t.count, 1, "覆盖后 count 仍应为 1");

    found = sr_path_lookup(&t, 0x1001);
    TEST_ASSERT_NOT_NULL(found, "覆盖后 lookup 应找到");
    TEST_ASSERT_EQ(found->seg_count, 4, "覆盖后 seg_count 应为新值 4");
}

/* ------------------------------------------------------------------ */
/* test_path_del：删除后 lookup 返回 NULL                               */
/* ------------------------------------------------------------------ */
static void test_path_del(void)
{
    sr_path_table_t t;
    sr_path_config_t cfg;

    TEST_ASSERT_EQ(sr_path_table_init(&t), 0, "table_init 应成功");

    make_path_cfg(&cfg, 0x1001, 3);
    sr_path_add(&t, &cfg);
    make_path_cfg(&cfg, 0x1002, 3);
    sr_path_add(&t, &cfg);
    TEST_ASSERT_EQ(t.count, 2, "添加两条后 count 应为 2");

    TEST_ASSERT_EQ(sr_path_del(&t, 0x1001), 0, "删除应成功");
    TEST_ASSERT_EQ(t.count, 1, "删除后 count 应为 1");
    TEST_ASSERT_NULL(sr_path_lookup(&t, 0x1001), "删除后 lookup 应返回 NULL");
    /* 另一条应保留（紧凑搬移正确） */
    TEST_ASSERT_NOT_NULL(sr_path_lookup(&t, 0x1002), "未删除的路径应仍存在");

    TEST_ASSERT_EQ(sr_path_del(&t, 0x1002), 0, "删除第二条应成功");
    TEST_ASSERT_EQ(t.count, 0, "全部删除后 count 应为 0");
}

/* ------------------------------------------------------------------ */
/* test_path_del_nonexist：删除不存在的 path_id 应返回错误              */
/* ------------------------------------------------------------------ */
static void test_path_del_nonexist(void)
{
    sr_path_table_t t;
    sr_path_config_t cfg;

    TEST_ASSERT_EQ(sr_path_table_init(&t), 0, "table_init 应成功");
    make_path_cfg(&cfg, 0x1001, 3);
    sr_path_add(&t, &cfg);

    TEST_ASSERT_EQ(sr_path_del(&t, 0x9999), -1, "删除不存在的 path_id 应返回 -1");
    TEST_ASSERT_EQ(t.count, 1, "删除失败不应改变 count");
    TEST_ASSERT_EQ(sr_path_del(NULL, 0x1001), -1, "t=NULL 删除应返回 -1");
}

/* ------------------------------------------------------------------ */
/* test_path_fill_header：生成 SR 头并验证字段与 HMAC 签名               */
/* ------------------------------------------------------------------ */
static void test_path_fill_header(void)
{
    sr_path_config_t cfg;
    sr_header_t h;
    uint32_t ts = 0x61000000u;
    int i;

    make_path_cfg(&cfg, 0x1234, 3);
    cfg.hmac_id = 0x00AA;

    TEST_ASSERT_EQ(sr_path_fill_header(&cfg, &h, ts), 0, "fill_header 应成功");

    /* 固定字段 */
    TEST_ASSERT_EQ(h.magic, SR_MAGIC, "magic 应为 0x53");
    TEST_ASSERT_EQ(h.ver_flags, SR_VER_FLAGS, "ver_flags 应正确");
    TEST_ASSERT_EQ(h.seg_count, 3, "seg_count 应为 3");
    TEST_ASSERT_EQ(h.seg_left, 3, "seg_left 应等于 seg_count");
    TEST_ASSERT_EQ(h.hdr_len, SR_HDR_LEN(3), "hdr_len 应为 42");
    TEST_ASSERT_EQ(h.action, SR_ACTION_FORWARD, "action 应为 FORWARD");

    /* 可扩展字段（网络序读回主机序） */
    TEST_ASSERT_EQ(sr_hdr_get_path_id(&h), 0x1234, "path_id 应正确");
    TEST_ASSERT_EQ(sr_hdr_get_hmac_id(&h), 0x00AA, "hmac_id 应正确");
    TEST_ASSERT_EQ(sr_hdr_get_timestamp(&h), ts, "timestamp 应正确");

    /* 段列表：应与配置一致（网络序字节） */
    for (i = 0; i < 3; i++) {
        TEST_ASSERT_MEM_EQ(h.segments[i].ip, cfg.segments[i].ip, 4,
                           "段 IP 应与配置一致");
        TEST_ASSERT_MEM_EQ(h.segments[i].port, cfg.segments[i].port, 2,
                           "段 Port 应与配置一致");
    }

    /* 基础验证通过 */
    TEST_ASSERT_EQ(sr_hdr_validate(&h, SR_HDR_LEN(3)), 0, "生成的头部应通过验证");

    /* HMAC 已签名：用配置的 P-Key 验证应成功 */
    TEST_ASSERT_EQ(sr_hmac_verify(&h, cfg.pkey), 0, "fill_header 后 HMAC 验证应成功");

    /* 非法参数 */
    TEST_ASSERT_EQ(sr_path_fill_header(NULL, &h, ts), -1, "cfg=NULL 应失败");
    TEST_ASSERT_EQ(sr_path_fill_header(&cfg, NULL, ts), -1, "h=NULL 应失败");
    cfg.seg_count = 0;
    TEST_ASSERT_EQ(sr_path_fill_header(&cfg, &h, ts), -1, "seg_count=0 应失败");
}

/* ------------------------------------------------------------------ */
/* test_path_table_full：添加超过 SR_MAX_PATHS 条路径应失败             */
/* ------------------------------------------------------------------ */
static void test_path_table_full(void)
{
    sr_path_table_t t;
    sr_path_config_t cfg;
    int i;

    TEST_ASSERT_EQ(sr_path_table_init(&t), 0, "table_init 应成功");

    /* 填满 SR_MAX_PATHS 条（path_id 各不相同） */
    for (i = 0; i < SR_MAX_PATHS; i++) {
        make_path_cfg(&cfg, (uint16_t)(0x1000 + i), 1);
        TEST_ASSERT_EQ(sr_path_add(&t, &cfg), 0, "填满过程中 add 应成功");
    }
    TEST_ASSERT_EQ(t.count, SR_MAX_PATHS, "填满后 count 应为 SR_MAX_PATHS");

    /* 第 SR_MAX_PATHS+1 条（新 path_id）应失败 */
    make_path_cfg(&cfg, 0x9999, 1);
    TEST_ASSERT_EQ(sr_path_add(&t, &cfg), -1, "表满后添加新路径应失败");
    TEST_ASSERT_EQ(t.count, SR_MAX_PATHS, "表满后 count 不应增加");

    /* 覆盖已有 path_id 仍应成功（不新增槽位） */
    make_path_cfg(&cfg, 0x1000, 2);
    TEST_ASSERT_EQ(sr_path_add(&t, &cfg), 0, "表满时覆盖已有路径应成功");
    TEST_ASSERT_EQ(t.count, SR_MAX_PATHS, "覆盖后 count 不应变化");
}

/* ------------------------------------------------------------------ */
/* 套件入口                                                             */
/* ------------------------------------------------------------------ */
void test_suite_sr_path(void)
{
    TEST_SUITE_BEGIN("SR Path (sr_path.c)");
    RUN_TEST(test_path_table_init);
    RUN_TEST(test_path_add);
    RUN_TEST(test_path_add_overwrite);
    RUN_TEST(test_path_del);
    RUN_TEST(test_path_del_nonexist);
    RUN_TEST(test_path_fill_header);
    RUN_TEST(test_path_table_full);
    TEST_SUITE_END();
}
