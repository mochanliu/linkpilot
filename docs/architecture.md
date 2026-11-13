# LinkPilot SD-WAN CPE 数据面架构

## 系统概览

LinkPilot 实现一套基于 VPP（Vector Packet Processing）的高性能 SD-WAN CPE 数据面，
结合 SR（Source Routing）源路由与 WireGuard 加密隧道，由 Go 控制器统一管理。

> **产品名**：LinkPilot。界面文案与文档叙述提到这套产品时用 LinkPilot；
> 代码标识符、二进制名、文件名与环境变量保留原有的 `sr-*` / `vbond` 写法（内部代号，
> 不随产品名改），历史实验记录里的旧称也保留原样。

```
┌─────────────────────────────────────────────────────────────┐
│               LinkPilot Controller (sr-ctrl)                │
│  ┌──────────┐  ┌──────────────┐  ┌───────────────────────┐  │
│  │Enroll/CA │  │Registry+Watch│  │  Tunnel Param Push    │  │
│  └────┬─────┘  └──────┬───────┘  └───────────────────────┘  │
└───────┼────────────────┼────────────────────────────────────┘
        │                │ (mTLS JSON / vppctl)
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

### 3. Go Controller（controller/）—— LinkPilot 控制器

独立 Go 模块 `sdwan/controller`，**零第三方依赖**（VPP 侧走 vppctl，不引 govpp）。
职责 = CPE 安全入网 + NAT 地址发现 + 隧道参数下发，协议是 JSON over mTLS：

- **cmd/sr-ctrl/**：控制器。自建 CA → claim code 入网签发设备证书 → 注册表 + watch 推送。
- **cmd/sr-stunprobe/**：手工探针，复用 `internal/stun` 的那个 `Prober`（和设备里跑的是
  同一份代码），所以它除了验证协议与 reflector，也当场打印 `local=`（这条 socket 的
  本地源）。**不代表数据面 socket 的映射**：探针从内核出去，不碰 VPP。
- **internal/wire/**：控制面协议类型的唯一来源（服务端与 agent 共用，字段不可能两侧漂移）。
- **internal/keybinding/**：Ed25519 设备身份 + "这把 X25519 公钥是我的"的签名接缝。
- **internal/stun/**：RFC 5389 编解码 + 内核 UDP 的收发两端 —— `reflector.go`（控制器侧
  应答器）与 `client.go`（设备侧 `Prober`：一条复用的 socket、逐目的地探测、给对称 NAT
  判定）。`Mapping` 里 `Addr`（反射器看到的我）和 `Local`（内核给这条 socket 选的本地源）
  两个都必须上报：前者是"对端该拨哪儿"，后者才是"我从哪个口出去"。
- **test/scripts/test-vbond.sh**：acceptance harness（复用数据面实验室的 `docker_net_ctrl`）。
- **test/scripts/vbond-client.sh**：openssl+curl 的手工客户端，不依赖 agent。

#### LinkPilot 控制器的三条定案（2026-10-17）

1. **地址发现不另开探针 socket**：探针就是 WG 自己的 persistent-keepalive。CPE 把已入网的
   观测节点配成一个 WG peer，keepalive 从数据面真正要用的那个 socket 出去；观测节点记录
   收到的外层源 `(ip:port)` 并回报控制器。理由：控制器要的是"隧道将来实际会被 NAT 成什么"，
   用另一个 socket 量到的映射是另一件事。（与思科 vBond 的观测方式一致。）
2. **UDP 端口按动作显式下发**：`vpn_port=51820`、`transit_port=51821`，控制器下发两个显式
   端口而不是 `opcode_base`，新增一种 action 就再下发一个端口。`srou_ip` 这类"标签地址"
   不需要——SR 段列表存的就是真实 underlay `ip:port`。
3. **WG 密钥由 agent 生成、装入 VPP**：`wireguard` API 收 `private_key`，所以私钥由 agent
   产生后交给数据面持有；公钥用设备身份密钥签名后随 heartbeat/mapping 上报，对端拿
   pinned CA 自己验链。这样密钥担保不依赖控制器诚实。

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
