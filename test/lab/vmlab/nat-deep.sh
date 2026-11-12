#!/bin/bash
# nat-deep.sh —— NAT 假死实验的 VPP 节点级深采样（lab 工具，不进产品）。
#
# 为什么：nat-watch（2s 轻采样）证明假死时 /proc 层一切正常（load/mem/pid 无异常），
# TCP 全死而 ICMP 活 —— 怀疑 VPP↔内核的 punt/lcp 通道被打断。vppctl 走 unix domain
# socket 不依赖网络，假死时仍能答话；这个脚本每 5s 从 VPP 自身视角记录 drop 计数、
# NAT 会话、接口计数、lcp pair 状态和内核侧地址，落盘 /var/log/natdeep.log。
# 假死后 reset，从盘上读它就是假死瞬间的节点级现场。
#
# systemd-run --unit=natdeep --collect /usr/local/bin/nat-deep.sh
LOG=${NATDEEP_LOG:-/var/log/natdeep.log}
while :; do
    {
        echo "=== $(date -Is)"
        timeout 3 vppctl show errors 2>&1 | grep -vE "^$|Name|Count" | tail -14
        echo "-- nat sessions:"; timeout 3 vppctl show nat44 ei sessions 2>&1 | head -4
        echo "-- host-enp0s9 counters:"; timeout 3 vppctl show interface host-enp0s9 2>&1 | grep -E "rx packets|tx packets|punt|drops|rx-error"
        echo "-- host-enp0s8 counters:"; timeout 3 vppctl show interface host-enp0s8 2>&1 | grep -E "rx packets|tx packets|punt|drops|rx-error"
        echo "-- lcp pairs:"; timeout 3 vppctl show lcp 2>&1 | grep -c itf-pair
        echo "-- kernel addr:"; ip -br addr show vpp0 2>&1; ip -br addr show lcp-x0 2>&1
    } >>"$LOG" 2>&1
    sleep 5
done
