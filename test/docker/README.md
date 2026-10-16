# Docker VPP 开发环境

用于 SR-WG VPP 插件的开发与功能测试。基于 Ubuntu 22.04 + VPP 官方包仓库（packagecloud.io/fdio），使用 tap 接口（非 DPDK，Docker 内无巨页支持）。

## 拓扑

三节点拓扑：`node1`（首节点，隧道起点）→ `transit`（中转节点，只转发不解密）→ `node2`（末节点，隧道终点）

| 网络 | 网段 | 说明 | 节点 |
|------|------|------|------|
| net_a | 10.10.1.0/24 | node1 ↔ transit 数据面 | node1 (.2), transit (.3) |
| net_b | 10.10.2.0/24 | transit ↔ node2 数据面 | transit (.3), node2 (.2) |
| net_ctrl | 10.10.0.0/24 | 管理/控制面 | node1 (.2), transit (.3), node2 (.4) |

所有容器 `privileged: true`（VPP 需要大页内存和网卡访问权限），源码目录挂载：
- `vpp-plugin/` → `/workspace/vpp-plugin`
- `test/` → `/workspace/test`

## 快速开始

```bash
# 构建镜像
docker-compose build

# 启动三节点
docker-compose up -d

# 进入容器
docker exec -it srwg-node1 bash
```

## 容器内操作

```bash
# 启动 VPP（前台）
vpp -c /etc/vpp/startup.conf &
# 或使用脚本
start-node.sh node1

# 验证 VPP
vppctl show version
vppctl show interface

# 编译插件
build-plugin.sh

# 加载插件
vppctl plugins load sr_wg_plugin.so

# 查看插件状态
vppctl show plugins
```

## 网络连通性验证

```bash
# node1 -> transit
docker exec srwg-node1 ping -c 3 10.10.1.3

# transit -> node2
docker exec srwg-transit ping -c 3 10.10.2.2

# node1 -> node2（需 VPP 转发路由配置后）
docker exec srwg-node1 ping -c 3 10.10.2.2
```

## 清理

```bash
docker-compose down -v   # -v 同时删除 /run/vpp 数据卷
```

## 备注

- VPP 日志：`/var/log/vpp/vpp.log`（容器内）
- API socket：`/run/vpp/api.sock`（二进制 API，控制器使用）
- CLI socket：`/run/vpp/cli.sock`
- 如果 packagecloud 仓库不可用，参见 Dockerfile 头部注释的源码编译备选方案
