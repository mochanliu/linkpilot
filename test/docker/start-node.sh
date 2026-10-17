#!/bin/bash
# 启动单个节点的 VPP 实例（容器内执行）
#
# 用法: start-node.sh [node_name]
#   node_name 仅用于日志前缀，不影响配置。
#
# 三件事：
#   1. 清理上一次异常退出留下的 unix socket
#   2. 安装 SR-WG 插件（编译产物在宿主机挂载卷里，三节点共享）
#   3. 在伪终端里 exec VPP
#
# 第 3 点说明：VPP 以 `nohup ... &` / `docker exec -d vpp` 这类**无 PTY**
# 的方式后台启动时会在 exec 会话回收后静默退出（日志里没有任何信号记录），
# 而前台 `timeout N vpp` 完全正常。用 script(1) 分配一个伪终端即可稳定保活。
set -e

NODE_NAME=${1:-node1}
VPP_CONF=${VPP_CONF:-/etc/vpp/startup.conf}
PLUGIN_DIR=${PLUGIN_DIR:-/usr/lib/x86_64-linux-gnu/vpp_plugins}
BUILT_SO=${BUILT_SO:-/workspace/vpp-plugin/build/lib/vpp_plugins/sr_wg_plugin.so}

echo "[${NODE_NAME}] preparing VPP runtime dirs ..."
mkdir -p /var/log/vpp /run/vpp
# 陈旧 socket 会让 VPP 启动即失败：
#   clib_socket_init: Active listener on '/run/vpp/stats.sock'
rm -f /run/vpp/*.sock
chgrp vpp /run/vpp 2>/dev/null || true

if [ -f "${BUILT_SO}" ]; then
    cp -f "${BUILT_SO}" "${PLUGIN_DIR}/"
    echo "[${NODE_NAME}] installed $(basename "${BUILT_SO}") -> ${PLUGIN_DIR}/"
else
    echo "[${NODE_NAME}] WARNING: ${BUILT_SO} not found, plugin will NOT be loaded" >&2
fi

# 内核转发必须关掉：本容器同时被 kernel 和 VPP 使用（host-interface 是
# AF_PACKET，kernel 仍会收到一份拷贝），开着 ip_forward 会让 kernel 抢先
# 转发同一个包，造成重复包并被 WireGuard 的 replay window 丢弃。
sysctl -w net.ipv4.ip_forward=0 >/dev/null 2>&1 || true

echo "[${NODE_NAME}] starting VPP (conf=${VPP_CONF}) ..."
# typescript 落盘而不是 /dev/null：容器里没有 rsyslog，插件的 clib_warning
# 只能从 VPP 的 stdout 里捞（排障时必须能看到）。
exec script -qefc "vpp -c ${VPP_CONF}" "${VPP_OUT_LOG:-/var/log/vpp/vpp-stdout.log}"
