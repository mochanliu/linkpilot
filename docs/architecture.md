# SD-WAN CPE 数据面架构

## 系统概览

本项目实现一套基于 VPP（Vector Packet Processing）的高性能 SD-WAN CPE 数据面，
结合 SR（Source Routing）源路由与 WireGuard 加密隧道，由 Go 控制器统一管理。

```
┌─────────────────────────────────────────────────────────────┐
│                      Go Controller (sr-ctrl)                 │
│  ┌──────────┐  ┌──────────────┐  ┌───────────────────────┐  │
│  │ Path Comp│  │ VPP API Client│  │ Northbound REST/gRPC │  │
│  └────┬─────┘  └──────┬───────┘  └───────────────────────┘  │
└───────┼────────────────┼────────────────────────────────────┘
        │                │ (Binary API / Shared Memory)
        ▼                ▼
┌─────────────────────────────────────────────────────────────┐
│                         VPP Data Plane                        │
│  ┌────────────┐  ┌────────────┐  ┌────────────────────────┐ │
│  │ SR Plugin  │  │ WG Plugin  │  │ Standard VPP Fwd Graph │ │
│  │ (encap/    │  │ (encrypt/  │  │ (ip4-unicast,          │ │
│  │  decap/    │  │  decrypt)  │  │  ip4-lookup, etc.)     │ │
│  │  transit)  │  └────────────┘  └────────────────────────┘ │
│  └────────────┘                                              │
└─────────────────────────────────────────────────────────────┘
```

## 核心组件

### 1. VPP SR Plugin（vpp-plugin/）

自定义 VPP 插件，注册在 `ip4-unicast` 节点之后，负责：

- **Encap**：在出口方向，将 SR Header 插入 UDP 与 WG payload 之间
- **Transit**：中转节点仅修改外层 IP/UDP 目的地址，seg_left 递减
- **Decap**：在最终目的节点剥离 SR Header，将 WG 数据包送入解密流程

### 2. WireGuard 加密

使用 VPP 内置或第三方 WG 插件完成 Noise IK 握手与数据加密。
SR Header 位于 WG 加密载荷之外（明文），保证中转节点可路由但不可篡改（HMAC 保护）。

### 3. Go Controller（controller/）

- **cmd/sr-ctrl/**：控制器入口，启动 gRPC/REST 服务
- **internal/path/**：路径计算引擎（最短路径、策略路由、故障切换）
- **internal/vppapi/**：封装 VPP Binary API，下发转发表项与 SR 策略
- **internal/api/**：北向接口，对接编排系统或 CLI

### 4. 测试框架（test/）

- **unit/**：Go 单元测试 + VPP 插件 C 单元测试
- **docker/**：多节点 Docker Compose 拓扑，模拟完整 SD-WAN 环境
- **scripts/**：自动化测试脚本
- **attack/**：安全测试（HMAC 伪造、重放攻击等验证）

## 数据流

```
发送端 CPE:
  内网 IP 包 → VPP ip4-lookup → SR Encap（添加 SR Header）
  → WG Encrypt → 外层 UDP/IP 封装 → 物理出口

中转节点:
  物理入口 → UDP 51821 → SR Transit（修改外层 dst, seg_left--）
  → 重新计算 checksum → 物理出口

接收端 CPE:
  物理入口 → UDP 51820 → SR Decap（剥离 SR Header）
  → WG Decrypt → VPP ip4-lookup → 内网交付
```

## 设计原则

1. **高性能**：数据面全部在 VPP 用户态完成，零拷贝，batch 处理
2. **可编程路径**：控制器通过 SR 源路由实现灵活的路径选择
3. **安全**：WireGuard 端到端加密 + SipHash-2-4 路径完整性保护
4. **可测试**：Docker 多节点拓扑，支持自动化集成测试和攻击模拟
5. **渐进部署**：SR Header 可选，不携带时退化为普通 WG 隧道
