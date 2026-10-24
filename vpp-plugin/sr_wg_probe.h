/*
 * sr_wg_probe.h - 隧道内轻量质量探针（方案 C 的 "B 精简版"）
 *
 * 可达性由 VPP core BFD 负责（不进本插件）；本文件只负责 BFD 给不了的三项
 * 指标：**延迟 / 抖动 / 丢包**。定案（2026-10-24）：
 *   - 只发**一种**包（request，对端原样折返成 reply），不做双向 STAMP，
 *     不携带发送/接收/回程三段时延的完整分解；
 *   - 默认 300ms 一次，控制器可按隧道调整；
 *   - 与 BFD 两套会话表，由 CLI / 北向合成一张视图。
 *
 * 为什么不用 ping 插件：`ping` 的 API 事件只有计数（vl_api_ping_finished_event_t
 * 不含 RTT），CLI 里有时间但拿不到；也不用量 BFD 的低频探测：
 * bfd_session_t 全结构没有 rtt/jitter/loss。所以只剩"自己发一种包"这一条路。
 *
 * 包长固定 76 字节（20 IP + 8 UDP + 48 探针头），走**内层**正常路由：
 *   sr-wg-probe-process -> ip4-lookup -> wgX -> wg4-output -> [sr-wg4-encap]
 *   -> ... -> sr-wg4-input -> wg4-input -> ip4-local -> ip4-udp-lookup
 *   -> sr-wg4-probe（对端）
 * 对端把 IP/UDP 源和目的对调、盖 t2/t3 后交回 ip4-lookup，回程同样受隧道路由
 * 与 SR 封装约束 —— 所以探针量到的就是**真实业务路径**，不是裸 UDP 回声。
 *
 * 时间戳语义（四段）：
 *   t1 发送方打点，t2 对端收到时打点，t3 对端折返时打点，t4 发送方收到回包打点。
 *   两端时钟**可以不同**：RTT = (t4 - t1) - (t3 - t2)，两个减法的操作数各自
 *   来自同一台机器的时钟，跨机时钟差被消掉。不要 dump t1/t2 的绝对值做比较。
 */
#ifndef SR_WG_PROBE_H
#define SR_WG_PROBE_H

#include <vlib/vlib.h>
#include <vnet/vnet.h>
#include <vnet/ip/ip4_packet.h>
#include <vppinfra/hash.h>
#include <vppinfra/pool.h>

/*
 * 探针的 UDP 目的端口。选 51829 是因为它紧挨着实验室默认的 WG/transit 端口
 * （51820/51821），不会和 51820+10k 的多隧道端口段撞车。
 * 与 WG/transit 端口不同，这个端口**不需要**每隧道一份：探针的内层目的地址
 * 是各隧道的 overlay /32，隧道身份已经由内层 IP 表达，端口只标识"这是探针"。
 */
#define SR_PROBE_PORT 51829

#define SR_PROBE_MAGIC 0x53525042 /* 'SRPB'，网络序 */
#define SR_PROBE_VERSION 1
#define SR_PROBE_FLAG_REPLY 0x01

/* 20 (IP4) + 8 (UDP) + 48 (探针头) —— 无 IPv4 选项 */
#define SR_PROBE_HDR_LEN 48
#define SR_PROBE_PKT_LEN (20 + 8 + SR_PROBE_HDR_LEN)

#define SR_PROBE_DEFAULT_INTERVAL_MS 300
#define SR_PROBE_TIMEOUT_MULT 3
/* 下限用于防止误配把探针变成打流；上限由 CLI/API 校验（60s） */
#define SR_PROBE_MIN_INTERVAL_MS 10
#define SR_PROBE_MAX_INTERVAL_MS 60000

/*
 * 丢包率滑动窗口大小。300ms 一轮时 128 个槽位 ≈ 38 秒窗口：
 * 足够让"偶尔丢一个"体现成非零、又不至于一次断链后永远背着历史包袱。
 */
#define SR_PROBE_WINDOW 128

typedef struct
{
  u32 magic;        /* 网络序 'SRPB' */
  u8 version;       /* SR_PROBE_VERSION */
  u8 flags;         /* SR_PROBE_FLAG_REPLY */
  /*
   * 发送方**本机**隧道接口的 sw_if_index（主机序）。只在发送方那台机器上
   * 有意义：回包回来时用它找回是哪条会话，对端原样保留、不解释。
   */
  u16 tx_sw_if_index;
  u32 seq;          /* 主机序；每会话独立递增 */
  u32 reserved;
  u64 t1_ns;        /* 发送方发时刻 */
  u64 t2_ns;        /* 对端收到时刻（request 里为 0） */
  u64 t3_ns;        /* 对端折返时刻（request 里为 0） */
  u64 reserved2;
} sr_probe_hdr_t;

/* 编译期钉住线上格式：任何字段增删都必须显式改 SR_PROBE_HDR_LEN */
typedef char sr_probe_hdr_size_check[(sizeof (sr_probe_hdr_t) ==
                                      SR_PROBE_HDR_LEN) ? 1 : -1];

typedef enum
{
  SR_PROBE_SLOT_FREE = 0,
  SR_PROBE_SLOT_OUTSTANDING,
  SR_PROBE_SLOT_REPLIED,
  SR_PROBE_SLOT_LOST,
} sr_probe_slot_state_t;

typedef struct
{
  u32 seq;
  f64 sent_at; /* vlib_time_now 秒（相对 VPP 启动） */
  u8 state;
} sr_probe_slot_t;

typedef struct
{
  u32 sw_if_index;        /* 本地隧道接口 */
  u32 peer_ip;            /* 内层目的 overlay 地址（主机序） */
  u32 local_ip;           /* 内层源 overlay 地址（主机序），0 时取自接口地址 */
  u32 interval_ms;
  u32 timeout_ms;
  u8 enabled;

  u32 seq;
  f64 next_send;          /* 下一次发送的 vlib 时间 */

  /* 累计计数 */
  u64 tx;
  u64 rx;
  u64 lost;
  u64 late_replies;       /* 迟到（已判丢）或窗口外的回包 */

  /* RTT / 抖动（毫秒） */
  f64 last_rtt_ms;
  f64 prev_rtt_ms;
  f64 min_rtt_ms;
  f64 max_rtt_ms;
  f64 sum_rtt_ms;
  u32 n_rtt;
  f64 jitter_ms;          /* RFC3550 式 EWMA：j += (|d| - j) / 16 */

  /* 滑动窗口（丢包率用），win_head 指向下一个要写的槽 */
  sr_probe_slot_t window[SR_PROBE_WINDOW];
  u32 win_head;
} sr_probe_session_t;

typedef struct
{
  sr_probe_session_t *sessions; /* pool */
  uword *session_by_sw_if;      /* sw_if_index -> pool index */

  u32 node_index;          /* sr-wg4-probe */
  u32 process_node_index;  /* sr-wg-probe-process */

  /* 模块级计数：回包数/分配失败/不认识的会话，方便判断探针本身是否健康 */
  u64 echoed;              /* 本机折返了别人多少个 request */
  u64 unknown_replies;     /* 回包找不到会话（接口删了/会话刚删） */
  u64 alloc_fail;          /* 缓冲区分配失败（不应出现，出现即资源问题） */
  u32 count;               /* 会话数（含 disabled） */
} sr_probe_main_t;

/* 初始化会话表（幂等）。 */
void sr_probe_init (sr_probe_main_t *pm);

/*
 * 建/改/删一条会话。
 *   is_add=1：sw_if_index 必须存在；local_ip=0 时取该接口的第一个 IPv4 地址；
 *             interval_ms=0 用 300，timeout_ms=0 用 3×interval。
 *   is_add=0：删会话（不存在时幂等返回 0）。
 * 返回 0 或 VNET_API_ERROR_*。
 */
int sr_probe_set (sr_probe_main_t *pm, u32 sw_if_index, u32 peer_ip,
                  u32 local_ip, u32 interval_ms, u32 timeout_ms, int is_add);

/* 按 sw_if_index 查会话；没有返回 NULL。 */
sr_probe_session_t *sr_probe_get (sr_probe_main_t *pm, u32 sw_if_index);

/* 会话数（CLI/API 头行用）。 */
u32 sr_probe_count (sr_probe_main_t *pm);

/* 窗口内丢包率（千分比）；窗口里还没有完成样本时返回 0。 */
u32 sr_probe_loss_permille (const sr_probe_session_t *s);

/* 立即发一个探针（CLI 手工触发 / e2e 用）。返回 1 已发出，0 无此会话。 */
int sr_probe_send_now (sr_probe_main_t *pm, vlib_main_t *vm,
                       u32 sw_if_index);

#endif /* SR_WG_PROBE_H */
