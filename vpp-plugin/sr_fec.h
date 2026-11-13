/*
 * sr_fec.h - 隧道内前向纠错（FEC）：XOR 块奇偶校验
 *
 * ==================== 方案（owner 2026-11-13 拍板，docs/fec-and-lb-plan.md §9） ====================
 *
 * 比例 1:n = 每 n 个数据包 1 个异或冗余包（n = 1..4）。n=1 时退化为整包复制
 * （对端 WG 反回放天然去重）；n≥2 时冗余包 = 成员 WG 帧的逐字节异或。
 * 组内恰好丢 1 个可恢复，丢 2 个无解。不引第三方库（无 GF(256)）。
 *
 * ==================== 关键设计决定（§9.2-9.4 已拍） ====================
 *
 * 1) 编码点在**加密之后**（encap 尾部）：异或的对象是 WG 帧（WG transport 头 +
 *    密文）。还原帧带原 nonce，对端 wg4-input 可直接解密。明文异或会破坏 WG
 *    nonce 唯一性；含 SR 头的全载荷异或会被中转的 seg_left 改写破坏 —— 两者都
 *    不行，所以异或窗口 = UDP 载荷里 SR 头之后的部分。
 *
 * 2) 冗余包标识 = **payload 首字节类型位 0xF0**（SR_MAGIC=0x53、WG 类型 1..4
 *    都撞不上它）。冗余包复制成员的外层头（IP4/UDP/SR 头逐字拷贝）：
 *    HMAC 校验天然通过（只改 action 类别位 —— HMAC 排除字段，免重签），
 *    **中转跳照常转发**，两跳隧道的 FEC 免费成立。对端 sr-wg4-input 剥完
 *    SR 头后按首字节分流进解码。
 *
 * 3) 数据包**不打标、不缓冲、不延迟**：数据包照常直通，只顺带把 WG 帧异或进
 *    发送组缓冲；第 n 个包发出后立即补一个冗余包。没有定时器 —— 流量停在
 *    半组时那段就没有冗余，数据永远不等 FEC。
 *
 * 4) 接收侧配对 = 计数法：对端在两次冗余包之间数收到的 WG 帧数 j，
 *    j == n-1 → 恰好丢 1，异或还原；j == n → 全到；j < n-1 → 多丢，放弃。
 *    还原不需要知道丢的是第几个成员：冗余包头里的 len_xor（成员长度异或）
 *    自反出缺失帧的长度（异或自反性）。配对错位（乱序/半组跨周期）最多让
 *    还原出垃圾帧 —— wg4-input 的 WG 标签校验会把它丢掉，**绝不产生脏数据**。
 *
 * ==================== 限制（v1 如实记录） ====================
 *
 * - 冗余包外层比最大成员长 fec 头的 10 字节，内层 MTU 贴着隧道上限时冗余包
 *   可能超 MTU 被丢（内层包 ≤ TunnelMTU - 64 时无虞）。
 * - 乱序链路上计数配对会错位：单路径 UDP 流内乱序罕见，v1 接受降级。
 * - 还原帧的 QoS 类别落 BE（action 类别位没有随异或传递）：只影响"丢了又被
 *   救回的那一包"，不值得为它扩头部。
 * - 数据面状态假定单线程访问（VPP main 线程；lab 部署无 worker）。
 */
#ifndef SR_FEC_H
#define SR_FEC_H

#include "sr_wg.h"

#ifdef __cplusplus
extern "C" {
#endif

/* ---------- 线格式常量 ---------- */

/* 冗余包 payload 首字节。SR_MAGIC=0x53、WG 消息类型 1..4、普通 IPv4 版本
 * nibble(4..7) 都撞不上它；0xF0 落在 RFC 6890 保留段。 */
#define SR_FEC_MAGIC          0xF0

/* 冗余包头：magic(1) k(1) seq(4) len_xor(2) pad(2)，全网络序 */
#define SR_FEC_HDR_LEN        10

/* 异或成员数上限 = 下拉的四档比例的最大值。 */
#define SR_FEC_MAX_MEMBERS    4

/* WG 帧长度上限：外层 1500 - IP4(20) - UDP(8) - 最小 SR 头(30)。 */
#define SR_FEC_MAX_FRAME      1442

/* ---------- 每隧道状态 ---------- */

typedef struct
{
  u32 sw_if_index;              /* 所属隧道接口（show / 生命周期对齐用） */
  u8  ratio;                    /* 每 ratio 个数据包 1 个冗余包（1..4） */

  /* ---- 发送侧：进行中的异或组 ---- */
  u32 tx_seq;                   /* 下一组冗余包的序号（单调 +1） */
  u8  tx_n;                     /* 已异或进本组的成员数 */
  u16 tx_len[SR_FEC_MAX_MEMBERS];
  u16 tx_pad;                   /* 本组最大成员长度 = 冗余体长度 */
  u8  tx_acc[SR_FEC_MAX_FRAME]; /* 成员帧的滚动异或 */

  /* ---- 接收侧：配对窗口 ---- */
  u32 rx_last_seq;              /* 最后一个冗余包的 seq（去陈旧） */
  u8  rx_j;                     /* 自上个冗余包以来收到的 WG 帧数 */
  u16 rx_len[SR_FEC_MAX_MEMBERS];
  u8  rx_ring[SR_FEC_MAX_MEMBERS][SR_FEC_MAX_FRAME];

  /* ---- 计数（show sr-wg fec） ---- */
  u64 parity_sent;              /* 冗余包发出数（含 ratio=1 的整包复制） */
  u64 parity_bytes;             /* 冗余包外层字节数（带宽账） */
  u64 recovered;                /* 还原成功帧数 */
  u64 complete;                 /* 冗余包到时组内一个不缺（j==k）的次数 */
  u64 unrecovered;              /* 组内多丢（j < k-1）放弃的次数 */
  u64 bad_parity;               /* 头不合法/陈旧/无状态而丢弃的冗余包数 */
  u64 too_big;                  /* 成员帧超 SR_FEC_MAX_FRAME 未入组的次数 */
} sr_fec_state_t;

/* ---------- 管理面 ---------- */

/*
 * 设置隧道接口的 FEC 比例（0 = 关，1..4 = 每 ratio 个数据包 1 个冗余包）。
 * 按隧道接口 sw_if_index 定位绑定；没有绑定的接口报 NO_SUCH_ENTRY。
 * 重复设置同值幂等；ratio=0 时回收状态（半组直接丢弃 —— 数据不等 FEC）。
 * 返回 0 成功，非 0 为 VNET_API_ERROR_*。
 */
int sr_fec_set (sr_wg_main_t *sm, u32 sw_if_index, u8 ratio);

/* 当前开 FEC 的绑定数（数据面热路径的总开关判据）。 */
u32 sr_fec_active_count (sr_wg_main_t *sm);

/* ---------- 数据面（由 encap / input 调用） ---------- */

/*
 * 发送侧：把刚封装完的数据包异或进所属隧道 FEC 组；组满（第 ratio 个成员）
 * 时新造一个冗余包 buffer 并返回其 index（~0 = 本包不需要发冗余包）。
 *
 *   pkt      = 数据包 L3 起点（[IP4|UDP|SR|WG帧...]）
 *   outer_len = ip4_len + 8
 *   sr_len   = SR 头长度（异或窗口从 pkt+outer_len+sr_len 开始）
 *   wg_len   = WG 帧字节数（调用方从 udp->length 算好传入）
 *   fib_index = 冗余包重查表用的 FIB（与成员包 Step 14 写入的一致）
 *
 * 冗余包：复制成员的 [IP4|UDP|SR] 头（IP ToS 清零、SR action 类别位清零，
 * 两者都免重签），追加 fec 头 + 异或体；VLIB_TX/fib_index 与成员一致，
 * 交回 ip4-lookup 走与数据包相同的出口路径。
 * 数据包本身**只读**：任何失败都不影响它的封装与转发（fail-open）。
 */
u32 sr_fec_tx (vlib_main_t *vm, sr_wg_main_t *sm, sr_wg_tunnel_bind_t *bind,
	       vlib_buffer_t *member, u8 *pkt, u32 outer_len, u16 sr_len,
	       u16 wg_len, u32 fib_index);

/*
 * 接收侧：数据包解封装成功后（current 指向 WG 帧）把帧拷进配对环。
 * 只在计数器窗口未满时拷贝；环满（冗余包丢了多组）时跳过 —— 恢复不了，
 * 但绝不能拖累数据包。
 */
void sr_fec_rx_data (sr_wg_tunnel_bind_t *bind, vlib_buffer_t *b);

/*
 * 接收侧：冗余包（SR 头已被 input 剥掉，current 指向 fec 载荷）。
 * 返回 0 = buffer 已原地变换成还原出的 WG 帧（current 指向帧首、长度已修、
 * 外层长度/校验和已修），调用方照常送 wg4-input；
 * 返回 -1 = 丢弃（组全到/多丢/头不合法/陈旧），buffer 保持 fec 载荷原样。
 */
int sr_fec_rx_parity (sr_wg_main_t *sm, sr_wg_tunnel_bind_t *bind,
		      vlib_buffer_t *b, ip4_header_t *outer,
		      udp_header_t *udp);

#ifdef __cplusplus
}
#endif

#endif /* SR_FEC_H */
