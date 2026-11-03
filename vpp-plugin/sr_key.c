/*
 * sr_key.c - P-Key 密钥管理
 *
 * O(1) 定位：sm->key_index[hmac_id] 给出槽位（值 = 槽位下标 + 1，0 = 不存在），
 * keys[] 保持紧凑（[0..key_count-1] 有效），删除用末位搬移并同步改写索引。
 * add/del/lookup 三个入口都不含循环。
 */
#include "sr_wg.h"

/*
 * 定位 hmac_id 对应的槽位。返回槽下标；未找到或索引与条目不一致返回 -1。
 *
 * 索引不是唯一事实来源：命中后必须回读 keys[slot].hmac_id 核对，这样即使索引
 * 与条目被写坏（改动 del/add 时漏改一处），表现也是"查不到这把钥匙"，
 * 不是"拿别人的密钥去验签"。
 */
static int
sr_wg_key_slot (sr_wg_main_t *sm, u16 hmac_id)
{
  u16 v;
  u32 slot;

  v = sm->key_index[hmac_id];
  if (v == 0)
    return -1;

  slot = (u32) (v - 1);
  if (slot >= sm->key_count)
    return -1;
  if (sm->keys[slot].hmac_id != hmac_id)
    return -1;

  return (int) slot;
}

/*
 * 查找 hmac_id 对应的密钥条目。
 * 返回条目指针；未找到（含已登记但 active=0）返回 NULL。
 *
 * 这是热路径：sr-wg4-transit 与 sr-wg4-input 每个收包都调一次。
 */
sr_wg_key_entry_t *
sr_wg_key_lookup (sr_wg_main_t *sm, u16 hmac_id)
{
  int slot;

  if (!sm)
    return NULL;

  slot = sr_wg_key_slot (sm, hmac_id);
  if (slot < 0)
    return NULL;
  if (!sm->keys[slot].active)
    return NULL;

  return &sm->keys[slot];
}

/*
 * 添加/更新密钥。若 hmac_id 已存在则覆盖，否则追加。
 * 返回 SR_WG_KEY_OK，参数非法 SR_WG_KEY_ERR_PARAM，
 * 表已满（且 hmac_id 是新的）SR_WG_KEY_ERR_FULL。
 */
int
sr_wg_key_add (sr_wg_main_t *sm, u16 hmac_id, const u8 *key)
{
  int slot;

  if (!sm || !key)
    return SR_WG_KEY_ERR_PARAM;

  slot = sr_wg_key_slot (sm, hmac_id);

  /* 已存在 → 原地覆盖（覆盖不受容量限制：不新增槽位） */
  if (slot >= 0)
    {
      clib_memcpy_fast (sm->keys[slot].key, key, SR_PKEY_LEN);
      sm->keys[slot].active = 1;
      /*
       * 路径表里存的是密钥**副本**（热路径不再查表）。轮换密钥时必须把
       * 新值刷进所有引用该 hmac_id 的路径，否则 `key set` 之后旧路径
       * 仍按旧密钥签名，对端验签失败 —— 现象就是"一换密钥隧道就断"。
       */
      sr_path_replace_pkey (&sm->path_table, hmac_id, key);
      return SR_WG_KEY_OK;
    }

  /* 追加新条目 */
  if (sm->key_count >= SR_WG_MAX_KEYS)
    return SR_WG_KEY_ERR_FULL;

  slot = (int) sm->key_count;
  sm->keys[slot].hmac_id = hmac_id;
  clib_memcpy_fast (sm->keys[slot].key, key, SR_PKEY_LEN);
  sm->keys[slot].active = 1;
  sm->key_index[hmac_id] = (u16) (slot + 1);
  sm->key_count++;
  sr_path_replace_pkey (&sm->path_table, hmac_id, key);
  return SR_WG_KEY_OK;
}

/*
 * 删除密钥。将末尾条目搬到被删位置，保持紧凑。
 * 返回 0 成功，-1 未找到。
 */
int
sr_wg_key_del (sr_wg_main_t *sm, u16 hmac_id)
{
  int slot;

  if (!sm)
    return -1;

  slot = sr_wg_key_slot (sm, hmac_id);
  if (slot < 0)
    return -1;

  sm->key_index[hmac_id] = 0;

  /* 清零敏感数据 */
  clib_memset (&sm->keys[slot], 0, sizeof (sr_wg_key_entry_t));

  /* 紧凑：末尾条目搬到此处的槽位，索引跟着改 */
  if ((u32) slot != sm->key_count - 1)
    {
      sm->keys[slot] = sm->keys[sm->key_count - 1];
      sm->key_index[sm->keys[slot].hmac_id] = (u16) (slot + 1);
    }

  clib_memset (&sm->keys[sm->key_count - 1], 0,
               sizeof (sr_wg_key_entry_t));
  sm->key_count--;
  return 0;
}
