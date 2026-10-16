# SR-WG 源路由路径头格式规范 v0.1

## 概述

SR（Source Routing）路径头插入在 UDP Header 和 WireGuard 加密数据之间，
为数据包提供显式路径信息。路径头是明文的，通过 HMAC-SHA256 保护完整性。

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

### HMAC（8 字节）

| 偏移 | 长度 | 字段名 | 说明 |
|------|------|--------|------|
| 16+N*6 | 8B | hmac | HMAC-SHA256 截断为 64 位 |

### 长度计算公式

```
hdr_len = 16 + seg_count * 6 + 8 = 24 + seg_count * 6
```

## HMAC 覆盖范围

包含：ver_flags, hdr_len, seg_count, reserved, hmac_id, path_id, timestamp, segments[0..N-1]
排除：magic（固定值）, seg_left（每跳变）, action（每跳可能变）

## 段列表语义

- segments[0] = 最终目的节点（locator:vpn_port）
- segments[seg_count-1] = 外层 UDP dst 之后的第一个中转
- 外层 UDP dst = segments[seg_count-1] 的 IP:Port（首跳免费）
- 中转处理：取 segments[seg_left-1] 作为新外层 dst，seg_left--

## UDP 端口分配

| 端口 | 用途 | 行为 |
|------|------|------|
| 51820 | VPN 隧道 | Encap/Decap |
| 51821 | 中转 | Transit（只改外层头，不解密） |
| 51822+ | 预留 | 未来 service-chain 等 |
