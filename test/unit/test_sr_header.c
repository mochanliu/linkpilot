/*
 * test_sr_header.c - SR 头线格式（sr_header.h）单元测试
 *
 * 覆盖：初始化、长度计算、段读写、边界条件、验证、
 *       段弹出（pop）、下一跳（get_next_hop）、序列化长度。
 */
#include "test_framework.h"
#include "sr_header.h"

/* 运行本文件全部用例（供 test_main.c 调用） */
void test_suite_sr_header(void);

/* ------------------------------------------------------------------ */
/* test_hdr_init：验证 sr_hdr_init 正确设置各固定字段                   */
/* ------------------------------------------------------------------ */
static void test_hdr_init(void)
{
    sr_header_t h;
    int rc = sr_hdr_init(&h, 3);

    TEST_ASSERT_EQ(rc, 0, "sr_hdr_init(3) 应返回 0");
    TEST_ASSERT_EQ(h.magic, SR_MAGIC, "magic 应为 0x53");
    TEST_ASSERT_EQ(h.ver_flags, SR_VER_FLAGS, "ver_flags 应为版本1|标志0");
    TEST_ASSERT_EQ(h.hdr_len, SR_HDR_LEN(3), "hdr_len 应为 24 + 3*6 = 42");
    TEST_ASSERT_EQ(h.seg_count, 3, "seg_count 应为 3");
    TEST_ASSERT_EQ(h.seg_left, 3, "seg_left 初值应等于 seg_count");
    TEST_ASSERT_EQ(h.action, SR_ACTION_FORWARD, "action 应为 FORWARD");

    /* reserved 与段列表应清零 */
    TEST_ASSERT_EQ(h.reserved[0], 0, "reserved[0] 应为 0");
    TEST_ASSERT_EQ(h.reserved[1], 0, "reserved[1] 应为 0");
    TEST_ASSERT_MEM_EQ(h.segments[0].ip, "\x00\x00\x00\x00", 4,
                       "段列表应清零");

    /* 非法参数：seg_count == 0 */
    TEST_ASSERT_EQ(sr_hdr_init(&h, 0), -1, "seg_count=0 应返回 -1");
    /* 非法参数：seg_count > SR_MAX_SEGS */
    TEST_ASSERT_EQ(sr_hdr_init(&h, SR_MAX_SEGS + 1), -1,
                   "seg_count 超过上限应返回 -1");
    /* 非法参数：h == NULL */
    TEST_ASSERT_EQ(sr_hdr_init(NULL, 1), -1, "h=NULL 应返回 -1");
}

/* ------------------------------------------------------------------ */
/* test_hdr_total_len：验证不同 seg_count 下的长度计算                    */
/*   0 段=24B（宏定义），1 段=30B，4 段=48B，8 段=72B                     */
/* ------------------------------------------------------------------ */
static void test_hdr_total_len(void)
{
    /* SR_HDR_LEN(segs) = 16 + segs*6 + 8 = 24 + segs*6 */
    TEST_ASSERT_EQ(SR_HDR_LEN(0), 24, "0 段总长应为 24B");
    TEST_ASSERT_EQ(SR_HDR_LEN(1), 30, "1 段总长应为 30B");
    TEST_ASSERT_EQ(SR_HDR_LEN(4), 48, "4 段总长应为 48B");
    TEST_ASSERT_EQ(SR_HDR_LEN(8), 72, "8 段总长应为 72B");

    /* sr_hdr_total_len 应与宏一致 */
    TEST_ASSERT_EQ(sr_hdr_total_len(0), 24, "sr_hdr_total_len(0) 应为 24");
    TEST_ASSERT_EQ(sr_hdr_total_len(1), 30, "sr_hdr_total_len(1) 应为 30");
    TEST_ASSERT_EQ(sr_hdr_total_len(4), 48, "sr_hdr_total_len(4) 应为 48");
    TEST_ASSERT_EQ(sr_hdr_total_len(8), 72, "sr_hdr_total_len(8) 应为 72");

    /* SR_HDR_MAX_LEN 应等于 8 段时的长度 */
    TEST_ASSERT_EQ(SR_HDR_MAX_LEN, 72, "SR_HDR_MAX_LEN 应为 72");
}

/* ------------------------------------------------------------------ */
/* test_hdr_set_get_segment：设置段后读回，验证 IP/Port（网络序）        */
/* ------------------------------------------------------------------ */
static void test_hdr_set_get_segment(void)
{
    sr_header_t h;
    sr_segment_t *seg;
    uint32_t ip_net;    /* 网络序 IPv4 */
    uint16_t port_net;  /* 网络序 Port */

    TEST_ASSERT_EQ(sr_hdr_init(&h, 3), 0, "init 应成功");

    /* 构造 192.168.1.10:51820 的网络序字节 */
    ip_net   = htonl(0xC0A8010A);   /* 192.168.1.10 */
    port_net = htons(SR_PORT_VPN);  /* 51820 */

    TEST_ASSERT_EQ(sr_hdr_set_segment(&h, 1, ip_net, port_net), 0,
                   "设置 index=1 段应成功");

    seg = sr_hdr_get_segment(&h, 1);
    TEST_ASSERT_NOT_NULL(seg, "index=1 段指针应非 NULL");
    TEST_ASSERT_MEM_EQ(seg->ip, &ip_net, 4, "段 IP 应与写入的网络序一致");
    TEST_ASSERT_MEM_EQ(seg->port, &port_net, 2, "段 Port 应与写入的网络序一致");

    /* 逐字节验证网络序（192,168,1,10 / 51820 = 0xCA,0x6C） */
    TEST_ASSERT_EQ(seg->ip[0], 192, "IP 首字节应为 192");
    TEST_ASSERT_EQ(seg->ip[1], 168, "IP 第二字节应为 168");
    TEST_ASSERT_EQ(seg->ip[2], 1,   "IP 第三字节应为 1");
    TEST_ASSERT_EQ(seg->ip[3], 10,  "IP 末字节应为 10");
    TEST_ASSERT_EQ(seg->port[0], (uint8_t)(SR_PORT_VPN >> 8),
                   "Port 高字节应为网络序高字节");
    TEST_ASSERT_EQ(seg->port[1], (uint8_t)(SR_PORT_VPN & 0xFF),
                   "Port 低字节应为网络序低字节");

    /* 越界读应返回 NULL */
    TEST_ASSERT_NULL(sr_hdr_get_segment(&h, -1), "index=-1 应返回 NULL");
    TEST_ASSERT_NULL(sr_hdr_get_segment(&h, 3), "index>=seg_count 应返回 NULL");
    /* 越界写应返回 -1 */
    TEST_ASSERT_EQ(sr_hdr_set_segment(&h, 3, ip_net, port_net), -1,
                   "越界写段应返回 -1");
    TEST_ASSERT_EQ(sr_hdr_set_segment(&h, -1, ip_net, port_net), -1,
                   "index=-1 写段应返回 -1");
    TEST_ASSERT_EQ(sr_hdr_set_segment(NULL, 0, ip_net, port_net), -1,
                   "h=NULL 写段应返回 -1");
}

/* ------------------------------------------------------------------ */
/* test_hdr_boundary：0 段、1 段、MAX(8) 段边界                         */
/* ------------------------------------------------------------------ */
static void test_hdr_boundary(void)
{
    sr_header_t h;
    int i;

    /* 0 段：非法，init 失败 */
    TEST_ASSERT_EQ(sr_hdr_init(&h, 0), -1, "0 段 init 应失败");

    /* 1 段：最小合法路径 */
    TEST_ASSERT_EQ(sr_hdr_init(&h, 1), 0, "1 段 init 应成功");
    TEST_ASSERT_EQ(h.hdr_len, 30, "1 段 hdr_len 应为 30");
    TEST_ASSERT_EQ(h.seg_left, 1, "1 段 seg_left 应为 1");
    TEST_ASSERT_NOT_NULL(sr_hdr_get_next_hop(&h), "1 段应有下一跳");

    /* MAX(8) 段：最大合法路径 */
    TEST_ASSERT_EQ(sr_hdr_init(&h, SR_MAX_SEGS), 0, "8 段 init 应成功");
    TEST_ASSERT_EQ(h.seg_count, SR_MAX_SEGS, "seg_count 应为 8");
    TEST_ASSERT_EQ(h.hdr_len, 72, "8 段 hdr_len 应为 72");

    /* 所有 8 段均可读写 */
    for (i = 0; i < SR_MAX_SEGS; i++) {
        uint32_t ip = htonl(0x0A000001u + (uint32_t)i);
        uint16_t pt = htons((uint16_t)(SR_PORT_TRANSIT + i));
        TEST_ASSERT_EQ(sr_hdr_set_segment(&h, i, ip, pt), 0, "8 段内写应成功");
    }
    for (i = 0; i < SR_MAX_SEGS; i++) {
        sr_segment_t *s = sr_hdr_get_segment(&h, i);
        TEST_ASSERT_NOT_NULL(s, "8 段内读应非 NULL");
    }
    /* 第 9 段越界 */
    TEST_ASSERT_NULL(sr_hdr_get_segment(&h, SR_MAX_SEGS),
                     "index=8 越界应返回 NULL");

    /* 超过 MAX 段：非法 */
    TEST_ASSERT_EQ(sr_hdr_init(&h, SR_MAX_SEGS + 1), -1,
                   "9 段 init 应失败");
}

/* ------------------------------------------------------------------ */
/* test_hdr_validate：正常/错误 magic/错误 version/长度不一致/buf 不足   */
/* ------------------------------------------------------------------ */
static void test_hdr_validate(void)
{
    sr_header_t h;

    /* 正常头部 */
    TEST_ASSERT_EQ(sr_hdr_init(&h, 3), 0, "init 应成功");
    TEST_ASSERT_EQ(sr_hdr_validate(&h, SR_HDR_LEN(3)), 0,
                   "正常头部验证应通过");

    /* 错误 magic */
    h.magic = 0x00;
    TEST_ASSERT_EQ(sr_hdr_validate(&h, SR_HDR_LEN(3)), -1,
                   "错误 magic 应返回 -1");
    h.magic = SR_MAGIC;  /* 复位 */

    /* 错误 version（改高 4 位） */
    h.ver_flags = (uint8_t)((0x02 << 4) | 0x00);
    TEST_ASSERT_EQ(sr_hdr_validate(&h, SR_HDR_LEN(3)), -1,
                   "错误 version 应返回 -1");
    h.ver_flags = SR_VER_FLAGS;  /* 复位 */

    /* hdr_len 与 seg_count 不一致 */
    h.hdr_len = (uint8_t)(SR_HDR_LEN(3) + 1);
    TEST_ASSERT_EQ(sr_hdr_validate(&h, SR_HDR_LEN(3) + 1), -1,
                   "hdr_len 与实际不一致应返回 -1");
    h.hdr_len = (uint8_t)SR_HDR_LEN(3);  /* 复位 */

    /* buf_len 不足以容纳完整头部 */
    TEST_ASSERT_EQ(sr_hdr_validate(&h, SR_HDR_LEN(3) - 1), -1,
                   "buf_len 不足应返回 -1");

    /* buf_len 小于固定头部长度 */
    TEST_ASSERT_EQ(sr_hdr_validate(&h, SR_FIXED_HDR_LEN - 1), -1,
                   "buf_len < 固定长度应返回 -1");

    /* seg_left > seg_count 非法 */
    h.seg_left = (uint8_t)(h.seg_count + 1);
    TEST_ASSERT_EQ(sr_hdr_validate(&h, SR_HDR_LEN(3)), -1,
                   "seg_left > seg_count 应返回 -1");
    h.seg_left = h.seg_count;  /* 复位 */

    /* h == NULL */
    TEST_ASSERT_EQ(sr_hdr_validate(NULL, SR_HDR_LEN(3)), -1,
                   "h=NULL 应返回 -1");
}

/* ------------------------------------------------------------------ */
/* test_hdr_pop_segment：3 段路径连续 pop，验证顺序与 seg_left 递减      */
/* ------------------------------------------------------------------ */
static void test_hdr_pop_segment(void)
{
    sr_header_t h;
    sr_segment_t out;
    uint32_t ip0 = htonl(0x0A000001);  /* segments[0] = 终点 */
    uint32_t ip1 = htonl(0x0A000002);  /* segments[1] = 中转2 */
    uint32_t ip2 = htonl(0x0A000003);  /* segments[2] = 第一跳 */
    uint16_t pt  = htons(SR_PORT_TRANSIT);

    TEST_ASSERT_EQ(sr_hdr_init(&h, 3), 0, "init 应成功");
    sr_hdr_set_segment(&h, 0, ip0, pt);
    sr_hdr_set_segment(&h, 1, ip1, pt);
    sr_hdr_set_segment(&h, 2, ip2, pt);

    /* 初始 seg_left == 3 */
    TEST_ASSERT_EQ(h.seg_left, 3, "初始 seg_left 应为 3");

    /* 第一次 pop：应取 segments[seg_left-1] = segments[2] = ip2 */
    TEST_ASSERT_EQ(sr_hdr_pop_segment(&h, &out), 0, "第一次 pop 应成功");
    TEST_ASSERT_MEM_EQ(out.ip, &ip2, 4, "第一次 pop 应为 segments[2]");
    TEST_ASSERT_EQ(h.seg_left, 2, "第一次 pop 后 seg_left 应为 2");

    /* 第二次 pop：应取 segments[1] = ip1 */
    TEST_ASSERT_EQ(sr_hdr_pop_segment(&h, &out), 0, "第二次 pop 应成功");
    TEST_ASSERT_MEM_EQ(out.ip, &ip1, 4, "第二次 pop 应为 segments[1]");
    TEST_ASSERT_EQ(h.seg_left, 1, "第二次 pop 后 seg_left 应为 1");

    /* 第三次 pop：应取 segments[0] = ip0 */
    TEST_ASSERT_EQ(sr_hdr_pop_segment(&h, &out), 0, "第三次 pop 应成功");
    TEST_ASSERT_MEM_EQ(out.ip, &ip0, 4, "第三次 pop 应为 segments[0]");
    TEST_ASSERT_EQ(h.seg_left, 0, "第三次 pop 后 seg_left 应为 0");

    /* seg_left == 0 时 pop 应失败 */
    TEST_ASSERT_EQ(sr_hdr_pop_segment(&h, &out), -1,
                   "seg_left==0 时 pop 应失败");

    /* 非法参数 */
    TEST_ASSERT_EQ(sr_hdr_pop_segment(NULL, &out), -1, "h=NULL pop 应失败");
    TEST_ASSERT_EQ(sr_hdr_pop_segment(&h, NULL), -1, "out=NULL pop 应失败");
}

/* ------------------------------------------------------------------ */
/* test_hdr_get_next_hop：验证返回 segments[seg_left-1] 的指针           */
/* ------------------------------------------------------------------ */
static void test_hdr_get_next_hop(void)
{
    sr_header_t h;
    sr_segment_t *next;
    uint32_t ip0 = htonl(0xC0A80001);
    uint32_t ip2 = htonl(0xC0A80003);
    uint16_t pt  = htons(SR_PORT_TRANSIT);

    TEST_ASSERT_EQ(sr_hdr_init(&h, 3), 0, "init 应成功");
    sr_hdr_set_segment(&h, 0, ip0, pt);
    sr_hdr_set_segment(&h, 2, ip2, pt);

    /* seg_left == 3，下一跳应为 segments[2] */
    next = sr_hdr_get_next_hop(&h);
    TEST_ASSERT_NOT_NULL(next, "seg_left=3 时下一跳应非 NULL");
    TEST_ASSERT_MEM_EQ(next->ip, &ip2, 4, "下一跳应为 segments[2]");
    /* get_next_hop 不应修改 seg_left */
    TEST_ASSERT_EQ(h.seg_left, 3, "get_next_hop 不应修改 seg_left");

    /* 手动把 seg_left 置为 1，下一跳应为 segments[0] */
    h.seg_left = 1;
    next = sr_hdr_get_next_hop(&h);
    TEST_ASSERT_NOT_NULL(next, "seg_left=1 时下一跳应非 NULL");
    TEST_ASSERT_MEM_EQ(next->ip, &ip0, 4, "seg_left=1 时下一跳应为 segments[0]");

    /* seg_left == 0（已到终点）应返回 NULL */
    h.seg_left = 0;
    TEST_ASSERT_NULL(sr_hdr_get_next_hop(&h), "seg_left=0 时应返回 NULL");

    /* h == NULL 应返回 NULL */
    TEST_ASSERT_NULL(sr_hdr_get_next_hop(NULL), "h=NULL 时应返回 NULL");
}

/* ------------------------------------------------------------------ */
/* test_hdr_serialize_len：返回值应等于 SR_HDR_LEN(seg_count)            */
/* ------------------------------------------------------------------ */
static void test_hdr_serialize_len(void)
{
    int s;
    for (s = 0; s <= SR_MAX_SEGS; s++) {
        TEST_ASSERT_EQ(sr_hdr_serialize_len((uint8_t)s), SR_HDR_LEN(s),
                       "serialize_len 应等于 SR_HDR_LEN");
        TEST_ASSERT_EQ(sr_hdr_serialize_len((uint8_t)s), sr_hdr_total_len((uint8_t)s),
                       "serialize_len 应等于 total_len");
    }

    /* 与初始化后头部的 hdr_len 一致 */
    {
        sr_header_t h;
        TEST_ASSERT_EQ(sr_hdr_init(&h, 4), 0, "init 应成功");
        TEST_ASSERT_EQ(sr_hdr_serialize_len(h.seg_count), h.hdr_len,
                       "serialize_len 应等于 hdr_len 字段");
    }
}

/* ------------------------------------------------------------------ */
/* 套件入口                                                             */
/* ------------------------------------------------------------------ */
void test_suite_sr_header(void)
{
    TEST_SUITE_BEGIN("SR Header (sr_header.h)");
    RUN_TEST(test_hdr_init);
    RUN_TEST(test_hdr_total_len);
    RUN_TEST(test_hdr_set_get_segment);
    RUN_TEST(test_hdr_boundary);
    RUN_TEST(test_hdr_validate);
    RUN_TEST(test_hdr_pop_segment);
    RUN_TEST(test_hdr_get_next_hop);
    RUN_TEST(test_hdr_serialize_len);
    TEST_SUITE_END();
}
