/*
 * test_sr_obs.c - 外层源观测表（sr_obs.c）单元测试
 *
 * 覆盖：槽位选择（确定性 / 值域 / 相邻 peer 不集体撞槽）、插入、命中更新、
 *       verified 只升不降、槽冲突淘汰、清空、遍历游标。
 */
#include "test_framework.h"
#include "sr_obs.h"

void test_suite_sr_obs(void);

/* 观测一个元组：ip 用主机序 u32（表只按调用方给的比特存，不解释字节序） */
static void obs1(sr_obs_table_t *t, uint32_t ip, uint16_t sport,
                 uint16_t dport, uint16_t rx_if, uint64_t now, uint8_t v)
{
    sr_obs_record(t, ip, sport, dport, rx_if, now, v);
}

/* 取元组应落在的槽；表内部结构，测试直接读 slots[] 便于断言 */
static sr_obs_entry_t *slot_of(sr_obs_table_t *t, uint32_t ip, uint16_t sport,
                              uint16_t dport)
{
    return &t->slots[sr_obs_slot(ip, sport, dport)];
}

/* ------------------------------------------------------------------ */
/* test_slot_is_deterministic：同一三元组恒定映射到同一槽，且在值域内      */
/* ------------------------------------------------------------------ */
static void test_slot_is_deterministic(void)
{
    uint32_t s1 = sr_obs_slot(0x0A000001u, 50000, 51820);
    uint32_t s2 = sr_obs_slot(0x0A000001u, 50000, 51820);

    TEST_ASSERT_EQ(s1, s2, "同一三元组必须映射到同一槽");
    TEST_ASSERT(s1 < SR_OBS_SLOTS, "槽号必须小于 SR_OBS_SLOTS");
}

/* ------------------------------------------------------------------ */
/* test_slot_spreads_adjacent_peers：连续 IP + 同目的端口不得集体撞槽      */
/*                                                                     */
/* 这是选哈希式子的理由：vpn_port 是常量，若只用 ip ^ port，最后一字节     */
/* 相邻的 peer 会大面积撞同一槽，几十台就把表冲垮。                        */
/* ------------------------------------------------------------------ */
static void test_slot_spreads_adjacent_peers(void)
{
    uint8_t used[SR_OBS_SLOTS];
    int i, n_used = 0, collisions = 0;

    memset(used, 0, sizeof(used));

    for (i = 0; i < 32; i++) {
        uint32_t ip = 0x0A000001u + (uint32_t)i;   /* 只差最后一字节的 peer */
        uint32_t s = sr_obs_slot(ip, (uint16_t)(40000 + i), 51820);
        TEST_ASSERT(s < SR_OBS_SLOTS, "槽号越界");
        if (used[s])
            collisions++;
        else
            n_used++;
        used[s] = 1;
    }

    /* 32 个不同 peer 落进 64 槽：允许生日碰撞，但不允许大面积同槽 */
    TEST_ASSERT(n_used >= 20, "相邻 IP 的 peer 应散落到至少 20 个不同槽");
    TEST_ASSERT(collisions <= 12, "相邻 IP 不得集体撞槽");
}

/* ------------------------------------------------------------------ */
/* test_record_insert：首次观测建条目                                    */
/* ------------------------------------------------------------------ */
static void test_record_insert(void)
{
    sr_obs_table_t t;
    sr_obs_entry_t *e;

    memset(&t, 0, sizeof(t));
    obs1(&t, 0x0A000064u, 50000, 51820, 3, 100, 0);

    e = slot_of(&t, 0x0A000064u, 50000, 51820);
    TEST_ASSERT_EQ(t.count, 1u, "首次观测 count 应为 1");
    TEST_ASSERT_EQ(t.records, 1u, "records 应为 1");
    TEST_ASSERT_EQ(t.evictions, 0u, "无冲突时 evictions 应为 0");
    TEST_ASSERT_EQ(e->valid, 1, "条目应有效");
    TEST_ASSERT_EQ(e->src_ip, 0x0A000064u, "src_ip 原样保存");
    TEST_ASSERT_EQ(e->src_port, 50000, "src_port 原样保存");
    TEST_ASSERT_EQ(e->dst_port, 51820, "dst_port 原样保存");
    TEST_ASSERT_EQ(e->rx_if, 3, "rx_if 原样保存");
    TEST_ASSERT_EQ(e->hits, 1u, "首次观测 hits 应为 1");
    TEST_ASSERT_EQ(e->first_seen, 100u, "first_seen 应为首次时刻");
    TEST_ASSERT_EQ(e->last_seen, 100u, "last_seen 应等于 first_seen");
    TEST_ASSERT_EQ(e->verified, 0, "verified 应如实取入参");

    /* NULL 表必须安全返回（数据面不会传 NULL，但纯函数要有边界） */
    sr_obs_record(NULL, 1, 2, 3, 4, 5, 1);
    sr_obs_clear(NULL);
    TEST_ASSERT_EQ(sr_obs_next(NULL, NULL, NULL), 0, "空表遍历应返回 0");
}

/* ------------------------------------------------------------------ */
/* test_record_hit：重复观测只推进 last_seen/hits                        */
/* ------------------------------------------------------------------ */
static void test_record_hit(void)
{
    sr_obs_table_t t;
    sr_obs_entry_t *e;

    memset(&t, 0, sizeof(t));
    obs1(&t, 0x0A000064u, 50000, 51820, 3, 100, 1);
    obs1(&t, 0x0A000064u, 50000, 51820, 7, 137, 1);

    e = slot_of(&t, 0x0A000064u, 50000, 51820);
    TEST_ASSERT_EQ(t.count, 1u, "命中不应新增条目");
    TEST_ASSERT_EQ(t.records, 2u, "records 应累加每一次观测");
    TEST_ASSERT_EQ(t.evictions, 0u, "命中不算淘汰");
    TEST_ASSERT_EQ(e->hits, 2u, "hits 应累加");
    TEST_ASSERT_EQ(e->first_seen, 100u, "first_seen 不应被后续观测改写");
    TEST_ASSERT_EQ(e->last_seen, 137u, "last_seen 应更新");
    /* 换收包接口不改写 rx_if：那是要控制器重做发现的事件，不是本地能解释的 */
    TEST_ASSERT_EQ(e->rx_if, 3, "rx_if 保留首见接口");
}

/* ------------------------------------------------------------------ */
/* test_verified_only_upgrades：认证事实不可降级                          */
/* ------------------------------------------------------------------ */
static void test_verified_only_upgrades(void)
{
    sr_obs_table_t t;
    sr_obs_entry_t *e;

    /* 先旁路包(0) 后 SR 验签包(1) => 升为 1 */
    memset(&t, 0, sizeof(t));
    obs1(&t, 0xC0A80101u, 51000, 51820, 2, 10, 0);
    obs1(&t, 0xC0A80101u, 51000, 51820, 2, 20, 1);
    e = slot_of(&t, 0xC0A80101u, 51000, 51820);
    TEST_ASSERT_EQ(e->verified, 1, "验签过的观测必须把条目升为 verified");

    /* 先 1 后 0 不得降回 0：认证过这件事不因后来的旁路包而失效 */
    obs1(&t, 0xC0A80101u, 51000, 51820, 2, 30, 0);
    TEST_ASSERT_EQ(e->verified, 1, "verified 不可降级");
    TEST_ASSERT_EQ(e->hits, 3u, "三次观测应累计 hits");
}

/* ------------------------------------------------------------------ */
/* test_slot_collision_evicts：同槽新条目覆盖旧条目并计 evictions         */
/* ------------------------------------------------------------------ */
static void test_slot_collision_evicts(void)
{
    sr_obs_table_t t;
    uint32_t ip_a = 0x0A000001u, ip_b = 0;
    uint16_t sp_a = 40000, sp_b = 0;
    uint32_t target;
    int i, found = 0;

    /*
     * 不硬编码撞槽常量：自己找一对确实同槽的三元组。
     * 64 槽下穷举几千个候选必然命中，测试因此对哈希式子的改动是稳健的。
     */
    target = sr_obs_slot(ip_a, sp_a, 51820);
    for (i = 1; i < 20000; i++) {
        uint32_t ip = 0x0A000002u + (uint32_t)i;
        uint16_t sp = (uint16_t)(41000 + i);
        if (sr_obs_slot(ip, sp, 51820) == target) {
            ip_b = ip;
            sp_b = sp;
            found = 1;
            break;
        }
    }
    TEST_ASSERT(found, "应能找到与 ip_a 撞同一槽的三元组");
    TEST_ASSERT(ip_b != ip_a || sp_b != sp_a, "撞槽的另一组必须不同元组");

    memset(&t, 0, sizeof(t));
    obs1(&t, ip_a, sp_a, 51820, 1, 100, 1);
    obs1(&t, ip_b, sp_b, 51820, 2, 200, 0);

    TEST_ASSERT_EQ(t.count, 1u, "覆盖同槽条目不应增加 count");
    TEST_ASSERT_EQ(t.evictions, 1u, "挤掉有效条目应计一次 eviction");
    TEST_ASSERT_EQ(t.records, 2u, "records 统计的是观测次数");
    TEST_ASSERT_EQ(t.slots[target].src_ip, ip_b, "冲突时新条目赢");
    TEST_ASSERT_EQ(t.slots[target].hits, 1u, "新条目从 hits=1 重新开始");
    TEST_ASSERT_EQ(t.slots[target].verified, 0, "新条目的 verified 不被旧值污染");

    /* 再观测被淘汰的 ip_a：它挤掉 ip_b，evictions 变 2（keepalive 会自愈） */
    obs1(&t, ip_a, sp_a, 51820, 1, 300, 1);
    TEST_ASSERT_EQ(t.evictions, 2u, "反向覆盖同样计淘汰");
    TEST_ASSERT_EQ(t.slots[target].src_ip, ip_a, "旧条目下一轮会自己回来");
}

/* ------------------------------------------------------------------ */
/* test_clear：清空把条目与计数一起归零                                  */
/* ------------------------------------------------------------------ */
static void test_clear(void)
{
    sr_obs_table_t t;
    int i;

    memset(&t, 0, sizeof(t));
    for (i = 0; i < 10; i++)
        obs1(&t, 0x0A000001u + (uint32_t)i, (uint16_t)(40000 + i), 51820, 1,
             (uint64_t)i, 1);
    TEST_ASSERT(t.count > 0, "前置条件：表里应有条目");

    sr_obs_clear(&t);
    TEST_ASSERT_EQ(t.count, 0u, "clear 后 count 归零");
    TEST_ASSERT_EQ(t.records, 0u, "clear 后 records 归零");
    TEST_ASSERT_EQ(t.evictions, 0u, "clear 后 evictions 归零");
    for (i = 0; i < SR_OBS_SLOTS; i++)
        TEST_ASSERT_EQ(t.slots[i].valid, 0, "clear 后不得残留有效条目");
}

/* ------------------------------------------------------------------ */
/* test_next_traversal：遍历取尽所有有效条目，游标可续、可探               */
/* ------------------------------------------------------------------ */
static void test_next_traversal(void)
{
    sr_obs_table_t t;
    sr_obs_entry_t e, e2, probe;
    uint32_t cursor = 0, seen = 0;
    uint32_t c_part, c_rest, c1, c2;
    int i, n_part = 0, n_rest = 0, half_seen, p, r;

    memset(&t, 0, sizeof(t));
    for (i = 0; i < 20; i++)
        obs1(&t, 0x0A000001u + (uint32_t)i * 7u, (uint16_t)(40000 + i), 51820,
             1, (uint64_t)i, 1);

    while (sr_obs_next(&t, &cursor, &e))
        seen++;

    TEST_ASSERT_EQ(seen, t.count, "遍历条数必须等于 count");
    TEST_ASSERT_EQ(sr_obs_next(&t, &cursor, &e), 0, "取尽后应返回 0");
    TEST_ASSERT_EQ(cursor, (uint32_t)SR_OBS_SLOTS, "取尽后游标停在表尾");

    /*
     * 游标可续：取到一半记下游标，从该游标继续取完，两段之和必须正好是全量。
     * 这里不能用"槽号过半"来切 —— 游标语义是"从 cursor 起向后扫"，
     * 停在第 N 条之后的游标可能仍小于表长的一半。
     */
    half_seen = (int)seen / 2;
    c_part = 0;
    while (n_part < half_seen && sr_obs_next(&t, &c_part, &e))
        n_part++;
    c_rest = c_part;
    while (sr_obs_next(&t, &c_rest, &e))
        n_rest++;

    TEST_ASSERT_EQ(n_part + n_rest, (int)seen, "分段遍历之和必须等于全量");
    TEST_ASSERT_EQ(c_rest, (uint32_t)SR_OBS_SLOTS, "续遍历取尽后游标也在表尾");

    /* out==NULL 只是"还有没有下一条"的探测：前进的游标与真实读取完全一致 */
    c1 = 0;
    c2 = 0;
    p = sr_obs_next(&t, &c1, NULL);
    r = sr_obs_next(&t, &c2, &probe);
    TEST_ASSERT_EQ(p, r, "探测与读取应给出同样的'有没有'");
    TEST_ASSERT_EQ(c1, c2, "探测与读取推进同一个游标");
    TEST_ASSERT_EQ(sr_obs_next(&t, &c1, &e), 1, "探测之后仍可读取");
    sr_obs_next(&t, &c2, &e2);
    TEST_ASSERT_EQ(e.src_ip, e2.src_ip, "探测不得改变后续读到的条目");
}

void test_suite_sr_obs(void)
{
    TEST_SUITE_BEGIN("sr_obs 外层源观测表");
    RUN_TEST(test_slot_is_deterministic);
    RUN_TEST(test_slot_spreads_adjacent_peers);
    RUN_TEST(test_record_insert);
    RUN_TEST(test_record_hit);
    RUN_TEST(test_verified_only_upgrades);
    RUN_TEST(test_slot_collision_evicts);
    RUN_TEST(test_clear);
    RUN_TEST(test_next_traversal);
    TEST_SUITE_END();
}
