/*
 * sr_obs.c - 外层源观测表实现
 *
 * 直接映射：slot = hash(src_ip, src_port, dst_port) % SR_OBS_SLOTS。
 * 命中（三元组完全相同）就更新 last_seen/hits/verified；不命中就覆盖该槽，
 * 覆盖掉有效条目时计一次 eviction。没有 LRU、没有分配、没有锁。
 * 可信度分档的语义（verified）见 sr_obs.h 文件头。
 */
#include "sr_obs.h"

#include <string.h>

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

void
sr_obs_record(sr_obs_table_t *t, uint32_t src_ip, uint16_t src_port,
              uint16_t dst_port, uint16_t rx_if, uint64_t now_sec,
              uint8_t verified)
{
    sr_obs_entry_t *e;

    if (!t)
        return;

    e = &t->slots[sr_obs_slot(src_ip, src_port, dst_port)];

    if (e->valid && e->src_ip == src_ip && e->src_port == src_port &&
        e->dst_port == dst_port) {
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
