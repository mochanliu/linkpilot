/*
 * test_sr_path.c - 路径表管理（sr_path.c）单元测试
 *
 * 覆盖：初始化、添加、覆盖、删除、删除不存在、fill_header 生成、
 *       表满溢出、hmac_id 引用计数、密钥轮换刷新路径副本、
 *       4096 容量下的定位索引一致性（删除搬移后槽位归属）、
 *       终点段取自报文（A 方案：路径只存中转跳，endpoint 由调用方传入）。
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
 * 构造一条 relay_count 跳的路径配置（只有中转跳，没有终点）。
 * 中转跳 i 的 IP = 10.0.0.(i+1)，端口 = SR_PORT_TRANSIT，均为网络序。
 */
static void make_path_cfg(sr_path_config_t *cfg, uint16_t path_id,
                          uint8_t relay_count)
{
    int i;
    memset(cfg, 0, sizeof(*cfg));
    cfg->path_id      = path_id;
    cfg->hmac_id      = 0x0007;
    cfg->relay_count  = relay_count;
    cfg->active       = 1;
    memcpy(cfg->pkey, g_pkey, SR_PKEY_LEN);
    for (i = 0; i < relay_count; i++) {
        uint32_t ip = htonl(0x0A000001u + (uint32_t)i);
        uint16_t pt = htons(SR_PORT_TRANSIT);
        memcpy(cfg->relays[i].ip, &ip, 4);
        memcpy(cfg->relays[i].port, &pt, 2);
    }
}

/* 终点段：ip/port 传主机序，本函数换成网络序（与 encap 从报文取到的形状一致） */
static void make_endpoint(sr_segment_t *ep, uint32_t ip_host, uint16_t port_host)
{
    uint32_t ip_net = htonl(ip_host);
    uint16_t pt_net = htons(port_host);
    memset(ep, 0, sizeof(*ep));
    memcpy(ep->ip, &ip_net, 4);
    memcpy(ep->port, &pt_net, 2);
}

/* ------------------------------------------------------------------ */
/* test_path_table_init：初始化后 count == 0                            */
/* ------------------------------------------------------------------ */
static void test_path_table_init(void)
{
    static sr_path_table_t t;
    TEST_ASSERT_EQ(sr_path_table_init(&t), 0, "table_init 应成功");
    TEST_ASSERT_EQ(t.count, 0, "初始化后 count 应为 0");
    TEST_ASSERT_EQ(sr_path_table_init(NULL), -1, "t=NULL 应返回 -1");
}

/* ------------------------------------------------------------------ */
/* test_path_add：添加路径后 lookup 能找到                              */
/* ------------------------------------------------------------------ */
static void test_path_add(void)
{
    static sr_path_table_t t;
    sr_path_config_t cfg;
    sr_path_config_t *found;

    TEST_ASSERT_EQ(sr_path_table_init(&t), 0, "table_init 应成功");
    make_path_cfg(&cfg, 0x1001, 3);

    TEST_ASSERT_EQ(sr_path_add(&t, &cfg), 0, "add 应成功");
    TEST_ASSERT_EQ(t.count, 1, "add 后 count 应为 1");

    found = sr_path_lookup(&t, 0x1001);
    TEST_ASSERT_NOT_NULL(found, "lookup 应能找到刚添加的路径");
    TEST_ASSERT_EQ(found->path_id, 0x1001, "找到路径的 path_id 应匹配");
    TEST_ASSERT_EQ(found->relay_count, 3, "找到路径的 relay_count 应为 3");

    /* 未添加的 path_id 应查不到 */
    TEST_ASSERT_NULL(sr_path_lookup(&t, 0x2002), "不存在的 path_id 应返回 NULL");

    /* 非法参数 */
    TEST_ASSERT_EQ(sr_path_add(NULL, &cfg), -1, "t=NULL add 应失败");
    TEST_ASSERT_EQ(sr_path_add(&t, NULL), -1, "cfg=NULL add 应失败");
    /* relay_count = 0 合法：无中转跳的隧道仍要能带 SR 头（verified 观测的凭证） */
    make_path_cfg(&cfg, 0x1002, 0);
    TEST_ASSERT_EQ(sr_path_add(&t, &cfg), 0, "relay_count=0 add 应成功");
    TEST_ASSERT_NOT_NULL(sr_path_lookup(&t, 0x1002), "零中转跳路径应可查到");
    /* 越界：线格式要留一位给终点段，所以上限是 SR_MAX_RELAYS */
    make_path_cfg(&cfg, 0x1003, SR_MAX_RELAYS + 1);
    TEST_ASSERT_EQ(sr_path_add(&t, &cfg), -1, "relay_count 越界 add 应失败");
}

/* ------------------------------------------------------------------ */
/* test_path_add_overwrite：同 path_id 再添加应覆盖                     */
/* ------------------------------------------------------------------ */
static void test_path_add_overwrite(void)
{
    static sr_path_table_t t;
    sr_path_config_t cfg;
    sr_path_config_t *found;

    TEST_ASSERT_EQ(sr_path_table_init(&t), 0, "table_init 应成功");

    make_path_cfg(&cfg, 0x1001, 2);
    TEST_ASSERT_EQ(sr_path_add(&t, &cfg), 0, "首次 add 应成功");
    TEST_ASSERT_EQ(t.count, 1, "首次 add 后 count 应为 1");

    /* 同 path_id，改为 4 跳再添加 → 应覆盖，count 不增 */
    make_path_cfg(&cfg, 0x1001, 4);
    TEST_ASSERT_EQ(sr_path_add(&t, &cfg), 0, "覆盖 add 应成功");
    TEST_ASSERT_EQ(t.count, 1, "覆盖后 count 仍应为 1");

    found = sr_path_lookup(&t, 0x1001);
    TEST_ASSERT_NOT_NULL(found, "覆盖后 lookup 应找到");
    TEST_ASSERT_EQ(found->relay_count, 4, "覆盖后 relay_count 应为新值 4");
}

/* ------------------------------------------------------------------ */
/* test_path_del：删除后 lookup 返回 NULL                               */
/* ------------------------------------------------------------------ */
static void test_path_del(void)
{
    static sr_path_table_t t;
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
    static sr_path_table_t t;
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
    sr_segment_t ep;
    uint32_t ts = 0x61000000u;
    int i;

    make_path_cfg(&cfg, 0x1234, 2);           /* 2 跳中转 */
    cfg.hmac_id = 0x00AA;
    make_endpoint(&ep, 0x0A0A0103u, SR_PORT_VPN);

    TEST_ASSERT_EQ(sr_path_fill_header(&cfg, &h, ts, &ep), 0, "fill_header 应成功");

    /* 固定字段：线格式段数 = 中转跳 + 终点那一位 */
    TEST_ASSERT_EQ(h.magic, SR_MAGIC, "magic 应为 0x53");
    TEST_ASSERT_EQ(h.ver_flags, SR_VER_FLAGS, "ver_flags 应正确");
    TEST_ASSERT_EQ(h.seg_count, 3, "seg_count 应为 relay_count+1 = 3");
    TEST_ASSERT_EQ(h.seg_left, 3, "fill_header 后 seg_left 应等于 seg_count");
    TEST_ASSERT_EQ(h.hdr_len, SR_HDR_LEN(3), "hdr_len 应为 42");
    TEST_ASSERT_EQ(h.action, SR_ACTION_FORWARD, "action 应为 FORWARD");

    /* 可扩展字段（网络序读回主机序） */
    TEST_ASSERT_EQ(sr_hdr_get_path_id(&h), 0x1234, "path_id 应正确");
    TEST_ASSERT_EQ(sr_hdr_get_hmac_id(&h), 0x00AA, "hmac_id 应正确");
    TEST_ASSERT_EQ(sr_hdr_get_timestamp(&h), ts, "timestamp 应正确");

    /* segments[0] 来自报文（endpoint），segments[1+i] 来自路径表的中转跳 */
    TEST_ASSERT_MEM_EQ(h.segments[0].ip, ep.ip, 4, "终点段 IP 应等于传入的 endpoint");
    TEST_ASSERT_MEM_EQ(h.segments[0].port, ep.port, 2, "终点段端口应等于 endpoint");
    for (i = 0; i < cfg.relay_count; i++) {
        TEST_ASSERT_MEM_EQ(h.segments[1 + i].ip, cfg.relays[i].ip, 4,
                           "中转段 IP 应与配置一致");
        TEST_ASSERT_MEM_EQ(h.segments[1 + i].port, cfg.relays[i].port, 2,
                           "中转段端口应与配置一致");
    }

    /* 基础验证通过 */
    TEST_ASSERT_EQ(sr_hdr_validate(&h, SR_HDR_LEN(3)), 0, "生成的头部应通过验证");

    /* HMAC 已签名：用配置的 P-Key 验证应成功 */
    TEST_ASSERT_EQ(sr_hmac_verify(&h, cfg.pkey), 0, "fill_header 后 HMAC 验证应成功");

    /* 非法参数 */
    TEST_ASSERT_EQ(sr_path_fill_header(NULL, &h, ts, &ep), -1, "cfg=NULL 应失败");
    TEST_ASSERT_EQ(sr_path_fill_header(&cfg, NULL, ts, &ep), -1, "h=NULL 应失败");
    TEST_ASSERT_EQ(sr_path_fill_header(&cfg, &h, ts, NULL), -1, "endpoint=NULL 应失败");
    cfg.relay_count = SR_MAX_RELAYS + 1;
    TEST_ASSERT_EQ(sr_path_fill_header(&cfg, &h, ts, &ep), -1,
                   "relay_count 越界应失败");
}

/* ------------------------------------------------------------------ */
/* test_fill_header_relay_less：零中转跳 = 只有终点段的一跳路径          */
/* ------------------------------------------------------------------ */
static void test_fill_header_relay_less(void)
{
    sr_path_config_t cfg;
    sr_header_t h;
    sr_segment_t ep;

    make_path_cfg(&cfg, 0x1300, 0);
    make_endpoint(&ep, 0x0A0A0103u, SR_PORT_VPN);

    TEST_ASSERT_EQ(sr_path_fill_header(&cfg, &h, 0x61000001u, &ep), 0,
                   "零中转跳应能签出头（这是 endpoint 为空时对端地址的唯一来源）");
    TEST_ASSERT_EQ(h.seg_count, 1, "seg_count 应为 1");
    TEST_ASSERT_MEM_EQ(h.segments[0].ip, ep.ip, 4, "唯一一段就是终点");
    TEST_ASSERT_EQ(sr_hmac_verify(&h, cfg.pkey), 0, "零中转跳的头应验签通过");
}

/* ------------------------------------------------------------------ */
/* test_fill_header_endpoint_drift：endpoint 变了不必重建路径，签名照旧   */
/*                                                                   */
/* 这正是 A 方案要买到的性质：路径表里不存终点，所以 NAT 学到新地址、     */
/* 对端换端口都不需要重新下发路径。反证也要拿到 —— endpoint 在 HMAC       */
/* 覆盖范围内，签完再偷偷改它必须验签失败（否则"改写外层"就成了伪造通道）。 */
/* ------------------------------------------------------------------ */
static void test_fill_header_endpoint_drift(void)
{
    sr_path_config_t cfg;
    sr_header_t h1, h2;
    sr_segment_t ep_a, ep_b;

    make_path_cfg(&cfg, 0x1400, 1);
    make_endpoint(&ep_a, 0x0A0A0103u, SR_PORT_VPN);
    make_endpoint(&ep_b, 0xC0000222u, 20001u);   /* 换了地址，也换了端口 */

    TEST_ASSERT_EQ(sr_path_fill_header(&cfg, &h1, 0x61000002u, &ep_a), 0,
                   "endpoint A 签名应成功");
    TEST_ASSERT_EQ(sr_path_fill_header(&cfg, &h2, 0x61000002u, &ep_b), 0,
                   "同一路径、endpoint B 签名应成功（路径不用重建）");
    TEST_ASSERT_EQ(sr_hmac_verify(&h2, cfg.pkey), 0, "endpoint B 的头应验签通过");
    TEST_ASSERT(memcmp(h2.segments[0].ip, h1.segments[0].ip, 4) != 0,
                "终点段应随报文变化，不是路径表里的老地址");

    /* 反证：签名之后再改终点段 = 篡改，必须验签失败 */
    make_endpoint(&ep_b, 0xC0000222u, 20002u);
    memcpy(h1.segments[0].ip, ep_b.ip, 4);
    memcpy(h1.segments[0].port, ep_b.port, 2);
    TEST_ASSERT_NEQ(sr_hmac_verify(&h1, cfg.pkey), 0,
                    "签完后改写终点段应验签失败（终点在 HMAC 覆盖范围内）");
}

/* ------------------------------------------------------------------ */
/* test_path_table_full：添加超过 SR_MAX_PATHS 条路径应失败             */
/* ------------------------------------------------------------------ */
static void test_path_table_full(void)
{
    static sr_path_table_t t;
    sr_path_config_t cfg;
    int i;

    TEST_ASSERT_EQ(sr_path_table_init(&t), 0, "table_init 应成功");

    /* 容量就是"一台盒子的隧道数"，至少要到 4096（运营口径 3000 条/盒） */
    TEST_ASSERT(SR_MAX_PATHS >= 4096, "SR_MAX_PATHS 不得低于 4096");

    /* 填满 SR_MAX_PATHS 条（path_id 各不相同） */
    for (i = 0; i < SR_MAX_PATHS; i++) {
        make_path_cfg(&cfg, (uint16_t)(0x1000 + i), 1);
        TEST_ASSERT_EQ(sr_path_add(&t, &cfg), 0, "填满过程中 add 应成功");
    }
    TEST_ASSERT_EQ(t.count, SR_MAX_PATHS, "填满后 count 应为 SR_MAX_PATHS");
    /* 每一条都要能查到 —— 容量不等于可达，索引表错了就是"配了但选不到路" */
    for (i = 0; i < SR_MAX_PATHS; i += 511) {
        make_path_cfg(&cfg, (uint16_t)(0x1000 + i), 1);
        TEST_ASSERT_NOT_NULL(sr_path_lookup(&t, cfg.path_id), "满表应逐条可查");
    }

    /* 第 SR_MAX_PATHS+1 条（新 path_id）应失败，且必须是"表已满"这个码 */
    make_path_cfg(&cfg, 0x9999, 1);
    TEST_ASSERT_EQ(sr_path_add(&t, &cfg), SR_PATH_ERR_FULL,
                   "表满后添加新路径应返回 SR_PATH_ERR_FULL");
    TEST_ASSERT_EQ(t.count, SR_MAX_PATHS, "表满后 count 不应增加");

    /* 覆盖已有 path_id 仍应成功（不新增槽位） */
    make_path_cfg(&cfg, 0x1000, 2);
    TEST_ASSERT_EQ(sr_path_add(&t, &cfg), 0, "表满时覆盖已有路径应成功");
    TEST_ASSERT_EQ(t.count, SR_MAX_PATHS, "覆盖后 count 不应变化");

    /* 腾出槽位后必须能再加 —— 删一条就够一条，说明索引注销了 */
    TEST_ASSERT_EQ(sr_path_del(&t, 0x1000), 0, "删除应成功");
    make_path_cfg(&cfg, 0x9999, 1);
    TEST_ASSERT_EQ(sr_path_add(&t, &cfg), 0, "腾槽后添加新路径应成功");
    TEST_ASSERT_EQ(t.count, SR_MAX_PATHS, "腾槽后 count 应回到满值");
    TEST_ASSERT_NOT_NULL(sr_path_lookup(&t, 0x9999), "新路径应可查");
}

/* ------------------------------------------------------------------ */
/* test_path_index_compact：删除的末位搬移必须同步改搬家的条目索引        */
/* ------------------------------------------------------------------ */
static void test_path_index_compact(void)
{
    static sr_path_table_t t;
    sr_path_config_t cfg;
    sr_path_config_t *f;

    TEST_ASSERT_EQ(sr_path_table_init(&t), 0, "table_init 应成功");

    make_path_cfg(&cfg, 0x00A1, 1);   /* slot 0 */
    TEST_ASSERT_EQ(sr_path_add(&t, &cfg), 0, "add A1 应成功");
    make_path_cfg(&cfg, 0x00A2, 2);   /* slot 1 */
    TEST_ASSERT_EQ(sr_path_add(&t, &cfg), 0, "add A2 应成功");
    make_path_cfg(&cfg, 0x00A3, 3);   /* slot 2 */
    TEST_ASSERT_EQ(sr_path_add(&t, &cfg), 0, "add A3 应成功");

    /* 删中间/头部条目，末尾条目会搬到被删槽位；索引必须跟着改，
     * 否则 A3 搬到 slot0 后按 0x00A3 查会落到别处或查不到。 */
    TEST_ASSERT_EQ(sr_path_del(&t, 0x00A1), 0, "删除 A1 应成功");
    TEST_ASSERT_NULL(sr_path_lookup(&t, 0x00A1), "A1 删除后应查不到");
    f = sr_path_lookup(&t, 0x00A3);
    TEST_ASSERT_NOT_NULL(f, "A3 搬家后仍应查到");
    TEST_ASSERT_EQ(f, &t.paths[0], "A3 应落在 slot 0");
    TEST_ASSERT_EQ(f->relay_count, 3, "A3 搬家后内容不应错");
    TEST_ASSERT_EQ(t.paths[2].path_id, 0, "腾空槽位应清零");

    /* 覆盖式 add 不能改变槽位（索引仍要成立） */
    make_path_cfg(&cfg, 0x00A3, 5);
    TEST_ASSERT_EQ(sr_path_add(&t, &cfg), 0, "覆盖 A3 应成功");
    f = sr_path_lookup(&t, 0x00A3);
    TEST_ASSERT_EQ(f, &t.paths[0], "覆盖不应搬动槽位");
    TEST_ASSERT_EQ(f->relay_count, 5, "覆盖后 relay_count 应为新值");

    /* 删末尾槽位上的条目：前面的条目不该被搬动 */
    TEST_ASSERT_EQ(sr_path_del(&t, 0x00A2), 0, "删除 A2（末尾槽位）应成功");
    f = sr_path_lookup(&t, 0x00A3);
    TEST_ASSERT_NOT_NULL(f, "A3 应仍在原位");
    TEST_ASSERT_EQ(f, &t.paths[0], "A3 槽位不应被改动");
    TEST_ASSERT_EQ(t.count, 1, "count 应为 1");
    TEST_ASSERT_EQ(sr_path_del(&t, 0x00A3), 0, "删除 A3 应成功");
    TEST_ASSERT_EQ(t.count, 0, "全删后 count 应为 0");
    TEST_ASSERT_NULL(sr_path_lookup(&t, 0x00A2), "空表应查不到");
    TEST_ASSERT_NULL(sr_path_lookup(&t, 0x00A3), "空表应查不到");

    /* 反复增删：索引不得残留，同 path_id 不得占两个槽 */
    for (int k = 0; k < 200; k++) {
        make_path_cfg(&cfg, (uint16_t)(0x2000 + k), 1);
        TEST_ASSERT_EQ(sr_path_add(&t, &cfg), 0, "循环 add 应成功");
        TEST_ASSERT_EQ(sr_path_del(&t, (uint16_t)(0x2000 + k)), 0,
                       "循环 del 应成功");
    }
    TEST_ASSERT_EQ(t.count, 0, "循环增删后 count 应回到 0");
    make_path_cfg(&cfg, 0x2000, 1);
    TEST_ASSERT_EQ(sr_path_add(&t, &cfg), 0, "循环后仍可添加");
    TEST_ASSERT_EQ(sr_path_del(&t, 0x2000), 0, "循环后仍可删除");
}

/* ------------------------------------------------------------------ */
/* test_path_id_bounds：path_id 的两端都可用                             */
/* ------------------------------------------------------------------ */
static void test_path_id_bounds(void)
{
    static sr_path_table_t t;
    sr_path_config_t cfg;

    TEST_ASSERT_EQ(sr_path_table_init(&t), 0, "table_init 应成功");

    /* 0 是合法 path_id，不能当"空"用（API 的"全量"哨兵是 0xffff） */
    make_path_cfg(&cfg, 0x0000, 1);
    TEST_ASSERT_EQ(sr_path_add(&t, &cfg), 0, "path_id=0 应可添加");
    TEST_ASSERT_NOT_NULL(sr_path_lookup(&t, 0x0000), "path_id=0 应可查到");

    /* 0xffff 是 dump 的"全量"哨兵，控制面不会下发它；数据面仍要能存 */
    make_path_cfg(&cfg, 0xffff, 1);
    TEST_ASSERT_EQ(sr_path_add(&t, &cfg), 0, "path_id=0xffff 应可添加");
    TEST_ASSERT_NOT_NULL(sr_path_lookup(&t, 0xffff), "path_id=0xffff 应可查到");

    /* 索引表按 path_id 定长，越界不存在 —— 这里只保证两端都落位 */
    TEST_ASSERT_EQ(t.count, 2, "两条都应占槽");
}

/* ------------------------------------------------------------------ */
/* test_path_count_using_hmac_id：删密钥前必须先确认没有路径在引用        */
/* ------------------------------------------------------------------ */
static void test_path_count_using_hmac_id(void)
{
    static sr_path_table_t t;
    sr_path_config_t cfg;

    TEST_ASSERT_EQ(sr_path_table_init(&t), 0, "table_init 应成功");
    TEST_ASSERT_EQ(sr_path_count_using_hmac_id(&t, 0x0007), 0, "空表应为 0");
    TEST_ASSERT_EQ(sr_path_count_using_hmac_id(NULL, 0x0007), 0, "t=NULL 应返回 0");

    make_path_cfg(&cfg, 0x1001, 1);          /* hmac_id 0x0007 */
    sr_path_add(&t, &cfg);
    make_path_cfg(&cfg, 0x1002, 1);
    cfg.hmac_id = 0x0008;
    sr_path_add(&t, &cfg);
    make_path_cfg(&cfg, 0x1003, 1);
    cfg.hmac_id = 0x0007;
    cfg.active = 0;                          /* inactive 仍占着密钥副本 */
    sr_path_add(&t, &cfg);

    TEST_ASSERT_EQ(sr_path_count_using_hmac_id(&t, 0x0007), 2,
                   "应统计到 2 条（含 inactive）");
    TEST_ASSERT_EQ(sr_path_count_using_hmac_id(&t, 0x0008), 1, "应为 1 条");
    TEST_ASSERT_EQ(sr_path_count_using_hmac_id(&t, 0x9999), 0, "未被引用应为 0");

    /* 删掉一条后计数随之下降 —— 这是 key_del 放行的依据 */
    TEST_ASSERT_EQ(sr_path_del(&t, 0x1003), 0, "删除应成功");
    TEST_ASSERT_EQ(sr_path_count_using_hmac_id(&t, 0x0007), 1, "删除后应为 1 条");
}

/*
 * test_key_rotation：密钥轮换后，路径里的 P-Key 副本必须被刷新，
 * 且签出的头部只能用新密钥验通过（旧密钥应失败）。
 * 这正是 sr_wg_key_add 调 sr_path_replace_pkey 要解决的问题。
 */
static void test_key_rotation(void)
{
    static sr_path_table_t t;
    sr_path_config_t cfg;
    sr_path_config_t *p1, *p7;
    sr_header_t h;
    sr_segment_t ep;
    uint8_t newkey[SR_PKEY_LEN];
    int i;

    for (i = 0; i < SR_PKEY_LEN; i++)
        newkey[i] = (uint8_t)(0x5cu + i);
    make_endpoint(&ep, 0x0A0A0103u, SR_PORT_VPN);

    TEST_ASSERT_EQ(sr_path_table_init(&t), 0, "table_init 应成功");

    make_path_cfg(&cfg, 0x1001, 2);          /* hmac_id = 0x0007 */
    sr_path_add(&t, &cfg);
    make_path_cfg(&cfg, 0x1002, 2);
    cfg.hmac_id = 0x0008;                    /* 另一把密钥，不应被动 */
    sr_path_add(&t, &cfg);

    TEST_ASSERT_EQ(sr_path_replace_pkey(&t, 0x0007, newkey), 1,
                   "只应刷新 1 条引用 hmac_id=7 的路径");

    p1 = sr_path_lookup(&t, 0x1001);
    p7 = sr_path_lookup(&t, 0x1002);
    TEST_ASSERT_NOT_NULL(p1, "0x1001 应存在");
    TEST_ASSERT_NOT_NULL(p7, "0x1002 应存在");
    TEST_ASSERT_MEM_EQ(p1->pkey, newkey, SR_PKEY_LEN, "轮换后 pkey 副本应为新值");
    TEST_ASSERT_MEM_EQ(p7->pkey, g_pkey, SR_PKEY_LEN,
                       "其他 hmac_id 的路径副本不应被改");

    /* 新密钥签出的头：新密钥验通过，旧密钥验失败 */
    TEST_ASSERT_EQ(sr_path_fill_header(p1, &h, 0x62000001u, &ep), 0,
                   "fill_header 应成功");
    TEST_ASSERT_EQ(sr_hmac_verify(&h, newkey), 0, "新密钥应验证通过");
    TEST_ASSERT_NEQ(sr_hmac_verify(&h, g_pkey), 0, "旧密钥不应验证通过");

    /* 旧路径（未刷新）用旧密钥仍有效 → 说明「不刷新」确实会导致断链 */
    TEST_ASSERT_EQ(sr_path_fill_header(p7, &h, 0x62000002u, &ep), 0,
                   "fill_header 应成功");
    TEST_ASSERT_EQ(sr_hmac_verify(&h, g_pkey), 0, "未轮换的路径仍用旧密钥签名");

    /* 参数非法 */
    TEST_ASSERT_EQ(sr_path_replace_pkey(NULL, 0x0007, newkey), 0, "t=NULL 应返回 0");
    TEST_ASSERT_EQ(sr_path_replace_pkey(&t, 0x0007, NULL), 0, "pkey=NULL 应返回 0");
    TEST_ASSERT_EQ(sr_path_replace_pkey(&t, 0x9999, newkey), 0, "无引用应返回 0");
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
    RUN_TEST(test_fill_header_relay_less);
    RUN_TEST(test_fill_header_endpoint_drift);
    RUN_TEST(test_path_table_full);
    RUN_TEST(test_path_index_compact);
    RUN_TEST(test_path_id_bounds);
    RUN_TEST(test_path_count_using_hmac_id);
    RUN_TEST(test_key_rotation);
    TEST_SUITE_END();
}
