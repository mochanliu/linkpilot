#!/bin/bash
# nat-watch.sh —— NAT 数据面实验的盒内取证器（lab 工具，不进产品）。
#
# 为什么需要它：2026-11-11 的假死现场没有留下任何可判读的证据（没确认 VPP 死活、
# 没看内存/CPU、没有 core）。这个脚本以 2s 粒度记录轻量指标（pid/load/MemAvailable），
# 每 10s 追加一次 VPP 自身视角的快照（main-heap、threads）；任何时刻 `touch
# /tmp/nat-snap` 可以要求下一拍立刻抓一份完整现场（含 NAT 会话表），用于假死前的
# 最后一帧。
#
# 设计约束：
#   - 所有 vppctl 调用都套 timeout，VPP 卡住时不能把取证器一起拖死；
#   - 只用 POSIX 工具 + awk，不依赖 jq/python（假死时也保证能跑）；
#   - 由 systemd-run --unit=natwatch 拉起，脱离 ssh 会话生命周期。
#
# 用法：
#   systemd-run --unit=natwatch --collect /usr/local/bin/nat-watch.sh
#   tail -f /var/log/natwatch.log
#   touch /tmp/nat-snap          # 请求一次完整快照
#   systemctl stop natwatch      # 收工

LOG=${NATWATCH_LOG:-/var/log/natwatch.log}
SNAP=/tmp/nat-snap
TICK=${NATWATCH_TICK:-2}
FULL_EVERY=${NATWATCH_FULL:-10}   # 每 N 拍记一次 heap/threads（2s*10 = 20s）

snapshot() {   # $1 = 触发原因
    {
        echo "--- snapshot $(date -Is) reason=$1"
        echo "## uptime/load: $(uptime)"
        echo "## meminfo:"; grep -E '^(MemTotal|MemFree|MemAvailable|SwapTotal|SwapFree|Committed_AS)' /proc/meminfo
        echo "## vpp pid: $(pidof vpp || echo DEAD)"
        echo "## vpp threads:"; timeout 4 vppctl show threads 2>&1 | head -30
        echo "## vpp memory:"; timeout 4 vppctl show memory main-heap 2>&1 | head -20
        echo "## nat44 interfaces:"; timeout 4 vppctl show nat44 ei interfaces 2>&1 | head -20
        echo "## nat44 sessions:"; timeout 4 vppctl show nat44 ei sessions 2>&1 | head -20
        echo "## kernel softirq:"; grep -E '^(cpu|intr)' /proc/stat | head -2
        echo "## dmesg tail:"; dmesg 2>/dev/null | tail -8
        echo "--- snapshot end"
    } >>"$LOG" 2>&1
}

tick=0
last_pid=0
while :; do
    tick=$((tick + 1))
    ts=$(date +%s)
    pid=$(pidof vpp 2>/dev/null | awk '{print $1}')
    [ -n "$pid" ] || pid=0
    load=$(cut -d' ' -f1-3 /proc/loadavg)
    mem=$(awk '/^MemAvailable/{print $2}' /proc/meminfo)
    swap=$(awk '/^SwapFree/{print $2}' /proc/meminfo)
    line="$ts pid=$pid load=$load memAvailKB=$mem swapFreeKB=$swap"
    if [ "$pid" != 0 ] && [ "$last_pid" != 0 ] && [ "$pid" != "$last_pid" ]; then
        line="$line NOTE=vpp_restarted(old=$last_pid)"
    fi
    last_pid=$pid
    if [ "$pid" != 0 ] && [ $((tick % FULL_EVERY)) -eq 0 ]; then
        heap=$(timeout 4 vppctl show memory main-heap 2>/dev/null | tr -s ' ' | grep -m1 'total:' | cut -c1-140)
        sessions=$(timeout 4 vppctl show nat44 ei sessions 2>/dev/null | grep -c .)
        line="$line heap=$heap natLines=$sessions"
    fi
    echo "$line" >>"$LOG"
    if [ -f "$SNAP" ]; then
        rm -f "$SNAP"
        snapshot on-demand
    fi
    sleep "$TICK"
done
