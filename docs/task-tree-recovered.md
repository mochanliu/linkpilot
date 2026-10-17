# 任务恢复 · 断点与专家分工

> 来源：旧版 Qoder Desktop experts 会话 `8851eb54-9bbb-45e9-969c-644087658b04`（标题 "SDWAN"，工作区
> `/Users/louis/Documents/QoderCN/2026-10-15/chat-1`）。该会话在新版只被扫描登记、从未导入，任务树与子代理
> 报告以明文形式保留在 `~/.qoder-cn/cache/experts/8851eb54-…/{tasks,agents,inboxes}`。
> 会话气泡正文（123 条）在旧库 `local.db` 中是密文，本文不猜测其内容，只用明文可证部分。

## 一、项目定位（用户原话锚点）

- 这是**同一个自研 SD-WAN 项目**，不是 SDN 控制器项目。
- 数据面 = VPP 插件 + SR 头 + MAC 保护（本仓 `vpp-plugin/`）。
- 编排/控制面 = 另一半（vBond 式 CPE 入网、STUN、路径下发）。
- 用户的两条原始约束（明文取自 `inboxes/leader.json`）：
  1. "路径信息是明文的，在密文前 udp 后，所以需要 hmac 机制保护起来" —— SR 头明文可见，需 MAC 保护。
  2. "未来扩展的所有 function 都是通过 udp 端口来区分" —— UDP 端口是数据面的功能多路复用键。
  3. "我开了 docker，你的所有研发和测试都可以在 docker 下起 ubuntu 容器进行。"

## 二、任务树（10 项，与旧会话一致）

| # | 任务 | Owner | 状态 |
|---|------|-------|------|
| 1 | 搭建项目目录结构并创建 refactor 分支 | Lee | completed |
| 2 | 实现 SR Header 库和 HMAC 模块（Phase 1 核心） | Taylor | completed |
| 3 | 编写 SR Header 库单元测试 | Jay | completed |
| 4 | 搭建 Docker VPP 开发环境 | Felix | completed |
| 5 | 在 Docker 中验证 SR Header 库编译和单元测试 | Chris (qa) | completed |
| 6 | 实现 VPP 插件骨架（Phase 2） | Robin | completed |
| 7 | 在 Docker 中验证 VPP 插件编译和加载 | Ben | completed |
| 8 | 实现 Encap 节点完整逻辑和 WireGuard 集成（Phase 3） | Jimmy | completed |
| 9 | 修复 Docker 环境中 WireGuard 握手失败问题 | Oscar | completed |
| 10 | 替换 HMAC-SHA256 为 SipHash-2-4 并修复 Encap 路径 | Jason | **in_progress（断点）** |

## 三、#10 断点的确切位置

#10 含两件事，**都没做完**：

### (a) HMAC-SHA256 → SipHash-2-4（128-bit key / 64-bit tag，去 OpenSSL 依赖）

- 半成品已存为 `siphash-wip.patch`（12 文件，已在基线树上 `git apply --check` 验证可干净落上）。
- **已知问题**：容器内单测 275/276，仅 SipHash 标准 KAT 向量不过
  （实测 `len15 = b6b609ab1579022d`，期望 `a129ca6149be45e5`）；另用独立参照逐长度比对仍不一致 → 实现确有 bug，
  不是向量抄错。
- 参考物：`develop` 分支上有 `code/include/srou/siphash.h`（`SROU_SIPROUND` / `srou_rotl64`），
  且其北向模型里 `d_key` 就是"逐跳 SipHash"，很可能是已知良好的实现。

### (b) encap 在 WG 短接下收不到包（数据面重构，风险较大，旧会话未擅自改动）

- 现象：e2e `18 passed / 4 failed`；`encap_ok / transit_ok / decap_ok` 全 0。
- 实测证据：node1 `show node wg4-output-tun` 显示 `adj-midchain-tx = 33 vectors`、`sr-wg4-encap = 0 vectors`。
- 根因：WireGuard 的 `wg4-output-tun` feature 直接走 `adj-midchain-tx`，**不经过 `ip4-output` arc**，
  所以挂在 `ip4-output` 上的 `sr-wg4-encap` 节点拿不到包。
- 旧会话给出的两个候选方向（均未实施）：
  1. 改到**物理接口**（如 host-eth0）的 `ip4-output` arc 上拦截外层 WG 包，重做 endpoint/端口匹配；
  2. 放弃 feature arc，改用**自定义 DPO / rewrite** 显式串接。

## 四、代码基线与环境

- 本仓分支 `refactor/dataplane`，HEAD `1f44722`（Phase 1-3 从未提交，已固化快照）。回滚后基线单测 **266/266 ALL PASS**。
- `vpp-plugin/` 3388 LOC；线格式单一真源 `vpp-plugin/sr_header.h`；12 个 RPC 在 `vpp-plugin/sr_wg.api`。
- Docker 三节点：`srwg-node1` / `srwg-transit` / `srwg-node2`（当前在跑）。
  `/workspace/vpp-plugin`、`/workspace/test` 为绑定挂载，改宿主文件即时生效。
- **`test/docker/crypto_openssl/` 必须保留**：VPP 官方包无同步 crypto handler，WG 握手靠这个自研插件修好（#9）。
- 宿主机无可用工具链（`xcrun: invalid active developer path`），一切编译/测试进容器。

## 五、旧会话的下一步（用户当时指的方向）

实现类 Cisco vBond 的能力：**新 CPE 入网（enrollment）+ STUN**，并尽量复用 `develop` 分支上已由 workbuddy
实现、且测试通过的代码（控制器 `code/ctrl/server/main.go`、CPE 代理 `cpeagent/agent.go`、真 RFC 5389 STUN
客户端 `src/control/internal/dpdiscover/dpdiscover.go`、Ed25519 claim-code + mTLS、X25519 密钥绑定、
O-key 派生，以及 `code/ctrl/test/e2e.sh` 1047 行 / `keyplane.sh` 406 行两套装在跑的证据）。

先出方案，再动代码。方案要落回本项目既有约束：SR 头明文需 MAC、UDP 端口做功能多路复用、容器内可验证。

---

## 六、完整架构恢复（出处：`develop` 分支 `12-Architecture-Overview-and-Progress.md`，
##     它是 00–11 号文档的汇总定稿；WorkBuddy 镜像仓有 00–44 共 45 篇决策记录）

### A. 产品定义

> **部署像 Meraki 一样简单、能力像 Viptela 一样强的 SD-WAN，差异化在"源路由 TE 写进包头"（SRoU），
> 且全尺寸设备跑同一个软件镜像。**

| # | 产品承诺 | 技术后果 |
|---|---|---|
| P1 | 一个二进制覆盖 100M 门店 → 1–10G 分支 → 10–100G Hub；PoP/CPE/Hub/Spoke 只是逻辑标签 | 没有角色专用镜像/测试矩阵，**角色按包计算** |
| P2 | 源路由 TE + **中转不解密**的无状态转发；任何 CPE 都能在自己边缘装路径 | 中间节点只改 2 字节并转发；这是**唯一自研的数据面功能** |
| P3 | **无 PoP 概念**，没有运营商骨干；"PoP" 就是客户自己的另一台 vCPE | 一张 overlay 只属于一个租户，分段按租户而非按骨干 |

**与 Viptela 的刻意分歧**：Viptela 把路径计算集中在 vSmart；我们把**路径决策放在边缘**（P2 决定）。推论：

> **控制器是 truth provider，不是 decision maker。**
> 控制器拥有：谁存在（身份）、在哪里（endpoint 表）、链路好不好（telemetry/TE DB）、允许什么（策略约束）。
> 边缘拥有：**此刻用哪条路径**。

必须守住不变式：**控制器全挂不影响已建立隧道转发**，只是不再重优化、不能新建隧道。

### B. 数据面（六条承重决策）

- **B.1 密码学**：把"WG 还是 IPsec"拆成四个正交维度 —— 密码算法 **AES-GCM**（只有它能硬件卸载，100G 才够）；
  密钥管理 **控制面下发、无 IKE**（这正是 POC 选 WG 的真实原因，不是速度）；封装 **自研 SRoU shim**
  （唯一能让中转不解密还转发的办法）；抗重放/SA ≈100 行自己写（借 ESP 的思路不借它的框架）。
  准确表述是"100G 必须 AES-GCM + 硬件卸载"，不是"100G 必须 IPsec"。
- **B.2 双完整性域**（"中转不能解密"因此是**结构性**保证而非策略）：
  端到端 = AES-GCM / **O-key**（只有两端持有）覆盖整个内层 IP 包；
  逐跳转向 = 截断 MAC（48 bit）/ **D-key**（域内共享）只覆盖 shim 的不可变字段。
  **关键规则：shim 不进 AEAD 的 AAD**，否则每一跳都要重算 ICV → 每跳都要 O-key → 保证塌掉。
  中转完全不做密码运算，所以 Hub 的 100G 主要是**转发**指标。
- **B.3 数据面形态**：统一控制面 → 后端无关的策略 IR → 薄数据面抽象 → 后端在**构建/安装期**选定
  （eBPF/TC+XDP 100M–1G｜内核 XFRM+eBPF 1–10G｜VPP/DPDK 10–100G）；CI 必须 x86-64 与 arm64 双跑。
  硬件方向改为**单 DPU 盒子**（首选 Marvell OCTEON 10，未实测），三个 go/no-go 实验（快路径能否做我们的
  头操作、shim 与 inline-crypto 的先后、LS1023 真实吞吐）尚未做。
- **B.4 转发模型 SRoU v4**（`code/include/srou/srou_v4.h`，56B）：标签**就是 ip:port**（IP 说去哪、端口说做什么），
  标签在包里 ⇒ 中转**不需要 node_id→地址映射**（正是"无 PoP/边缘决策/分布式"的落地）。
  字段：ver_flags(0x4A) / SegCount|Flags / protoid / hoplimit / flowid / epoch / nonce / orisrc_ip / orisrc_port /
  segleft / rsvd / **signer(2B node ID)** / seg[4]{ip:port} / mac(6B)；MAC 覆盖 48B=384bit；**首跳无标签**，
  seg[0] 是最终目的地。逐跳状态机分 POP / DELIVER 两支，含"强制路径检查 seg[segleft]==(my_ip, arrival_port)"。
- **B.5 无角色模型**：`role = f(SegLeft, SegCount)`，同一台设备在不同包上走 ingress/transit/egress 三支。
  转发节点只有三张表：FIB(node_id→下一跳)、keyring(signer,epoch)→16B 验签密钥、my_id。
  **三条不变式**：[C1] signer 由路径推出（= seg[SegCount-1]，零额外字节）；
  [C2] 密钥由 signer 选择、**可本地重算** `K(node,epoch)=SipHash(E_root,0x51‖node‖epoch)‖SipHash(E_root,0x52‖…)`，
  每租户独立 `E_root` → 免费拿到域隔离，是限爆炸半径的主工具；
  [C3] **首跳绑定**（对称 MAC 下"能验就能签"，故在首跳要求 signer == tunnel peer，与攻击者是否持有全密钥表无关）。
- **B.6 服务链**：段边界 == 加密边界 == 服务链边界；服务节点必在 segleft==0 的 egress 剥 shim 解密、
  再作为新 ingress 重新签名重新封装（`srou_reoriginate`）。
  **服务链是"可强制"而不只是"可引导"**：`signer==服务节点` 密码学上等价于"确实被它重新封装过"，
  旁路包 MAC 合法但 `egress_admits=0` 被丢 —— 可写进等保条款。

### C. 控制面（M-C0 已实现并通过）

单个 Go 二进制、**从第一天就多租户**，Postgres + bus，非微服务；模块 = Enrollment / Orchestrator(endpoint
registry, NAT assist) / Policy-Config templates / Telemetry&TE DB；到 CPE 走 gRPC(mTLS, streaming)。

- **零触入网**：设备首启生成 Ed25519 keypair → `POST /enroll {serial, pubkey, claim_code}` →
  claim→tenant、分配 `node_id`（2B，与 v4 shim signer 同一个 ID）→ 签设备证书。**证书本身就是授权**，
  任何请求参数都改不了租户。
- **多租户四层**（缺一不可）：身份（按租户签发证书）/ 数据（每条记录带 tenant_id、查询强制作用域）/
  密钥（每租户 E_root，KDF 输入含租户）/ 数据面（v4 shim MAC 按租户 keyring 验，跨租户包第 1 跳即丢）。
- **隧道自动化两个子问题**：可达性（endpoint registry，控制器从连接源五元组**看见** NAT 映射，变更即推）；
  会话密钥（**控制器代议 X25519**：双方交换临时公钥半，`HKDF(X25519(eph,peer_eph), transcript, tenant_id)`，
  控制器**永远拿不到会话密钥**；每会话前向保密、定时 rekey、无 IKE 状态机）。shim MAC 的 keyring **故意**由控制器下发。
- **NAT 穿越四角色**：Reflector(UDP 3478，"我的映射是什么") / Rendezvous(TCP 8444，登记+推送) /
  Prober(CPE 同时发包突刺) / Relay（**就是我们同一镜像的另一台 overlay 节点**，且**永不解密**）。
  三个常被忽略的性质：双方都要发（单边听必败）、**单 socket 纪律**（映射是按 (local port, remote addr) 学的，
  开第二个 socket 就把学到的全扔掉）、中继不解密。
- **HA（单区域·境内）**：两个无状态实例 + DNS/LB 共享一套 Postgres，**不用 etcd 不做选主**；
  **epoch 计数器放 DB**（全局单调）防两实例给出冲突 epoch；数据驻留含备份；ICP/增值电信/等保 2.0 是 SaaS 形态的门禁。

### D. 已实测数字（可引用，LS1023A）

AES-256-GCM 软件 677 MB/s≈5.4Gbps；ChaCha20-Poly1305 267 MB/s（慢 2.5×）；
**SipHash-2-4 32B 输入 62.8–70ns → 13.4–15.9 Mpps/core**；HMAC-SHA256 纯软件 5070ns（**慢 80.7×**），
10Gbps/1420B 要吃掉 446% 一个核 → 热路径不可用。
验证结论：跨语言跨架构 diff = 0；v3 224/224 bit-flip 全拦、10 万伪造 0 通过、T1–T13；
v4 384/384 全拦、20 万伪造 0 通过、T1–T17（含 T17 服务链强制 admits=1/bypass=0）；
SipHash 官方向量 `726fdb47dd0e0e31` 命中，KDF 向量 `a0e00e7246de53e87b5caa2d9e8fab4d` v3↔v4 一致。
控制面 M-C0：2 CPE（其中一台在**真 MASQUERADE NAT** 后）e2e 全绿，映射发现拿到 NAT 后地址、
打洞 20/20 reach=ready、重启换端口后 peer 收到推送、tenant-b 注册表只见自己节点。

### E. 路线图与遗留

已完成：SRoU v3/v4 格式+状态机（真机验证）、控制器 M-C0。
待做：**M-C1** 零触入网端到端+最小面板 → **M-C2** X25519 代议 + 密钥落到数据面 map + **穿隧道 ping** →
**M-C3** telemetry→TE DB→边缘重算路径 → **M-C4** 单控制器第二租户验四层隔离 → **M-H1** Hub 层 DPU 实验。
命名跟进项 F1–F8：Postgres 接 Store、gRPC 换传输（保留 wire 类型）、X25519 会话密钥、控制器 HA 演练、
**真 RFC 5389 STUN 互操作**、Relay 角色进 CPE 镜像、epoch 落 DB、合规线。
明确推迟：IPv6 underlay、国密 SM2/3/4、nonce 滑窗抗重放、运营商 TE。
风险 9 条中最硬的四条：自定义封装**可能打断 RSS**（外层 UDP 源端口必须从第一天带流熵
`0xC000|(hash(inner5)‖salt)&0x3FFF`，这是设计缺陷不是调优问题）；XDP 不能加头（封装放 TC、解封装放 XDP）；
MTU/分片（源路由+加密后内层只剩 ~1400B，PMTUD 与 MSS 钳制必做）；
边缘独立决策的**同向振荡**（要滞后阈值+冷却，且**段列表永不参与转发决策**，把抖动锁在起点）。
不可消除的信任边界：已注册且持有验签密钥的节点能改别人的路径 —— 对称 MAC 防不住，靠每租户 E_root 缩小爆炸半径。

### F. POC 逆向的六条真问题（已由设计取代，留作反面基线）

明文 bearer token 当流标识（`(P&0xFFFF)==prev` 不依赖任何秘密，1Mpps 下 59ms 穷举）→ 换成 48bit SipHash MAC 覆盖 48B；
转发支路是 UDP reflector + 伪造源注入 → 强制路径检查 + [C3] 关闭；宣称的 HMAC **代码里根本没有**；
`checklicense()` 里硬编码 RSA 私钥；`time.FixedZone("Beijing Time", 0)` 时区错 8 小时；
控制面从不加载/挂载 eBPF 且所有错误被 `_ =` 吞掉。

---

## 七、恢复过程中发现的**真冲突**（需要用户拍板，不要由我替他决定）

`refactor/dataplane` 上的 VPP 插件用的线格式与 doc 09/12 定稿的 **SRoU v4 不是同一个东西**：

| | 当前 VPP 插件 `sr_header.h` | 架构定稿 SRoU v4 `srou_v4.h` |
|---|---|---|
| magic/ver | `0x53`，ver_flags 高 4 位版本 | `0x4A` |
| 长度 | 16 + 6×seg_count + 8，**变长**，最多 8 段（72B） | 32 + 6N，**4 固定槽**（56B） |
| MAC | **8 字节**（SipHash-2-4 64bit tag），覆盖 13B+段 | **6 字节**（48bit），覆盖 48B |
| 字段 | hmac_id / path_id / timestamp | flowid / epoch / nonce / orisrc_ip / orisrc_port / **signer** |
| 密钥 | `hmac_id` 索引，p-key **由控制器下发**（`sr_wg key set`） | [C2] `K(node,epoch)=SipHash(E_root,…)` **可本地重算** |
| [C1] signer 由路径推出 | **无** | 有 |
| [C3] 首跳绑定 | **无** | 有 |
| 强制路径检查 | 依赖 VPP 本地投递（未显式比对 my_ip+arrival port） | 显式检查 |

共享的只有 SipHash-2-4 原语与"E_root 域根"思路。也就是说：**控制面（doc 10/11、`wire.go`）的授权模型是围绕
v4 的 node_id/epoch/E_root 设计的，而数据面插件目前吃的是 path_id/hmac_id/p-key。**
在 vBOND 接入之前必须二选一（统一到 v4，或把 v4 的不变式移植进当前格式），否则入网下发的东西数据面不认。
