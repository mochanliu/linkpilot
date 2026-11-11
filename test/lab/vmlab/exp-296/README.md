# exp-296 — 监控第 7 项（层 1）：每隧道字节/包计数 + VPP stats 段客户端

日期：2026-11-11　分支：feat/vbond-control-plane

## 做了什么

VPP 的二进制 API（api.sock）**没有**每接口计数器的 dump（26.06 的
interface.api 只有 `sw_interface_clear_stats`）。每隧道字节数的正路是 stats
共享内存段（/run/vpp/stats.sock）。本实验：

1. `internal/vppapi/statseg.go`：零依赖 stats 段只读客户端
   （govpp statsclient 的最小版）。
2. wire.TunnelMetrics 加 has_counters / counter_sec / rx/tx packets+bytes；
   agent 遥测窗口算增量（复用 counterDelta，语义与探针计数一致）。
3. 控制器 tunnel.sample 落盘带流量；监控页隧道表加「流量 ↓/↑」列，
   24h 趋势加第三张图（bps）。

## stats 段协议（全部在 CPE1 / VPP 26.06 实测钉死）

- 连接 **unixpacket** → VPP accept 后 `sendmsg(零长数据, 1 个 memfd)` 然后
  **立即 close**（src/vlib/stats/init.c `stats_socket_accept_ready`；
  listener 带 `CLIB_SOCKET_F_PASSCRED`）。
- 段头 v2：version u64@0、base u64@8、epoch@16、in_progress@24、
  dir_vector 指针@32。共享内存里存 VPP 绝对指针，客户端地址 =
  mmap 基址 + (存值 − header.base)。
- 目录：向量长度 u32 在数据前 8 字节；每条 144 字节：type i32@0、
  union u64@8、name[128]@16。/if/names=type 4（名字向量）、
  /if/rx、/if/tx=type 3（combined）。
- combined：数据区是"每线程一个 u64 指针"；每线程向量元素 16 字节
  = {packets u64, bytes u64}，下标 = sw_if_index，跨线程求和。
- 合成段解析有单测（statseg_test.go）；socket+fd 全链路有两个替身服务端
  测试（普通文件 fd、SO_PASSCRED+立即 close 两种）。

## 两个新坑（都实测过）

1. **Go 的 ReadMsgUnix 会把"最后一条消息"和 io.EOF 一起返回**。VPP 发完
   fd 立即 close，Go 客户端拿到 `oobn=24 且 err=EOF`——把 err 当致命错
   直接 return，就把已经到手的 fd 扔了。表现是"stats 段没递 fd: EOF"。
   修法：先解控制消息，fd 到手就赢。（python 的 recvmsg 没这个表现，所以
   python 探针一路绿灯、Go 一上线就翻车。）
2. Python `struct.unpack_from('<iQ', m, e)` 是**无对齐**打包：i 在 0、Q 在
   4（不是 8）！第一版探针把 union 读歪了还不报错。对齐布局要么手写偏移，
   要么逐字段 unpack。

另：Go `syscall.Sendmsg` 空载荷+fd 会垫一个哑字节（接收端会看到 1 B 数据），
所以接收侧别拿"数据体非空"当坏事。

## 部署与验证

- sr-agent linux `4491dd6dce2250a7` 四台全换（客户可见最长 4.29s）；
  sr-ctrl darwin `b417525d351ec0aa`。
- 全部 10 条隧道 has_counters=true，窗口 10s，空闲底噪 ↓0.7/↑1.3 KB/s
  （探针 1pps + BFD，合理）。
- cust12 ping 172.16.4.1 期间 node-2 的 sw=6 窗口 ↓7.1→8.7 KB，
  计数确实在数流量。
- 台账 tunnel.sample 带 has_counters/counter_sec/rx_bytes/tx_bytes，
  /api/history 能取到（页面的 bps 趋势线数据源）。
- 首窗报累计（counter_sec=0）；某窗没读到计数（stats 段抖动）该窗
  has_counters=false、下一窗重新从累计起步——丢失那段不编增量。

## 口径说明

- 计数是 **overlay 口径**：WG 接口的计数 = 解封装后/封装前的内层包，
  不含 WG/SR/UDP 隧道开销。这是"客户流量"的口径。
- VPP 重启 → agent 换新 Client → stats 段重新 mmap，计数从零起；
  窗口算法对归零免疫（counterDelta 语义同探针）。
