#!/bin/bash
# run.sh -- NAT bare-control experiment driver (runs on the Mac, drives CPE4 by ssh).
#
# Question it answers: is the NAT "all packets die" defect a property of the
# stock nat44-ei plugin + stock config path on this box (bare), or an
# interaction with OUR plugin set / config path (sr_wg, CLI-vs-API)?
#
# Phase matrix (only one variable differs per row; heap stays 256M everywhere
# because the API path proved explicit sizing fits, and CLI gets the same
# explicit counts):
#
#   bare    : sr_wg_plugin.so DISABLED in startup, NAT via stock CLI
#   withwg  : sr_wg_plugin.so enabled again (everything else identical), NAT via stock CLI
#   restore : original startup conf byte-identical, NAT off, vpp restarted
#
# Safety: before NAT is enabled each phase, nat-bare-watchdog.sh is deployed
# and started via systemd-run (survives ssh loss); it auto-disables NAT after
# ARM_SECONDS (default 180s) and dumps node-level evidence to
# /root/vmlab/nat-bare-watchdog.log. To keep NAT up past the arm window the
# operator runs `run.sh keep` (creates /root/vmlab/nat-bare-keep) and
# `run.sh unkeep` afterwards.
#
# Traffic target: 192.168.1.1 = the WAN-side gateway (online, same L2 as the
# WAN). NAT pool = host-enp0s9's own address (nat44 ei add interface address),
# so replies route back to CPE4. cust4 behind 172.31.4.0/24 is the inside client.
#
# PASS criteria (node-based, same as the 2026-11-12 forensic round):
#   - sessions > 0   (a translation was actually created)
#   - >= 4/5 echo replies from the gateway through NAT
#   - ssh to CPE4 stays alive the whole time
set -u -o pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
RAW=$HERE/raw
mkdir -p "$RAW"
STAMP=$(date +%Y%m%d-%H%M%S)
SSHO="-o BatchMode=yes -o ConnectTimeout=8 -o LogLevel=ERROR"
BOX=CPE4
CUST=cust4
GW=192.168.1.1
STARTUP=/root/vmlab/startup-vmlab.conf
BACKUP=/root/vmlab/startup-vmlab.conf.pre-nat-bare

# one ssh, strip CR, keep exit honest (vm-verify lesson: tr eats the rc)
run() { local h=$1 out rc; shift
    out=$(ssh -n $SSHO "$h" "$*" 2>&1 | tr -d '\r'); rc=$?
    printf '%s\n' "$out" | grep -v '^Warning: Permanently added'
    return $rc; }
vppc() { run "$BOX" "vppctl $*"; }
note() { printf '.... %s\n' "$*"; }

die() { printf '\033[31mFAIL\033[0m %s\n' "$*" >&2; exit 1; }

# wait for VPP to answer AND the underlay takeover to be done again
# (lcp pairs + LAN addr on TAP lcp-x0). Bounded, loud on timeout.
wait_vpp_ready() {
    local i
    for i in $(seq 1 60); do
        if vppc show version >/dev/null 2>&1; then break; fi
        sleep 2
    done
    vppc show version >/dev/null 2>&1 || die "vppctl 120s 不回话"
    for i in $(seq 1 60); do
        local pairs addr
        pairs=$(vppc show lcp 2>/dev/null | grep -c '^itf-pair')
        addr=$(run "$BOX" "ip -o -4 addr show dev lcp-x0 2>/dev/null" | grep -c '172.31.4.1/24')
        if [ "$pairs" -ge 2 ] && [ "$addr" -ge 1 ]; then
            note "underlay 就绪：lcp pairs=$pairs, lcp-x0 地址在"
            return 0
        fi
        sleep 2
    done
    die "underlay 120s 内没收口（lcp pairs / lcp-x0 地址缺）"
}

# startup conf surgery: $1 = disable|keep-wg
set_sr_wg() {
    run "$BOX" "test -f $BACKUP || cp -p $STARTUP $BACKUP"
    local plugins_block
    plugins_block=$(run "$BOX" "sed -n '/^plugins {/,/^}/p' $STARTUP")
    printf '%s\n' "$plugins_block" | grep -q '^plugins {' || die "startup 里没找到 plugins 段"
    case "$1" in
        disable)
            # no ^ anchor: the plugin lines are indented, the 2026-11-12 first
            # attempt anchored on ^ and silently never matched -- the readback
            # below is the guard, never trust the edit (AGENTS.md §2)
            printf '%s\n' "$plugins_block" | grep -q 'sr_wg_plugin.so { disable }' || \
                run "$BOX" "sed -i 's|plugin linux_nl_plugin.so { enable }|plugin linux_nl_plugin.so { enable }\\n  plugin sr_wg_plugin.so { disable }|' $STARTUP"
            ;;
        keep-wg)
            run "$BOX" "sed -i '/sr_wg_plugin.so { disable }/d' $STARTUP"
            ;;
    esac
    # read back what actually landed, and HARD FAIL if it is not there
    printf '%s\n' "$(run "$BOX" "sed -n '/^plugins {/,/^}/p' $STARTUP")"
    case "$1" in
        disable)
            run "$BOX" "grep -q 'sr_wg_plugin.so { disable }' $STARTUP" || die "disable 行没写进 startup，拒绝继续（2026-11-12 教训）"
            ;;
    esac
}

restart_vpp() {
    note "systemctl restart vpp（underlay 由 ExecStartPost 自动重跑）"
    run "$BOX" "sudo systemctl restart vpp" || die "vpp restart 失败"
    wait_vpp_ready
}

# the stray sr-agent (not under systemd, hand-started Nov 11) re-applies the
# overlay (wg1010/1011 ...) after every vpp restart -- it must be out of the
# picture while we bisect the plugin set. argv saved to
# /root/vmlab/sr-agent-cmdline.txt before killing; restore phase relaunches it.
agent_control() { # stop|start
    case "$1" in
        stop)
            run "$BOX" "pgrep -x sr-agent >/dev/null && { pgrep -x sr-agent | xargs tr '\\0' ' ' < /proc/\$(pgrep -ox sr-agent)/cmdline > /root/vmlab/sr-agent-cmdline.txt 2>/dev/null || true; pkill -x sr-agent; sleep 1; }; pgrep -x sr-agent >/dev/null && echo still-running || echo stopped"
            ;;
        start)
            run "$BOX" "pgrep -x sr-agent >/dev/null && echo already-running || { nohup \$(cat /root/vmlab/sr-agent-cmdline.txt) >/var/log/sr-agent-relaunch.log 2>&1 & sleep 2; pgrep -x sr-agent >/dev/null && echo relaunched || echo relaunch-failed; }"
            ;;
    esac
}

# node-level evidence snapshot into raw/
evidence() { # <label>
    local f=$RAW/$1-$STAMP.txt
    {
        echo "== $(date '+%F %T') phase=$1"
        echo '== show version';            vppc show version
        echo '== show plugins (sr/wg/nat)';vppc show plugins | grep -iE 'sr_wg|srmpls|nat|wg_plugin'
        echo '== features host-enp0s8';    vppc show features verbose host-enp0s8
        echo '== features host-enp0s9';    vppc show features verbose host-enp0s9
        echo '== nat44 ei interfaces';     vppc show nat44 ei interfaces
        echo '== nat44 ei sessions';       vppc show nat44 ei sessions
        echo '== nat44 ei addresses';      vppc show nat44 ei addresses
        echo '== errors (nat/drop tail)';  vppc show errors | grep -iE 'nat|drop' | tail -30
        echo '== lcp pairs';               vppc show lcp
        echo '== abf';                     vppc show abf
        echo '== on-box watchdog log';     run "$BOX" "cat /root/vmlab/nat-bare-watchdog.log 2>/dev/null"
    } > "$f" 2>&1
    note "evidence: $f"
}

deploy_safety() {
    scp -q $SSHO "$HERE/nat-bare-watchdog.sh" "$BOX:/tmp/nat-bare-watchdog.sh"
    run "$BOX" "install -m 0755 /tmp/nat-bare-watchdog.sh /root/vmlab/nat-bare-watchdog.sh && rm -f /root/vmlab/nat-bare-keep /root/vmlab/nat-bare-watchdog.log"
    note "watchdog 已部署（install -m 0755），ARM_SECONDS=${ARM_SECONDS:-180}"
}

arm_watchdog() {
    run "$BOX" "rm -f /root/vmlab/nat-bare-keep; systemd-run --unit nat-bare-wd-\$\$ /root/vmlab/nat-bare-watchdog.sh" \
        || die "watchdog 拉起失败"
    note "watchdog 已武装：${ARM_SECONDS:-180}s 后自动 nat44 ei plugin disable（除非 keep）"
}

nat_on_via_cli() {
    # stock CLI only -- the whole point of the bare phase
    note "NAT 使能（纯 CLI，显式尺寸：users 4096 / user-sessions 256 / sessions 16384）"
    vppc "nat44 ei plugin enable users 4096 user-sessions 256 sessions 16384" \
        || die "nat44 ei plugin enable 失败"
    vppc "set interface nat44 ei in host-enp0s8 out host-enp0s9" \
        || die "set interface nat44 ei 失败"
    vppc "nat44 ei add interface address host-enp0s9" \
        || die "nat44 ei add interface address 失败"
    note "readback:"; vppc "show nat44 ei interfaces"; vppc "show nat44 ei addresses"
}

nat_off_via_cli() {
    vppc "nat44 ei plugin disable" >/dev/null 2>&1
    sleep 1
    vppc "nat44 ei plugin disable" >/dev/null 2>&1
    note "NAT 已关"
}

baseline_no_nat() { # <tag>
    note "基线（NAT 未开）：cust4 -> 172.31.4.1（内腿）"
    run "$CUST" "ping -c3 -W2 172.31.4.1 | tail -2"
    note "基线（NAT 未开）：cust4 ->  ${GW}（回腿预期不通：网关没有 172.31.4.0/24 路由，但能证明出向腿活着）"
    local tx0 tx1
    tx0=$(vppc show interface host-enp0s9 | awk '/packets/{print $0; exit}' | head -1)
    run "$CUST" "ping -c3 -W2 $GW | tail -2"
    tx1=$(vppc show interface host-enp0s9 | awk '/packets/{print $0; exit}' | head -1)
    note "WAN 口计数：[$tx0] -> [$tx1]（涨了说明包确实从 WAN 出去了）"
}

nat_traffic_test() { # <tag>
    note "NAT 流量测试：cust4 -> $GW ping -c5"
    run "$CUST" "ping -c5 -W2 $GW | tail -3"
    local sess
    sess=$(vppc "show nat44 ei sessions" | grep -c . || true)
    echo "SESSIONS_ROWS=$sess"
    evidence "$1"
    # judgment
    local verdict=BROKEN
    if [ "$sess" -gt 0 ]; then
        local replies
        replies=$(run "$CUST" "ping -c5 -W2 $GW 2>/dev/null | grep -c 'ttl=' || true")
        if [ "$replies" -ge 4 ]; then verdict=PASS; fi
    fi
    printf '\033[36mRESULT %s: %s (sessions_rows=%s)\033[0m\n' "$1" "$verdict" "$sess"
    [ "$verdict" = PASS ]
}

case "${1:-}" in
    snapshot)
        evidence pre-snapshot
        run "$BOX" "cat $STARTUP" | sed "s/^/[startup] /"
        ;;
    bare)
        deploy_safety
        agent_control stop
        note "== 阶段1：bare（禁 sr_wg_plugin.so，无 sr-agent，其余原样）=="
        set_sr_wg disable
        restart_vpp
        vppc show plugins | grep -q sr_wg_plugin && die "sr_wg 仍然加载着，bare 阶段不成立"
        note "确认 sr_wg_plugin.so 未加载"
        baseline_no_nat
        arm_watchdog
        nat_on_via_cli
        sleep 3
        note "ssh 存活检查"; run "$BOX" "echo alive"
        nat_traffic_test bare || true
        nat_off_via_cli
        ;;
    withwg)
        deploy_safety
        agent_control stop
        note "== 阶段2：withwg（sr_wg_plugin.so 回来，仍无 sr-agent，其余与 bare 完全一致）=="
        set_sr_wg keep-wg
        restart_vpp
        vppc show plugins | grep -q sr_wg_plugin || die "sr_wg 没加载上，withwg 阶段不成立"
        note "确认 sr_wg_plugin.so 已加载"
        baseline_no_nat
        arm_watchdog
        nat_on_via_cli
        sleep 3
        note "ssh 存活检查"; run "$BOX" "echo alive"
        nat_traffic_test withwg || true
        nat_off_via_cli
        ;;
    keep)    run "$BOX" "touch /root/vmlab/nat-bare-keep"; note "keep 文件已放，watchdog 不会动 NAT" ;;
    unkeep)  run "$BOX" "rm -f /root/vmlab/nat-bare-keep"; note "keep 文件已撤" ;;
    nat-off) nat_off_via_cli ;;
    restore)
        note "== 恢复：原 startup conf + 重启 vpp =="
        run "$BOX" "test -f $BACKUP && cp -p $BACKUP $STARTUP && echo restored || die-no-backup"
        nat_off_via_cli
        restart_vpp
        agent_control start
        note "恢复后体检（cust4 内腿 + 路由表形状）"
        run "$CUST" "ping -c3 -W2 172.31.4.1 | tail -2"
        vppc show lcp
        ;;
    status)
        run "$BOX" "systemctl is-active vpp; cat /root/vmlab/nat-bare-watchdog.log 2>/dev/null | tail -20; vppctl show nat44 ei sessions | head -5"
        ;;
    *) echo "用法: run.sh snapshot|bare|withwg|keep|unkeep|nat-off|restore|status"; exit 2 ;;
esac
