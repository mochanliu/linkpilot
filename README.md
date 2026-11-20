# LinkPilot SD-WAN

An SD-WAN CPE data plane on **FD.io VPP**, with a source-routed + WireGuard-encrypted
overlay and a Go control plane. Linux only.

**Keywords:** open-source SD-WAN · SDWAN · software-defined WAN · SRv6-style
segment routing · source routing · WireGuard · FD.io VPP · VPP plugin · CPE ·
route reflector · NAT traversal · STUN · FEC · 开源 SD-WAN · 软件定义广域网 ·
广域网组网 · 零配置入网

> **Status: 0.9-beta, pre-1.0.** Interfaces, on-disk formats and the console change
> between builds without deprecation. It runs a live network, but treat it as
> beta software and read [docs/release-0.9-beta.md](docs/release-0.9-beta.md) for the
> limits that are known and accepted.

## What it does

- **Data plane** — a custom VPP plugin that inserts a source-routing header between
  UDP and the WireGuard payload, so a packet carries its own path. Encap, transit
  (multi-hop, up to 3 relays) and decap are three graph nodes; WireGuard does the
  encryption; VPP does the forwarding.
- **Overlay** — tunnels are VPP WireGuard + SR header, addressed per tenant. Path
  selection is by colour and weight, with per-tunnel FEC for lossy links.
- **Underlay** — direct or relayed UDP tunnels over any address family that routes,
  including NAT'd sites (STUN-observed endpoints, keepalive, port prediction).
- **Control plane** — `sr-ctrl` (enrollment, tenant CA, config compiler, console,
  Raft-style cluster) and `sr-rr` (route reflector grid). Both are pure Go with no
  third-party modules.
- **CPE agent** — `sr-agent` enrolls with a one-time claim code, then pulls config
  bundles over mTLS and applies them to VPP and FRR without restarting daemons.

## Components

| Path | What | Language |
| --- | --- | --- |
| `vpp-plugin/` | VPP plugin: SR header, FEC, QoS marking, observability | C |
| `controller/cmd/sr-ctrl` | Controller: enrollment, CA, registry, console, cluster | Go |
| `controller/cmd/sr-rr` | Route reflector: overlay route distribution, RR mesh | Go |
| `controller/cmd/sr-agent` | CPE agent: enroll, heartbeat, apply bundles | Go |
| `controller/internal/` | Config compiler, VPP binary API client, overlay/underlay models | Go |
| `test/` | Lab: docker compose CPE/controller, vCPE bootstrap, e2e scripts | shell |
| `docs/` | Design notes, deployment, and the engineering log | Markdown |

The console is a single embedded HTML file (`controller/cmd/sr-ctrl/console.html`)
served by `sr-ctrl`; it has no build step and no runtime CDN or third-party JS.
It ships in **Chinese and English** — the switch sits in the top-right corner, the
choice is remembered per browser, and `?lang=en` forces a language for a link.
Translations live in `controller/cmd/sr-ctrl/assets/i18n-en.js`.

## Build

### Controller and agent (Go 1.22+, no third-party modules)

```sh
cd controller
go build ./cmd/...          # sr-ctrl, sr-rr, sr-agent
go vet ./...
go test ./...               # add --cap-add NET_ADMIN if you run the krt package in docker
```

Cross-compiling for a CPE (static, no cgo):

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/...
```

### VPP plugin

Requires the VPP development headers and libraries (`vpp-dev` / a VPP source tree
at **26.06**, the version this is developed against), plus `cmake` and `ninja`.

```sh
cd vpp-plugin
mkdir -p build && cd build
cmake .. -DCMAKE_BUILD_TYPE=Release -G Ninja
ninja
# install sr_wg_plugin.so into your VPP plugin directory and add it to plugins { plugin sr_wg_plugin_plugin.so { enable } }
```

The C sources also build standalone as a static library for unit tests: `make -C vpp-plugin lib`.

## Try it in the lab

The lab runs everything in containers on one Linux host (see `test/lab/README.md`):

```sh
make -C test/lab bringup          # controller + RRs + a couple of CPE containers
test/lab/vcpe.sh up --claim CODE  # enroll a fresh "CPE" with a claim code from the console
test/lab/vcpe.sh status           # show its tenant / node id / heartbeat
```

The console listens on `:8443` (TLS) and `:8443/api`; the device mTLS endpoint is
`:8444`. The first start prints a console token once — use it to log in and create
the first tenant.

## Documentation

Docs are written in **Chinese**; the code, CLI flags, log lines and the console's
English mode are English. Start here:

- [docs/architecture.md](docs/architecture.md) — data plane and control plane overview
- [docs/controller-ha-deploy.md](docs/controller-ha-deploy.md) — deploying a controller cluster
- [docs/control-plane-discovery.md](docs/control-plane-discovery.md) — how a CPE or RR finds the controller
- [docs/agent-handover.md](docs/agent-handover.md) — engineering log: what was measured, what broke, what is still open
- [docs/sr-header-format.md](docs/sr-header-format.md) — on-the-wire SR header

## License

Apache License 2.0 — see [LICENSE](LICENSE). Third-party components and why
Apache-2.0 is the only sane choice for a VPP plugin: [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
The "LinkPilot" name and logo are not covered by the license.

## Acknowledgements

The SR header's *design approach* — a source-routing header with a magic byte
and a segment list placed at the head of the UDP payload — is inspired by the
expired Internet-Draft [draft-zartbot-sr-udp](https://datatracker.ietf.org/doc/draft-zartbot-sr-udp/)
("Segment Routing over UDP (SRoU)", 2020, not adopted). The on-the-wire format
implemented here is our own: no text, figure or code from the draft is reproduced.
See [docs/sr-header-format.md](docs/sr-header-format.md) §设计渊源.

## Contributing

Issues and pull requests are welcome — read [CONTRIBUTING.md](CONTRIBUTING.md)
first (DCO sign-off, one topic per change, no daemon restarts in the config path).
Security reports: [SECURITY.md](SECURITY.md).

---

## 中文摘要

**LinkPilot 是一套开源的 SD-WAN（SDWAN，软件定义广域网）方案**：数据面用自研 VPP 插件在 UDP 与
WireGuard 载荷之间插入源路由头，控制面是纯 Go 的 `sr-ctrl`（入网/CA/配置编译/控制台/集群）、
`sr-rr`（路由反射器）和 `sr-agent`（CPE 侧代理），三者都不依赖任何第三方 Go 模块。

- 构建：`cd controller && go build ./cmd/...`；插件见 `vpp-plugin/`（需 VPP 26.06 开发包）。
- 上手：`make -C test/lab bringup`，再用 `test/lab/vcpe.sh up --claim <码>` 入网一台 vCPE。
- 文档是中文的，代码、命令行、日志与控制台英文模式是英文。
- 许可证 Apache-2.0；第三方组件与合规说明见 `THIRD_PARTY_NOTICES.md`。
