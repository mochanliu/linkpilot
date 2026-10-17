/*
 * sr_path.c - 路径表管理实现
 *
 * 采用紧凑存储：paths[0..count-1] 为有效条目。
 *   - add: path_id 已存在则覆盖，否则追加到 paths[count++]
 *   - del: 找到后将末尾条目搬到被删槽位，count--
 *   - lookup: 线性查找（表规模 <= 64，足够快）
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

/* 内部：按 path_id 查找槽位下标，未找到返回 -1 */
static int sr_path_find_index(const sr_path_table_t *t, uint16_t path_id)
{
    int i;
    for (i = 0; i < t->count; i++) {
        if (t->paths[i].path_id == path_id)
            return i;
    }
    return -1;
}

int sr_path_add(sr_path_table_t *t, const sr_path_config_t *cfg)
{
    int idx;

    if (!t || !cfg)
        return -1;
    if (cfg->seg_count == 0 || cfg->seg_count > SR_MAX_SEGS)
        return -1;

    idx = sr_path_find_index(t, cfg->path_id);
    if (idx >= 0) {
        /* 覆盖已有路径 */
        t->paths[idx] = *cfg;
        return 0;
    }

    if (t->count >= SR_MAX_PATHS)
        return -1;  /* 表已满 */

    t->paths[t->count] = *cfg;
    t->count++;
    return 0;
}

int sr_path_del(sr_path_table_t *t, uint16_t path_id)
{
    int idx;

    if (!t)
        return -1;

    idx = sr_path_find_index(t, path_id);
    if (idx < 0)
        return -1;

    /* 将末尾条目搬到被删槽位，保持紧凑 */
    if (idx != t->count - 1)
        t->paths[idx] = t->paths[t->count - 1];

    memset(&t->paths[t->count - 1], 0, sizeof(sr_path_config_t));
    t->count--;
    return 0;
}

sr_path_config_t *sr_path_lookup(sr_path_table_t *t, uint16_t path_id)
{
    int idx;

    if (!t)
        return NULL;

    idx = sr_path_find_index(t, path_id);
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
