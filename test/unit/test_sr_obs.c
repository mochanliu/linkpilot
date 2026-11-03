/*
 * test_sr_obs.c - 外层源观测表（sr_obs.c）单元测试
 *
 * 覆盖：home 槽选择（确定性 / 值域 / 相邻 peer 不集体撞槽）、插入、命中更新、
 *       verified 只升不降、有界探测（窗口内命中 + 窗口满才淘汰）、清空、
 *       遍历游标、**4096 个不同源能否全部共存**（容量口径的验收判据）。
 */
#include "test_framework.h"
#include "sr_obs.h"

void test_suite_sr_obs(void);

/* 表放大到 16k 槽后单帧 786KB，全部放静态区：栈上摆 8 份会直接爆栈 */
static sr_obs_table_t g_t;

/* 观测一个元组：ip 用主机序 u32（表只按调用方给的比特存，不解释字节序） */
static void obs1(sr_obs_table_t *t, uint32_t ip, uint16_t sport,
                 uint16_t dport, uint16_t rx_if, uint64_t now, uint8_t v)
{
    sr_obs_record(t, ip, sport, dport, rx_if, now, v);
}

/*
 * 按元组取回条目：整表扫，不看哈希也不看探测窗口。
 *
 * 故意不用 sr_obs_slot() 反推位置 —— 那是被测实现的一部分，测试若照抄它的
 * 式子，实现把探测窗口写错时测试会一起错。这里只认"表里有没有这个元组"。
 */
static int find_entry(const sr_obs_table_t *t, uint32_t ip, uint16_t sport,
                      uint16_t dport, sr_obs_entry_t *out)
{
    uint32_t i;

    for (i = 0; i < SR_OBS_SLOTS; i++) {
        const sr_obs_entry_t *e = &t->slots[i];
        if (e->valid && e->src_ip == ip && e->src_port == sport &&
            e->dst_port == dport) {
            *out = *e;
            return 1;
        }
    }
    return 0;
}

/* 有效条目总数（走对外遍历口，不用内部数组） */
static uint32_t live_count(sr_obs_table_t *t)
{
    sr_obs_entry_t e;
    uint32_t cursor = 0, n = 0;

    while (sr_obs_next(t, &cursor, &e))
        n++;
    return n;
}

/* 元组是否互不相同（撞槽测试要用它确认"找到的两组确实是两件事"） */
static int tuple_eq(const sr_obs_entry_t *a, uint32_t ip, uint16_t sport,
                    uint16_t dport)
{
    return a->src_ip == ip && a->src_port == sport && a->dst_port == dport;
}

/* ------------------------------------------------------------------ */
/* test_slot_is_deterministic：同一三元组恒定映射到同一 home 槽，且在值域内 */
/* ------------------------------------------------------------------ */
static void test_slot_is_deterministic(void)
{
    uint32_t s1 = sr_obs_slot(0x0A000001u, 50000, 51820);
    uint32_t s2 = sr_obs_slot(0x0A000001u, 50000, 51820);

    TEST_ASSERT_EQ(s1, s2, "同一三元组必须映射到同一 home 槽");
    TEST_ASSERT(s1 < SR_OBS_SLOTS, "home 槽号必须小于 SR_OBS_SLOTS");
}

/* ------------------------------------------------------------------ */
/* test_slot_spreads_adjacent_peers：连续 IP + 同目的端口不得集体撞槽      */
/*                                                                     */
/* 这是选哈希式子的理由：vpn_port 是常量，若只用 ip ^ port，最后一字节     */
/* 相邻的 peer 会大面积撞同一槽，几十台就把表冲垮。                        */
/* ------------------------------------------------------------------ */
static void test_slot_spreads_adjacent_peers(void)
{
    static uint8_t used[SR_OBS_SLOTS];
    int i, n_used = 0, collisions = 0;

    memset(used, 0, sizeof(used));

    for (i = 0; i < 32; i++) {
        uint32_t ip = 0x0A000001u + (uint32_t)i;   /* 只差最后一字节的 peer */
        uint32_t s = sr_obs_slot(ip, (uint16_t)(40000 + i), 51820);
        TEST_ASSERT(s < SR_OBS_SLOTS, "home 槽号越界");
        if (used[s])
            collisions++;
        else
            n_used++;
        used[s] = 1;
    }

    /*
     * 32 个 home 槽撒在 16k 格里，生日碰撞的期望值 0.03 次，所以阈值必须比
     * 64 槽时代收紧一个量级：卡在 20 的话，这个测试扩容后就再也不检查任何东西。
     */
    TEST_ASSERT(n_used >= 28, "相邻 IP 的 peer 应散落到至少 28 个不同 home 槽");
    TEST_ASSERT(collisions <= 4, "相邻 IP 不得集体撞槽");
}

/* ------------------------------------------------------------------ */
/* test_capacity_4096_sources：4096 个不同源必须全部共存                  */
/*                                                                     */
/* 这是本表扩容的验收判据。直接映射时代（64 槽）同样的输入只留下 64 条、      */
/* evictions 4032 —— 表还没到标称容量就在静默丢观测，所以这一条不是"顺便     */
/* 测一下"，是"没有它就不许说容量是 4096"。                                */
/* ------------------------------------------------------------------ */
static void test_capacity_4096_sources(void)
{
    sr_obs_entry_t e;
    int i;
    uint32_t n_check = 0;

    memset(&g_t, 0, sizeof(g_t));

    for (i = 0; i < 4096; i++)
        /* 三元组互不相同：ip 步进 7，源端口在 2 万个里循环，dst_port 恒定 */
        obs1(&g_t, 0x0A000001u + 7u * (uint32_t)i,
             (uint16_t)(40000 + (i % 20000)), 51820, 1, (uint64_t)i, 1);

    TEST_ASSERT_EQ(g_t.count, 4096u, "4096 个不同源应全部登记");
    TEST_ASSERT_EQ(g_t.evictions, 0u, "负载因子 0.25 下不该发生淘汰");
    TEST_ASSERT_EQ(live_count(&g_t), 4096u, "遍历必须能取回全部 4096 条");
    TEST_ASSERT_EQ(g_t.records, 4096u, "records 等于观测次数");

    /* 抽查 64 条：头、尾、以及均匀分布的中间项 */
    for (i = 0; i < 4096; i += 64) {
        uint32_t ip = 0x0A000001u + 7u * (uint32_t)i;
        uint16_t sp = (uint16_t)(40000 + (i % 20000));
        TEST_ASSERT(find_entry(&g_t, ip, sp, 51820, &e),
                    "抽查的元组必须还在表里");
        TEST_ASSERT(tuple_eq(&e, ip, sp, 51820), "抽查读到的必须是同一个元组");
        TEST_ASSERT_EQ(e.hits, 1u, "每条只观测过一次");
        n_check++;
    }
    TEST_ASSERT_EQ(n_check, 64u, "抽查数量本身要成立（别写出空循环）");

    /* 满载后再观测同一个元组：必须是命中，不是又挤掉别人 */
    g_t.evictions = 0;
    obs1(&g_t, 0x0A000001u, 40000, 51820, 1, 9999, 1);
    TEST_ASSERT(find_entry(&g_t, 0x0A000001u, 40000, 51820, &e),
                "满载时重复观测不能把自己弄丢");
    TEST_ASSERT_EQ(g_t.evictions, 0u, "命中不该计淘汰");
}

/* ------------------------------------------------------------------ */
/* test_record_insert：首次观测建条目                                    */
/* ------------------------------------------------------------------ */
static void test_record_insert(void)
{
    sr_obs_entry_t e;

    memset(&g_t, 0, sizeof(g_t));
    obs1(&g_t, 0x0A000064u, 50000, 51820, 3, 100, 0);

    TEST_ASSERT(find_entry(&g_t, 0x0A000064u, 50000, 51820, &e), "条目应存在");
    TEST_ASSERT_EQ(g_t.count, 1u, "首次观测 count 应为 1");
    TEST_ASSERT_EQ(g_t.records, 1u, "records 应为 1");
    TEST_ASSERT_EQ(g_t.evictions, 0u, "无冲突时 evictions 应为 0");
    TEST_ASSERT_EQ(e.src_ip, 0x0A000064u, "src_ip 原样保存");
    TEST_ASSERT_EQ(e.src_port, 50000, "src_port 原样保存");
    TEST_ASSERT_EQ(e.dst_port, 51820, "dst_port 原样保存");
    TEST_ASSERT_EQ(e.rx_if, 3, "rx_if 原样保存");
    TEST_ASSERT_EQ(e.hits, 1u, "首次观测 hits 应为 1");
    TEST_ASSERT_EQ(e.first_seen, 100u, "first_seen 应为首次时刻");
    TEST_ASSERT_EQ(e.last_seen, 100u, "last_seen 应等于 first_seen");
    TEST_ASSERT_EQ(e.valid, 1, "条目应有效");
    TEST_ASSERT_EQ(e.verified, 0, "verified 应如实取入参");

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
    sr_obs_entry_t e;

    memset(&g_t, 0, sizeof(g_t));
    obs1(&g_t, 0x0A000064u, 50000, 51820, 3, 100, 1);
    obs1(&g_t, 0x0A000064u, 50000, 51820, 7, 137, 1);

    TEST_ASSERT(find_entry(&g_t, 0x0A000064u, 50000, 51820, &e), "条目应存在");
    TEST_ASSERT_EQ(g_t.count, 1u, "命中不应新增条目");
    TEST_ASSERT_EQ(g_t.records, 2u, "records 应累加每一次观测");
    TEST_ASSERT_EQ(g_t.evictions, 0u, "命中不算淘汰");
    TEST_ASSERT_EQ(e.hits, 2u, "hits 应累加");
    TEST_ASSERT_EQ(e.first_seen, 100u, "first_seen 不应被后续观测改写");
    TEST_ASSERT_EQ(e.last_seen, 137u, "last_seen 应更新");
    /* 换收包接口不改写 rx_if：那是要控制器重做发现的事件，不是本地能解释的 */
    TEST_ASSERT_EQ(e.rx_if, 3, "rx_if 保留首见接口");
}

/* ------------------------------------------------------------------ */
/* test_verified_only_upgrades：认证事实不可降级                          */
/* ------------------------------------------------------------------ */
static void test_verified_only_upgrades(void)
{
    sr_obs_entry_t e;

    /* 先旁路包(0) 后 SR 验签包(1) => 升为 1 */
    memset(&g_t, 0, sizeof(g_t));
    obs1(&g_t, 0xC0A80101u, 51000, 51820, 2, 10, 0);
    obs1(&g_t, 0xC0A80101u, 51000, 51820, 2, 20, 1);
    TEST_ASSERT(find_entry(&g_t, 0xC0A80101u, 51000, 51820, &e), "条目应存在");
    TEST_ASSERT_EQ(e.verified, 1, "验签过的观测必须把条目升为 verified");

    /* 先 1 后 0 不得降回 0：认证过这件事不因后来的旁路包而失效 */
    obs1(&g_t, 0xC0A80101u, 51000, 51820, 2, 30, 0);
    TEST_ASSERT(find_entry(&g_t, 0xC0A80101u, 51000, 51820, &e), "条目应存在");
    TEST_ASSERT_EQ(e.verified, 1, "verified 不可降级");
    TEST_ASSERT_EQ(e.hits, 3u, "三次观测应累计 hits");
}

/*
 * 凑出 n 个 home 槽相同的元组：不硬编码撞槽常量，自己搜。
 *
 * 搜索游标只往前走（k_from 记住上次命中的位置）：从头扫会让每个 i 都找到
 * 同一个 k，凑出来的"8 个同 home 元组"其实是同一条元组，测试就空转了。
 * 16k 槽下平均每 16384 个候选命中一次，7 次搜索共约 12 万次哈希，代价可忽略。
 */
static int make_same_home(uint32_t *ips, uint16_t *sports, int n,
                          uint16_t dport, uint32_t *home_out)
{
    uint32_t home;
    long k_from = 0;
    int i;

    ips[0] = 0x0A000001u;
    sports[0] = 40000;
    home = sr_obs_slot(ips[0], sports[0], dport);

    for (i = 1; i < n; i++) {
        long k;
        int found = 0;

        for (k = k_from + 1; k < k_from + 4000000; k++) {
            uint32_t ip = 0x0B000000u + (uint32_t)k;
            uint16_t sp = (uint16_t)(20000 + (k % 40000));

            if (sr_obs_slot(ip, sp, dport) != home)
                continue;

            ips[i] = ip;
            sports[i] = sp;
            k_from = k;
            found = 1;
            break;
        }
        if (!found)
            return 0;
    }

    *home_out = home;
    return 1;
}

/*
 * 另找一条 home 相同的元组（第 9 条）。ip 基准与 make_same_home 不同，
 * 所以即便 k 重叠也不会撞出同一个元组。找不到返回 0。
 */
static uint32_t next_same_home(uint32_t home, uint16_t *sport_out,
                               uint16_t dport)
{
    long k;

    for (k = 1; k < 8000000; k++) {
        uint32_t ip = 0x0C000000u + (uint32_t)k;
        uint16_t sp = (uint16_t)(20000 + (k % 40000));

        if (sr_obs_slot(ip, sp, dport) != home)
            continue;

        *sport_out = sp;
        return ip;
    }
    return 0;
}

/* ------------------------------------------------------------------ */
/* test_probe_window_hit：条目落在窗口里非 home 的槽位，仍必须能命中        */
/*                                                                     */
/* 有界线性探测的价值全在这里：home 被占时往后找一格，而不是立刻挤掉别人。   */
/* ------------------------------------------------------------------ */
static void test_probe_window_hit(void)
{
    uint32_t ips[SR_OBS_PROBE_WINDOW];
    uint16_t sports[SR_OBS_PROBE_WINDOW];
    uint32_t home = 0;
    sr_obs_entry_t e;
    int i;

    TEST_ASSERT(make_same_home(ips, sports, SR_OBS_PROBE_WINDOW, 51820, &home),
                "应能凑出 home 槽相同的 8 个元组");

    memset(&g_t, 0, sizeof(g_t));
    for (i = 0; i < SR_OBS_PROBE_WINDOW; i++)
        obs1(&g_t, ips[i], sports[i], 51820, 1, (uint64_t)(100 + i), 1);

    /* 8 个同 home 的元组正好铺满窗口：一个都不该被淘汰 */
    TEST_ASSERT_EQ(g_t.count, (uint32_t)SR_OBS_PROBE_WINDOW, "窗口内容量应全部登记");
    TEST_ASSERT_EQ(g_t.evictions, 0u, "窗口没满就不该淘汰");
    /* 除第一个之外，其余都不在 home 槽：证明确实探测到了后面的格子 */
    TEST_ASSERT_EQ(g_t.slots[home].src_ip, ips[0], "home 槽应是最先登记的那条");

    for (i = 1; i < SR_OBS_PROBE_WINDOW; i++) {
        TEST_ASSERT(find_entry(&g_t, ips[i], sports[i], 51820, &e),
                    "窗口后面的条目必须还在");
        obs1(&g_t, ips[i], sports[i], 51820, 1, (uint64_t)(200 + i), 1);
        TEST_ASSERT(find_entry(&g_t, ips[i], sports[i], 51820, &e),
                    "重复观测不能把窗口后面的条目弄丢");
        TEST_ASSERT_EQ(e.hits, 2u, "命中应累加 hits");
        TEST_ASSERT_EQ(e.last_seen, (uint64_t)(200 + i), "命中应更新 last_seen");
    }

    TEST_ASSERT_EQ(g_t.count, (uint32_t)SR_OBS_PROBE_WINDOW, "命中不新增条目");
    TEST_ASSERT_EQ(g_t.evictions, 0u, "全程不该发生淘汰");
}

/* ------------------------------------------------------------------ */
/* test_window_full_evicts_home：窗口占满后新条目覆盖 home 槽并计淘汰       */
/* ------------------------------------------------------------------ */
static void test_window_full_evicts_home(void)
{
    uint32_t ips[SR_OBS_PROBE_WINDOW + 1];
    uint16_t sports[SR_OBS_PROBE_WINDOW + 1];
    uint32_t home = 0, ip_extra;
    sr_obs_entry_t e;
    int i, retained;

    TEST_ASSERT(make_same_home(ips, sports, SR_OBS_PROBE_WINDOW, 51820, &home),
                "应能凑出 home 槽相同的 8 个元组");

    /* 第 9 条：同样的 home 槽，且元组与前面 8 条都不同 */
    ip_extra = next_same_home(home, &sports[SR_OBS_PROBE_WINDOW], 51820);
    TEST_ASSERT(ip_extra != 0, "应能凑出第 9 条同 home 的元组");
    TEST_ASSERT(ip_extra != ips[0], "第 9 条必须是不同元组");

    memset(&g_t, 0, sizeof(g_t));
    for (i = 0; i < SR_OBS_PROBE_WINDOW; i++)
        obs1(&g_t, ips[i], sports[i], 51820, 1, (uint64_t)(100 + i), 1);
    TEST_ASSERT_EQ(g_t.evictions, 0u, "前置条件：填满窗口不算淘汰");

    obs1(&g_t, ip_extra, sports[SR_OBS_PROBE_WINDOW], 51820, 2, 500, 0);

    retained = (int)live_count(&g_t);
    TEST_ASSERT_EQ(g_t.count, (uint32_t)SR_OBS_PROBE_WINDOW,
                   "窗口满时覆盖不改变条目数");
    TEST_ASSERT_EQ(retained, SR_OBS_PROBE_WINDOW, "遍历条数应与 count 一致");
    TEST_ASSERT_EQ(g_t.evictions, 1u, "挤掉一条应计一次 eviction");
    TEST_ASSERT(find_entry(&g_t, ip_extra, sports[SR_OBS_PROBE_WINDOW], 51820, &e),
                "新条目应登记成功");
    TEST_ASSERT_EQ(e.hits, 1u, "新条目从 hits=1 开始");
    TEST_ASSERT_EQ(e.verified, 0, "新条目的 verified 不被旧值污染");

    /* 被淘汰的只能是 home 槽那条（ips[0]）：其余仍在窗口里 */
    TEST_ASSERT(!find_entry(&g_t, ips[0], sports[0], 51820, &e),
                "home 槽的旧条目应被覆盖");
    for (i = 1; i < SR_OBS_PROBE_WINDOW; i++)
        TEST_ASSERT(find_entry(&g_t, ips[i], sports[i], 51820, &e),
                    "窗口后面的旧条目不该被连带淘汰");

    /* keepalive 会自愈：再观测被挤掉的那条，它反过来挤掉别人 */
    obs1(&g_t, ips[0], sports[0], 51820, 1, 600, 1);
    TEST_ASSERT_EQ(g_t.evictions, 2u, "反向覆盖同样计淘汰");
    TEST_ASSERT(find_entry(&g_t, ips[0], sports[0], 51820, &e),
                "旧条目下一轮会自己回来");
}

/* ------------------------------------------------------------------ */
/* test_clear：清空把条目与计数一起归零                                  */
/* ------------------------------------------------------------------ */
static void test_clear(void)
{
    sr_obs_entry_t e;
    int i;

    memset(&g_t, 0, sizeof(g_t));
    for (i = 0; i < 10; i++)
        obs1(&g_t, 0x0A000001u + (uint32_t)i * 7u, (uint16_t)(40000 + i), 51820,
             1, (uint64_t)i, 1);
    TEST_ASSERT(g_t.count > 0, "前置条件：表里应有条目");

    sr_obs_clear(&g_t);
    TEST_ASSERT_EQ(g_t.count, 0u, "clear 后 count 归零");
    TEST_ASSERT_EQ(g_t.records, 0u, "clear 后 records 归零");
    TEST_ASSERT_EQ(g_t.evictions, 0u, "clear 后 evictions 归零");
    TEST_ASSERT_EQ(live_count(&g_t), 0u, "clear 后遍历应取不到条目");
    for (i = 0; i < SR_OBS_SLOTS; i++)
        TEST_ASSERT_EQ(g_t.slots[i].valid, 0, "clear 后不得残留有效条目");

    /* clear 之后同一元组重新观测：是新建，不是命中旧值 */
    obs1(&g_t, 0x0A000001u, 40000, 51820, 5, 777, 0);
    TEST_ASSERT(find_entry(&g_t, 0x0A000001u, 40000, 51820, &e), "应能重新登记");
    TEST_ASSERT_EQ(e.hits, 1u, "clear 后 hits 从 1 开始");
    TEST_ASSERT_EQ(e.rx_if, 5, "clear 后 rx_if 取新值");
}

/* ------------------------------------------------------------------ */
/* test_next_traversal：遍历取尽所有有效条目，游标可续、可探               */
/* ------------------------------------------------------------------ */
static void test_next_traversal(void)
{
    sr_obs_entry_t e, e2, probe;
    uint32_t cursor = 0, seen = 0;
    uint32_t c_part, c_rest, c1, c2;
    int i, n_part = 0, n_rest = 0, half_seen, p, r;

    memset(&g_t, 0, sizeof(g_t));
    for (i = 0; i < 20; i++)
        obs1(&g_t, 0x0A000001u + (uint32_t)i * 7u, (uint16_t)(40000 + i), 51820,
             1, (uint64_t)i, 1);

    while (sr_obs_next(&g_t, &cursor, &e))
        seen++;

    TEST_ASSERT_EQ(seen, g_t.count, "遍历条数必须等于 count");
    TEST_ASSERT_EQ(sr_obs_next(&g_t, &cursor, &e), 0, "取尽后应返回 0");
    TEST_ASSERT_EQ(cursor, (uint32_t)SR_OBS_SLOTS, "取尽后游标停在表尾");

    /*
     * 游标可续：取到一半记下游标，从该游标继续取完，两段之和必须正好是全量。
     * 这里不能用"槽号过半"来切 —— 游标语义是"从 cursor 起向后扫"，
     * 停在第 N 条之后的游标可能仍小于表长的一半。
     */
    half_seen = (int)seen / 2;
    c_part = 0;
    while (n_part < half_seen && sr_obs_next(&g_t, &c_part, &e))
        n_part++;
    c_rest = c_part;
    while (sr_obs_next(&g_t, &c_rest, &e))
        n_rest++;

    TEST_ASSERT_EQ(n_part + n_rest, (int)seen, "分段遍历之和必须等于全量");
    TEST_ASSERT_EQ(c_rest, (uint32_t)SR_OBS_SLOTS, "续遍历取尽后游标也在表尾");

    /* out==NULL 只是"还有没有下一条"的探测：前进的游标与真实读取完全一致 */
    c1 = 0;
    c2 = 0;
    p = sr_obs_next(&g_t, &c1, NULL);
    r = sr_obs_next(&g_t, &c2, &probe);
    TEST_ASSERT_EQ(p, r, "探测与读取应给出同样的'有没有'");
    TEST_ASSERT_EQ(c1, c2, "探测与读取推进同一个游标");
    TEST_ASSERT_EQ(sr_obs_next(&g_t, &c1, &e), 1, "探测之后仍可读取");
    sr_obs_next(&g_t, &c2, &e2);
    TEST_ASSERT_EQ(e.src_ip, e2.src_ip, "探测不得改变后续读到的条目");
}

void test_suite_sr_obs(void)
{
    TEST_SUITE_BEGIN("sr_obs 外层源观测表");
    RUN_TEST(test_slot_is_deterministic);
    RUN_TEST(test_slot_spreads_adjacent_peers);
    RUN_TEST(test_capacity_4096_sources);
    RUN_TEST(test_record_insert);
    RUN_TEST(test_record_hit);
    RUN_TEST(test_verified_only_upgrades);
    RUN_TEST(test_probe_window_hit);
    RUN_TEST(test_window_full_evicts_home);
    RUN_TEST(test_clear);
    RUN_TEST(test_next_traversal);
    TEST_SUITE_END();
}
