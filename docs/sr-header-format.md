# SR-WG 源路由路径头格式规范 v0.1

## 概述

SR（Source Routing）路径头插入在 UDP Header 和 WireGuard 加密数据之间，
为数据包提供显式路径信息。路径头是明文的，通过 SipHash-2-4（128-bit 密钥）
保护完整性。线上字段与代码接口仍沿用 `hmac` 命名。

## 包结构

```
+----------+----------+-----------+-------------------+
| Outer IP | Outer UDP| SR Header | WG Data Message   |
| (20B)    | (8B)     | (可变长)  | (加密的payload)   |
+----------+----------+-----------+-------------------+
```

## SR Header 格式

### 固定部分（8 字节）

| 偏移 | 长度 | 字段名 | 说明 |
|------|------|--------|------|
| 0 | 1B | magic | 固定 0x53 ('S') |
| 1 | 1B | ver_flags | 高4位=版本(0x1)，低4位=标志位 |
| 2 | 1B | hdr_len | SR Header 总长度（字节） |
| 3 | 1B | seg_left | 剩余待处理段数（每跳递减） |
| 4 | 1B | seg_count | 路径总段数 |
| 5 | 1B | action | 动作码（预留，0x00=forward） |
| 6 | 2B | reserved | 保留，置零 |

### 可扩展部分（8 字节）

| 偏移 | 长度 | 字段名 | 说明 |
|------|------|--------|------|
| 8 | 2B | hmac_id | HMAC 密钥标识 |
| 10 | 2B | path_id | 路径标识 |
| 12 | 4B | timestamp | 发送时间戳（秒级） |

### 段列表（seg_count * 6 字节）

| 偏移 | 长度 | 字段名 | 说明 |
|------|------|--------|------|
| 16 | 4B | ipv4_addr | 段节点 IPv4 地址（网络序） |
| 20 | 2B | udp_port | 段节点 UDP 端口（网络序） |

每段 6 字节，共 seg_count 段。

### hmac 字段（8 字节）

| 偏移 | 长度 | 字段名 | 说明 |
|------|------|--------|------|
| 16+N*6 | 8B | hmac | SipHash-2-4 的 64 位 tag，大端（网络序）写入 |

### 长度计算公式

```
hdr_len = 16 + seg_count * 6 + 8 = 24 + seg_count * 6
```

## 签名覆盖范围（hmac 字段）

包含：ver_flags, hdr_len, seg_count, reserved, hmac_id, path_id, timestamp, segments[0..N-1]
排除：magic（固定值）, seg_left（每跳变）, action（每跳可能变）

上述字段按线格式顺序拼成连续字节缓冲（最长 13 + 8*6 = 61 字节）作为
SipHash-2-4 的输入，密钥为 hmac_id 对应的 128-bit P-Key。

## 密钥模型与轮换

- encap 签名读的是**路径配置里的 P-Key 副本**（热路径不回查密钥表）。
- `sr-wg key set <hmac_id> <hex32>` 会同时刷新所有引用该 hmac_id 的路径副本；
  轮换密钥只用 key set，**不要** del+set（del 会被拒绝，见下）。
- 密钥仍被路径引用时不允许删除：CLI 与 API 都会拒绝，避免「表里没密钥、
  路径里还留着旧副本」这种控制面看不见的状态。

## 段列表语义

- segments[0] = 最终目的节点（locator:vpn_port）
- segments[seg_count-1] = 外层 UDP dst 之后的第一个中转
- 外层 UDP dst = segments[seg_count-1] 的 IP:Port（首跳免费）
- 中转处理：取 segments[seg_left-1] 作为**新外层目的** IP:Port，seg_left--；
  外层**源** IP 与源端口一律不动（中转不参与 WireGuard，它不解密），
  TTL 递减交给 ip4-lookup

## UDP 端口分配

| 端口 | 用途 | 行为 |
|------|------|------|
| 51820 | VPN 隧道 | Encap/Decap |
| 51821 | 中转 | Transit（只改外层**目的** IP/端口，绝不动外层源，不解密） |
| 51822+ | 预留 | 未来 service-chain 等 |
