/*
 * sr_obs.c - 外层源观测表实现
 *
 * 开放寻址：home = hash(src_ip, src_port, dst_port)，从 home 起最多探测
 * SR_OBS_PROBE_WINDOW 个槽。命中（三元组完全相同）就更新 last_seen/hits/
 * verified；窗口里的第一个空槽就新建；窗口占满就覆盖 home 并计一次 eviction。
 * 没有 LRU、没有分配、没有锁，热路径工作量是常数。
 * 可信度分档的语义（verified）见 sr_obs.h 文件头。
 */
#include "sr_obs.h"

#include <string.h>

_Static_assert((SR_OBS_SLOTS & (SR_OBS_SLOTS - 1)) == 0,
               "SR_OBS_SLOTS must be a power of two (sr_obs_slot masks with &)");

/*
 * 哈希：把端口挪位后与 ip 混合。
 *
 * 不用 ip ^ port 这种朴素式子：vpn_port 是常量（51820），端口异或只在低 16
 * 位起作用，而不同 peer 的 src_ip 往往只差最后一字节，实测会让相邻 peer
 * 撞同一槽。这里对 ip 做一遍 murmur 式的乘-移位，把低位的差异扩散到高位再
 * 与端口混合。
 */
uint32_t
sr_obs_slot(uint32_t src_ip, uint16_t src_port, uint16_t dst_port)
{
    uint32_t h = src_ip;

    h ^= h >> 16;
    h *= 0x7feb352dU;
    h ^= h >> 15;

    /* 两个端口都参与：dst_port 区分同一源打到本机不同功能端口 */
    h ^= (uint32_t) src_port * 2654435761U;
    h ^= (uint32_t) dst_port << 8;

    h ^= h >> 13;
    return h & (SR_OBS_SLOTS - 1);   /* SR_OBS_SLOTS 必须是 2 的幂 */
}

/* 元组判同：只有三元组，rx_if 与时间不参与（理由见 sr_obs_record） */
static int
sr_obs_same(const sr_obs_entry_t *e, uint32_t src_ip, uint16_t src_port,
            uint16_t dst_port)
{
    return e->src_ip == src_ip && e->src_port == src_port &&
           e->dst_port == dst_port;
}

void
sr_obs_record(sr_obs_table_t *t, uint32_t src_ip, uint16_t src_port,
              uint16_t dst_port, uint16_t rx_if, uint64_t now_sec,
              uint8_t verified)
{
    sr_obs_entry_t *e = 0;
    uint32_t home, k;

    if (!t)
        return;

    home = sr_obs_slot(src_ip, src_port, dst_port);

    /*
     * 有界线性探测。命中同元组就更新；撞到空槽就是"不在表里"，落在这儿。
     *
     * "空槽 = 不存在"这条推理要求槽位一旦有效就永不单独转空 —— 本表只有整体
     * clear，没有单条删除，所以成立。将来若加"按元组过期删除"，必须同时改成
     * 带墓碑的探测，否则探测会在删除点提前停下、把后面的条目看成不存在。
     */
    for (k = 0; k < SR_OBS_PROBE_WINDOW; k++) {
        sr_obs_entry_t *p = &t->slots[(home + k) & (SR_OBS_SLOTS - 1)];

        if (!p->valid) {
            e = p;
            break;
        }
        if (sr_obs_same(p, src_ip, src_port, dst_port)) {
            e = p;
            break;
        }
    }

    if (!e)
        e = &t->slots[home];    /* 窗口占满：新条目赢，覆盖 home */

    if (e->valid && sr_obs_same(e, src_ip, src_port, dst_port)) {
        e->last_seen = now_sec;
        e->hits++;
        /* 认证过是不可降级的事实：先收到旁路包(0)、后收到 SR 验签包(1) 会升到 1 */
        e->verified |= (verified ? 1 : 0);
        /* rx_if 不更新：同一元组换收包接口意味着链路变了，那是控制器要重新
           做发现的事件，不是这里能解释的细节。保留首见接口。 */
        t->records++;
        return;
    }

    if (e->valid)
        t->evictions++;
    else
        t->count++;

    e->src_ip = src_ip;
    e->src_port = src_port;
    e->dst_port = dst_port;
    e->rx_if = rx_if;
    e->first_seen = now_sec;
    e->last_seen = now_sec;
    e->hits = 1;
    e->valid = 1;
    e->verified = (verified ? 1 : 0);
    t->records++;
}

void
sr_obs_clear(sr_obs_table_t *t)
{
    if (!t)
        return;
    memset(t, 0, sizeof(*t));
}

int
sr_obs_next(const sr_obs_table_t *t, uint32_t *cursor, sr_obs_entry_t *out)
{
    uint32_t i;

    if (!t || !cursor)
        return 0;

    for (i = *cursor; i < SR_OBS_SLOTS; i++) {
        if (!t->slots[i].valid)
            continue;
        if (out)
            *out = t->slots[i];
        *cursor = i + 1;
        return 1;
    }
    *cursor = SR_OBS_SLOTS;
    return 0;
}
