/*
 * sr_path.c - 路径表管理实现
 *
 * 存储紧凑、定位 O(1)：
 *   - paths[0..count-1] 为有效条目（保持紧凑，遍历照旧走 count）
 *   - index[path_id] = 槽位 + 1，0 = 不存在（path_id 是 16 位，表定长）
 *   - add: path_id 已存在则原地覆盖，否则追加到 paths[count++] 并登记索引
 *   - del: 末尾条目搬到被删槽位（搬完要改它的索引），count--
 *   - lookup: 一次数组访问 —— encap 热路径每个出站包都要查一次，
 *             4096 条容量下线性扫描不能接受
 */
#include "sr_path.h"

#include <string.h>

int sr_path_table_init(sr_path_table_t *t)
{
    if (!t)
        return -1;
    memset(t, 0, sizeof(*t));
    t->count = 0;
    return 0;
}

/*
 * 内部：按 path_id 定位槽位下标，未找到返回 -1。
 *
 * 索引是加速用的，不是唯一事实来源：回读 paths[slot].path_id 自校验，
 * 一旦两者不一致就当作"没有这条路"，不能让一条写坏的索引继续参与选路。
 */
static int sr_path_slot(const sr_path_table_t *t, uint16_t path_id)
{
    uint16_t v = t->index[path_id];
    int slot;

    if (v == 0)
        return -1;

    slot = (int)v - 1;
    if (slot < 0 || slot >= t->count)
        return -1;
    if (t->paths[slot].path_id != path_id)
        return -1;
    return slot;
}

int sr_path_add(sr_path_table_t *t, const sr_path_config_t *cfg)
{
    int idx;

    if (!t || !cfg)
        return SR_PATH_ERR_PARAM;
    if (cfg->seg_count == 0 || cfg->seg_count > SR_MAX_SEGS)
        return SR_PATH_ERR_PARAM;

    idx = sr_path_slot(t, cfg->path_id);
    if (idx >= 0) {
        /* 覆盖已有路径：槽位不变，索引照旧有效 */
        t->paths[idx] = *cfg;
        return SR_PATH_OK;
    }

    if (t->count >= SR_MAX_PATHS)
        return SR_PATH_ERR_FULL;

    t->paths[t->count] = *cfg;
    t->index[cfg->path_id] = (uint16_t)(t->count + 1);
    t->count++;
    return SR_PATH_OK;
}

int sr_path_del(sr_path_table_t *t, uint16_t path_id)
{
    int idx;

    if (!t)
        return SR_PATH_ERR_PARAM;

    idx = sr_path_slot(t, path_id);
    if (idx < 0)
        return SR_PATH_ERR_PARAM;

    /* 本条目的索引先注销 */
    t->index[path_id] = 0;

    /* 将末尾条目搬到被删槽位，保持紧凑（搬过来的条目槽位变了，索引要跟着改） */
    if (idx != t->count - 1) {
        t->paths[idx] = t->paths[t->count - 1];
        t->index[t->paths[idx].path_id] = (uint16_t)(idx + 1);
    }

    memset(&t->paths[t->count - 1], 0, sizeof(sr_path_config_t));
    t->count--;
    return SR_PATH_OK;
}

sr_path_config_t *sr_path_lookup(sr_path_table_t *t, uint16_t path_id)
{
    int idx;

    if (!t)
        return NULL;

    idx = sr_path_slot(t, path_id);
    if (idx < 0)
        return NULL;
    return &t->paths[idx];
}

int sr_path_count_using_hmac_id(const sr_path_table_t *t, uint16_t hmac_id)
{
    int i, n = 0;

    if (!t)
        return 0;

    for (i = 0; i < t->count; i++) {
        if (t->paths[i].hmac_id == hmac_id)
            n++;
    }
    return n;
}

int sr_path_replace_pkey(sr_path_table_t *t, uint16_t hmac_id,
                         const uint8_t *pkey)
{
    int i, n = 0;

    if (!t || !pkey)
        return 0;

    for (i = 0; i < t->count; i++) {
        if (t->paths[i].hmac_id != hmac_id)
            continue;
        memcpy(t->paths[i].pkey, pkey, SR_PKEY_LEN);
        n++;
    }
    return n;
}

int sr_path_fill_header(const sr_path_config_t *cfg, sr_header_t *h,
                        uint32_t timestamp)
{
    int i;

    if (!cfg || !h)
        return -1;
    if (cfg->seg_count == 0 || cfg->seg_count > SR_MAX_SEGS)
        return -1;

    /* 初始化固定字段（magic/ver_flags/hdr_len/seg_left/seg_count/action） */
    if (sr_hdr_init(h, cfg->seg_count) != 0)
        return -1;

    /* 可扩展部分 */
    sr_hdr_set_hmac_id(h, cfg->hmac_id);
    sr_hdr_set_path_id(h, cfg->path_id);
    sr_hdr_set_timestamp(h, timestamp);

    /* 段列表：cfg 中已为网络序，直接拷贝字节 */
    for (i = 0; i < cfg->seg_count; i++)
        h->segments[i] = cfg->segments[i];

    /* HMAC 签名（依赖上面所有字段已就位） */
    if (sr_hmac_sign(h, cfg->pkey) != 0)
        return -1;

    return 0;
}
