/*
 * sr_wg4_transit.c - SR-WG 中转 Graph Node
 *
 * 处理流程：
 *   udp4-local dispatch -> sr_wg4_transit (current_data 指向 SR Header)
 *   验证 SR Header -> HMAC 校验 -> seg_left-- -> 改写外层目的 IP/UDP dst_port
 *   -> ip4-lookup
 *
 * 中转只动「目的」，绝不动外层源 IP/端口：中转不参与 WireGuard（不解密），
 * 而目的端 wg4-input 按 peer endpoint 校验外层源，改源会让对端丢包并把
 * peer 的动态 endpoint 污染到中转。
 */
#include "sr_wg.h"
#include "sr_qos.h"
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
  _ (DISABLED, "sr-wg transit is not enabled")                           \
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
  /* 每批取一次秒级时钟即可（观测粒度是秒，见 sr_obs.h）；相对 VPP 启动 */
  const u64 now_sec = (u64) vlib_time_now (vm);

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

      /*
       * Step 0: `set sr-wg transit disable` 必须真的停下转发。
       * 端口->节点的 UDP 注册在 init 时就做掉了，disable 只能靠这个开关
       * 生效；否则 disable 之后本机仍会继续替别人转发 SR 流量。
       */
      if (PREDICT_FALSE (sm->transit_enabled == 0))
        {
          error = SR_WG4_TRANSIT_ERROR_DISABLED;
          sm->counters.drop_total++;
          goto done;
        }

      /* Step 1: 验证 SR Header 基础字段 */
      if (sr_hdr_validate (srh, b->current_length) != 0)
        {
          /* 细分错误类型：计数器供控制器/e2e 断言，不能只落在 error 串上 */
          if (b->current_length < SR_FIXED_HDR_LEN || srh->magic != SR_MAGIC)
            {
              error = SR_WG4_TRANSIT_ERROR_BAD_MAGIC;
              sm->counters.transit_bad_magic++;
            }
          else if ((srh->ver_flags & SR_VER_MASK) != (SR_VERSION << 4))
            {
              error = SR_WG4_TRANSIT_ERROR_BAD_VER;
              sm->counters.transit_bad_ver++;
            }
          else
            {
              error = SR_WG4_TRANSIT_ERROR_BAD_LEN;
              sm->counters.transit_bad_len++;
            }
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

      /*
       * Step 7.5: 记录外层源观测（vBOND 地址发现）。
       *
       * 必须在 Step 8/9 改写外层 dst **之前**记：观测要留的是"别人敲进本机
       * 哪个端口"(transit_port)，改写之后就变成下一跳的端口了。
       * 走到这里的包已全部过了 HMAC，所以 verified=1 —— 认证的是「发送者持有
       * p-key、头部声明的路径没被篡改」，不是地址本身：外层 IP 头不在
       * sr_hmac.c 的覆盖范围里，链路上的 NAT 照样会改它（见 sr_obs.h）。
       *
       * 为什么中转也要观测而不是只在终点 CPE 记：hub 形态下中转往往是唯一有
       * 公网地址、也最适合当证人的一方；而中转与终点各自看到什么元组，取决于
       * 中间还有几层 NAT —— 两处都记，控制器才有交叉比对的素材（中转保留外层
       * 源，所以正常情况下两端看到的一致；不一致就说明中间有地址改写）。
       */
      sr_obs_record (&sm->obs, clib_net_to_host_u32 (ip4->src_address.as_u32),
                     clib_net_to_host_u16 (udp->src_port),
                     clib_net_to_host_u16 (udp->dst_port),
                     (u16) vnet_buffer (b)->sw_if_index[VLIB_RX], now_sec, 1);

      /* Step 8: 改写外层 IP4 dst = next_hop.ip */
      clib_memcpy_fast (&ip4->dst_address, next_hop->ip, 4);

      /* Step 9: 改写外层 UDP dst_port = next_hop.port */
      clib_memcpy_fast (&udp->dst_port, next_hop->port, 2);

      /*
       * Step 9.5: 从 action 高 3bit 恢复 QoS 类别（owner 2026-11-13：必须步骤）。
       *
       * 隧道包中途会过运营商设备，外层 DSCP 可能被重标记（限速/重分类/清零）。
       * action 在我们的 UDP 载荷里，运营商不动它，是端到端唯一的权威类别。
       * 所以本机出向调度**不信任收到的外层 DSCP**：
       *   ① 重写外层 ToS = 类别代表 DSCP（Step 14 会重算 checksum，顺序必须
       *      在它之前）—— 下一跳/本机出向物理口调度看到的 ToS 重新可信；
       *   ② 写 buffer 元数据 —— 本机出向 interface-output 调度优先读它。
       * 这让"中转出向物理口加 QoS 策略"成为可能：类别跟着 SR 头走完整条链。
       */
      {
        u8 cls = sr_qos_action_to_class (srh->action);

        ip4->tos = (u8) (sr_qos_dscp_of_class (cls) << 2);
        vnet_buffer2 (b)->qos.bits = cls;
        b->flags |= VNET_BUFFER_F_QOS_DATA_VALID;
      }

      /*
       * Step 10: **外层源（IP 和端口）一个字节都不动。**
       * 中转不解密、不参与 WireGuard，只做「验签 + 弹一段 + 改外层 dst」；
       * 但目的端的 wg4-input 会按 peer 配置的 endpoint 校验外层源，
       * 一旦把源改成中转自己的地址，对端看到的源五元组就和配置不一致：
       * 包被丢，同时 peer 的动态 endpoint 会被污染到中转，回程发错方向，
       * 隧道永远建不起来（本轮在 lab 里实测过这一类失败）。
       * 代价是已知的：若中间网络开了 uRPF/BCP38 入口过滤，头节点的 underlay
       * 源可能在外人设备上被丢 —— 那是网络前提问题，不在中转里偷偷改源解决。
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
