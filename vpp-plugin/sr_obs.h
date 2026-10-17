/*
 * sr_obs.h - 外层源观测表（vBOND 地址发现的数据面依据）
 *
 * 定案：NAT 把隧道 socket 改成了什么，只有**收到包的那一侧**知道。所以本表
 * 只做一件事：记下每个到达本节点的包的外层源 (ip:port)、它敲的是本机哪个
 * 端口、以及被看到的时间与次数。采集点有三处（都是"我们接受了这个包"）：
 *   - sr-wg4-input 解封装成功的 SR 包   -> dst_port = vpn_port，verified=1
 *   - sr-wg4-input 旁路的普通 WG 包     -> dst_port = vpn_port，verified=0
 *   - sr-wg4-transit 转发成功的 SR 包   -> dst_port = transit_port，verified=1
 * agent 读出来回报控制器，控制器据此登记该节点的公网映射 —— 探针就是
 * WireGuard 自己的 keepalive，不再另开一个 socket 去量，因为另一个 socket
 * 的映射是另一件事。
 *
 * 结构选择：直接映射小表（SR_OBS_SLOTS 槽），热路径 = 一次哈希 + 一次比较 +
 * 覆盖写。理由是本节点同时面对的 peer 数量级是几十，不是几百万；要的是
 * "O(1)、不分配内存、不引入锁"，而不是精确的 LRU。槽冲突时**新条目赢**，
 * 被淘汰的计数记在 evictions：观测是重复发生的（keepalive 每几秒一轮），
 * 丢掉一次下一秒还会再来，所以宁可丢也不让表变成无界内存。
 *
 * 可信度必须分档（entry->verified），这是本表最要紧的一条语义：
 *   观测点在 sr-wg4-input，位于 **WireGuard 解密之前**，所以外层源地址
 *   本身是不可信的 —— 任何主机都能伪造一个源 IP 往我们的 vpn_port 发包。
 *   - SR 包过了 HMAC(p-key) 校验 => 发送者确实持有 p-key，且头部声明的
 *     路径没被篡改，记 verified=1。**认证的是 SR 头，不是地址**：
 *     sr_hmac.c 的覆盖范围是 ver_flags..segments，外层 IP 头不在签名里，
 *     链路上任何一个 NAT 都会把地址改掉（本实验的 docker 网桥实测会改）。
 *     所以 verified=1 的正确读法是「一个持有 p-key 的对端，此刻正用这个
 *     (ip:port) 发包」——它把地址和成员资格绑在一起，但地址是不是那台 CPE
 *     自己的，仍要控制器用已知元组集合做归属判定；
 *   - 旁路的普通 WG 包（handshake-init / keepalive）没有任何本地可验证的
 *     凭据（initiator 的 static key 在密文里），只能记 verified=0，
 *     控制器必须拿它当**候选**：要么由该节点自己的上报交叉确认，要么由
 *     "用它建隧道后握手真的成功"来追认。
 * 入网阶段（还没有 SR 路径可签）只能依赖 verified=0，这是先有鸡还是先有蛋
 * 的代价，不是可以省略的步骤。
 *
 * 纯 C（stdint），单元测试直接编译本文件，不依赖 VPP 头。
 */
#ifndef SR_OBS_H
#define SR_OBS_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define SR_OBS_SLOTS 64

typedef struct {
    uint32_t src_ip;      /* 观测到的外层源 IPv4，主机序 */
    uint16_t src_port;    /* 主机序 */
    uint16_t dst_port;    /* 本机被观测到的目的端口 = 本节点的 vpn_port */
    uint16_t rx_if;       /* 收包接口 sw_if_index（低 16 位足够，观测是本地事实） */
    uint64_t first_seen;  /* 秒（VPP 单调时钟，由调用方传入，便于单测） */
    uint64_t last_seen;
    uint32_t hits;
    uint8_t  valid;
    uint8_t  verified;    /* 1 = 至少有一次观测来自 SR HMAC 校验通过的包（见文件头） */
    uint8_t  pad[3];
} sr_obs_entry_t;

typedef struct {
    sr_obs_entry_t slots[SR_OBS_SLOTS];
    uint32_t count;        /* 当前有效条目数 */
    uint64_t records;      /* 记录过的包数（含命中已有槽位） */
    uint64_t evictions;    /* 冲突挤掉旧条目的次数 */
} sr_obs_table_t;

/* 槽位选择：ip 与两个端口混合。纯函数，单独可测。 */
uint32_t sr_obs_slot(uint32_t src_ip, uint16_t src_port, uint16_t dst_port);

/*
 * 记录一次观测。
 *   now_sec  : 调用方的秒级时钟（数据面传 vlib_time_now 的整数部分）
 *   verified : 1 = 本次观测来自 SR HMAC 校验通过的包；0 = 旁路包（不可信，
 *              只能当候选映射）。同一元组先 0 后 1 会把 verified 升到 1，
 *              且**不会降回**（认证过这件事不因后来的旁路包而失效）。
 */
void sr_obs_record(sr_obs_table_t *t, uint32_t src_ip, uint16_t src_port,
                   uint16_t dst_port, uint16_t rx_if, uint64_t now_sec,
                   uint8_t verified);

/* 清空（保留计数历史无意义，计数也一起归零：clear 就是"从这里开始重新观测"）。 */
void sr_obs_clear(sr_obs_table_t *t);

/*
 * 遍历有效条目：cursor 初始为 0，每次返回下一条并推进 cursor；无更多条目返回 0。
 * out 为 NULL 时只判断有没有下一条。
 */
int sr_obs_next(const sr_obs_table_t *t, uint32_t *cursor, sr_obs_entry_t *out);

#ifdef __cplusplus
}
#endif

#endif /* SR_OBS_H */
