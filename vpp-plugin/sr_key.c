/*
 * sr_key.c - P-Key 密钥管理
 *
 * 线性查找（最多 64 条），add/del/lookup 实现。
 */
#include "sr_wg.h"

/*
 * 查找 hmac_id 对应的密钥条目。
 * 返回条目指针；未找到返回 NULL。
 */
sr_wg_key_entry_t *
sr_wg_key_lookup (sr_wg_main_t *sm, u16 hmac_id)
{
  u32 i;

  for (i = 0; i < sm->key_count; i++)
    {
      if (sm->keys[i].hmac_id == hmac_id && sm->keys[i].active)
        return &sm->keys[i];
    }
  return NULL;
}

/*
 * 添加/更新密钥。若 hmac_id 已存在则覆盖，否则追加。
 * 返回 0 成功，-1 失败（参数非法或表已满）。
 */
int
sr_wg_key_add (sr_wg_main_t *sm, u16 hmac_id, const u8 *key)
{
  u32 i;

  if (!sm || !key)
    return -1;

  /* 检查是否已存在 */
  for (i = 0; i < sm->key_count; i++)
    {
      if (sm->keys[i].hmac_id == hmac_id)
        {
          clib_memcpy_fast (sm->keys[i].key, key, SR_PKEY_LEN);
          sm->keys[i].active = 1;
          /*
           * 路径表里存的是密钥**副本**（热路径不再查表）。轮换密钥时必须把
           * 新值刷进所有引用该 hmac_id 的路径，否则 `key set` 之后旧路径
           * 仍按旧密钥签名，对端验签失败 —— 现象就是"一换密钥隧道就断"。
           */
          sr_path_replace_pkey (&sm->path_table, hmac_id, key);
          return 0;
        }
    }

  /* 追加新条目 */
  if (sm->key_count >= SR_WG_MAX_KEYS)
    return -1;

  sm->keys[sm->key_count].hmac_id = hmac_id;
  clib_memcpy_fast (sm->keys[sm->key_count].key, key, SR_PKEY_LEN);
  sm->keys[sm->key_count].active = 1;
  sm->key_count++;
  sr_path_replace_pkey (&sm->path_table, hmac_id, key);
  return 0;
}

/*
 * 删除密钥。将末尾条目搬到被删位置，保持紧凑。
 * 返回 0 成功，-1 未找到。
 */
int
sr_wg_key_del (sr_wg_main_t *sm, u16 hmac_id)
{
  u32 i;

  if (!sm)
    return -1;

  for (i = 0; i < sm->key_count; i++)
    {
      if (sm->keys[i].hmac_id == hmac_id)
        {
          /* 清零敏感数据 */
          clib_memset (&sm->keys[i], 0, sizeof (sr_wg_key_entry_t));

          /* 紧凑：末尾条目搬到此处 */
          if (i != sm->key_count - 1)
            sm->keys[i] = sm->keys[sm->key_count - 1];

          clib_memset (&sm->keys[sm->key_count - 1], 0,
                       sizeof (sr_wg_key_entry_t));
          sm->key_count--;
          return 0;
        }
    }
  return -1;
}
