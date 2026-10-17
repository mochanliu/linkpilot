/*
 * sr_wg4_transit.c - SR-WG 中转 Graph Node
 *
 * 处理流程：
 *   udp4-local dispatch -> sr_wg4_transit (current_data 指向 SR Header)
 *   验证 SR Header -> HMAC 校验 -> seg_left-- -> 改写外层 IP/UDP -> ip4-lookup
 */
#include "sr_wg.h"
#include <vnet/udp/udp_packet.h>

/* ==================== Trace 定义 ==================== */

typedef struct {
  u32 next_index;
  u32 sw_if_index;
  u16 hmac_id;
  u16 path_id;
  u8  seg_left;
  u8  seg_count;
  ip4_address_t next_hop_ip;
  u16 next_hop_port;
  u8  error;
} sr_wg4_transit_trace_t;

static u8 *
format_sr_wg4_transit_trace (u8 *s, va_list *args)
{
  CLIB_UNUSED (vlib_main_t * vm) = va_arg (*args, vlib_main_t *);
  CLIB_UNUSED (vlib_node_t * node) = va_arg (*args, vlib_node_t *);
  sr_wg4_transit_trace_t *t = va_arg (*args, sr_wg4_transit_trace_t *);

  s = format (s, "SR-WG-TRANSIT: hmac_id=%u path_id=%u seg_left=%u/%u "
                 "next_hop=%U:%u err=%u next=%u",
              t->hmac_id, t->path_id, t->seg_left, t->seg_count,
              format_ip4_address, &t->next_hop_ip, t->next_hop_port,
              t->error, t->next_index);
  return s;
}

/* ==================== 错误枚举 ==================== */

#define foreach_sr_wg4_transit_error                                     \
  _ (NONE, "no error")                                                   \
  _ (BAD_MAGIC, "bad magic byte")                                        \
  _ (BAD_VER, "unsupported version")                                     \
  _ (BAD_LEN, "header length mismatch")                                  \
  _ (HMAC_FAIL, "hmac verification failed")                              \
  _ (SEG_EXHAUSTED, "segments exhausted")                                \
  _ (NO_KEY, "no key found for hmac_id")

typedef enum {
#define _(sym, str) SR_WG4_TRANSIT_ERROR_##sym,
  foreach_sr_wg4_transit_error
#undef _
    SR_WG4_TRANSIT_N_ERROR,
} sr_wg4_transit_error_t;

static char *sr_wg4_transit_error_strings[] = {
#define _(sym, str) str,
  foreach_sr_wg4_transit_error
#undef _
};

/* ==================== Next nodes ==================== */

typedef enum {
  SR_WG4_TRANSIT_NEXT_DROP,
  SR_WG4_TRANSIT_NEXT_IP4_LOOKUP,
  SR_WG4_TRANSIT_N_NEXT,
} sr_wg4_transit_next_t;

/* ==================== Node 注册 ==================== */

VLIB_REGISTER_NODE (sr_wg4_transit_node) = {
  .name = "sr-wg4-transit",
  .vector_size = sizeof (u32),
  .format_trace = format_sr_wg4_transit_trace,
  .type = VLIB_NODE_TYPE_INTERNAL,
  /* 不置这个 flag 时 `trace add sr-wg4-transit N` 直接报不支持 */
  .flags = VLIB_NODE_FLAG_TRACE_SUPPORTED,
  .n_errors = SR_WG4_TRANSIT_N_ERROR,
  .error_strings = sr_wg4_transit_error_strings,
  .n_next_nodes = SR_WG4_TRANSIT_N_NEXT,
  .next_nodes = {
    [SR_WG4_TRANSIT_NEXT_DROP] = "error-drop",
    [SR_WG4_TRANSIT_NEXT_IP4_LOOKUP] = "ip4-lookup",
  },
};

/* ==================== Node Function ==================== */

VLIB_NODE_FN (sr_wg4_transit_node) (vlib_main_t *vm, vlib_node_runtime_t *node,
                                    vlib_frame_t *frame)
{
  u32 n_left = frame->n_vectors;
  u32 *from = vlib_frame_vector_args (frame);
  vlib_buffer_t *bufs[VLIB_FRAME_SIZE];
  u16 nexts[VLIB_FRAME_SIZE];
  /* bufs/nexts 是数组，不可自增；用指针游标遍历 */
  vlib_buffer_t **buf_ptr = bufs;
  u16 *next_ptr = nexts;
  sr_wg_main_t *sm = &sr_wg_main;

  vlib_get_buffers (vm, from, bufs, n_left);

  while (n_left > 0)
    {
      vlib_buffer_t *b = buf_ptr[0];
      sr_header_t *srh;
      sr_wg_key_entry_t *key_entry;
      ip4_header_t *ip4;
      udp_header_t *udp;
      sr_segment_t *next_hop = NULL;
      u16 hmac_id;
      u32 next_index = SR_WG4_TRANSIT_NEXT_DROP;
      u8 error = SR_WG4_TRANSIT_ERROR_NONE;

      /* current_data 指向 SR Header（udp4-local 已剥掉 eth/ip4/udp） */
      srh = (sr_header_t *) vlib_buffer_get_current (b);

      /* Step 1: 验证 SR Header 基础字段 */
      if (sr_hdr_validate (srh, b->current_length) != 0)
        {
          /* 细分错误类型 */
          if (b->current_length < SR_FIXED_HDR_LEN || srh->magic != SR_MAGIC)
            error = SR_WG4_TRANSIT_ERROR_BAD_MAGIC;
          else if ((srh->ver_flags & SR_VER_MASK) != (SR_VERSION << 4))
            error = SR_WG4_TRANSIT_ERROR_BAD_VER;
          else
            error = SR_WG4_TRANSIT_ERROR_BAD_LEN;
          sm->counters.drop_total++;
          goto done;
        }

      /* Step 2: 查找 P-Key */
      hmac_id = sr_hdr_get_hmac_id (srh);
      key_entry = sr_wg_key_lookup (sm, hmac_id);
      if (!key_entry)
        {
          error = SR_WG4_TRANSIT_ERROR_NO_KEY;
          sm->counters.drop_total++;
          goto done;
        }

      /* Step 3: HMAC 验证 */
      if (sr_hmac_verify (srh, key_entry->key) != 0)
        {
          error = SR_WG4_TRANSIT_ERROR_HMAC_FAIL;
          sm->counters.transit_hmac_fail++;
          sm->counters.drop_total++;
          goto done;
        }

      /* Step 4: 检查 seg_left */
      if (srh->seg_left == 0)
        {
          error = SR_WG4_TRANSIT_ERROR_SEG_EXHAUSTED;
          sm->counters.transit_seg_exhausted++;
          sm->counters.drop_total++;
          goto done;
        }

      /* Step 5: 获取下一跳段 */
      next_hop = sr_hdr_get_next_hop (srh);
      if (!next_hop)
        {
          error = SR_WG4_TRANSIT_ERROR_SEG_EXHAUSTED;
          sm->counters.transit_seg_exhausted++;
          sm->counters.drop_total++;
          goto done;
        }

      /* Step 6: seg_left-- (不在 HMAC 覆盖范围内，无需重签) */
      srh->seg_left--;

      /* Step 7: 回退到 IP4 头（外层） */
      /* udp4-local advance 了 ip4_hdr_len + udp_hdr_len(8) */
      /* 第一版假设标准 20 字节 IPv4 头（无选项） */
      vlib_buffer_advance (b, -(int) SR_OUTER_HDR_LEN);

      ip4 = (ip4_header_t *) vlib_buffer_get_current (b);
      udp = (udp_header_t *) ((u8 *) ip4 + 20);

      /* Step 8: 改写外层 IP4 dst = next_hop.ip */
      clib_memcpy_fast (&ip4->dst_address, next_hop->ip, 4);

      /* Step 9: 改写外层 UDP dst_port = next_hop.port */
      clib_memcpy_fast (&udp->dst_port, next_hop->port, 2);

      /* Step 10: 可选改写 src IP（如果本机地址已配置） */
      if (sm->local_ip4.as_u32 != 0)
        ip4->src_address = sm->local_ip4;

      /*
       * Step 11: **不改写** UDP src_port。
       * SR 的 function 区分靠的是外层 dst port，transit 只需要把 dst port
       * 换成下一跳段口的 51820/51821。若把 src port 也改成 51821，对端
       * WireGuard 看到的源五元组就变成 10.10.1.2:51821，与其配置的 peer
       * endpoint（10.10.1.2:51820）不一致：wg4-input 报 "Interface error"
       * 丢弃，同时把 peer 的动态 endpoint 污染成错误地址，回包发往错误的
       * 端口 —— 隧道永远建不起来。保持原 src port 才能让 SR 路径与 WG
       * 直连路径在对端看起来完全一样（回程也复用同一对端口）。
       */

      /* Step 12: 重算 UDP 长度（SR Header 长度不变，但为保险起见） */
      /* udp->length 已经正确（payload 未变） */

      /* Step 13: UDP checksum 置 0（WG 惯例） */
      udp->checksum = 0;

      /*
       * Step 13.5: **不动外层 TTL。**
       * TTL 递减与 ttl<=1 的丢弃/ICMP-time-exceeded 由下一跳的 ip4-lookup
       * 负责（实测：本节点再减一次会变成每跳减 2，traceroute 跳数翻倍，
       * 且 loop 已被 ip4-lookup 的减到 0 即丢兜住）。
       */

      /* Step 14: 重算 IP4 checksum */
      ip4->checksum = ip4_header_checksum (ip4);

      /* 成功计数 */
      sm->counters.transit_ok++;
      next_index = SR_WG4_TRANSIT_NEXT_IP4_LOOKUP;

    done:
      b->error = error ? node->errors[error] : 0;

      if (PREDICT_FALSE (b->flags & VLIB_BUFFER_IS_TRACED))
        {
          sr_wg4_transit_trace_t *tr =
            vlib_add_trace (vm, node, b, sizeof (*tr));
          tr->next_index = next_index;
          tr->sw_if_index = vnet_buffer (b)->sw_if_index[VLIB_RX];
          tr->error = error;
          if (srh && b->current_length >= SR_FIXED_HDR_LEN)
            {
              tr->hmac_id = sr_hdr_get_hmac_id (srh);
              tr->path_id = sr_hdr_get_path_id (srh);
              tr->seg_left = srh->seg_left;
              tr->seg_count = srh->seg_count;
            }
          if (next_hop)
            {
              clib_memcpy_fast (&tr->next_hop_ip, next_hop->ip, 4);
              u16 port_net;
              clib_memcpy_fast (&port_net, next_hop->port, 2);
              tr->next_hop_port = clib_net_to_host_u16 (port_net);
            }
        }

      next_ptr[0] = next_index;

      /* 移动到下一个 buffer */
      n_left--;
      buf_ptr++;
      next_ptr++;
    }

  /* 批量 enqueue */
  vlib_buffer_enqueue_to_next (vm, node, from, nexts, frame->n_vectors);
  return frame->n_vectors;
}
