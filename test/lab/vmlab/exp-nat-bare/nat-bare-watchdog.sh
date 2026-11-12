#!/bin/bash
# nat-bare-watchdog.sh -- NAT bare-control experiment safety net (runs ON the box).
#
# Why: the defect we are testing kills ALL packets crossing the NAT interfaces,
# including the management path (ssh rides kernel vpp0 TAP <-> lcp <-> af_packet
# host-enp0s9). If the bare VPP reproduces the wedge, ssh is gone and only the
# VBox console can recover. This watchdog runs detached (systemd-run) BEFORE
# NAT is enabled, so recovery does not depend on the network being alive.
#
# Contract: after ARM_SECONDS it unconditionally runs `nat44 ei plugin disable`
# (twice, belt and braces), UNLESS /root/vmlab/nat-bare-keep exists -- the Mac
# driver touches that file when the operator wants NAT to stay up for more
# tests. Every recovery dumps full node-level evidence to the log first
# (show errors / nat44 sessions / interfaces / features verbose), so even a
# wedged run leaves its autopsy on disk.
#
# All vppctl output goes through the local unix/cli socket -- in the defect VPP
# stays alive and answers locally even with the network dead (measured
# 2026-11-12, nat-deep.sh).
set -u
LOG=/root/vmlab/nat-bare-watchdog.log
KEEP=/root/vmlab/nat-bare-keep
ARM_SECONDS=${ARM_SECONDS:-180}
: > "$LOG"
log() { echo "$(date '+%F %T') $*" >> "$LOG"; }

log "armed: auto-disable in ${ARM_SECONDS}s unless $KEEP exists"
sleep "$ARM_SECONDS"

if [ -e "$KEEP" ]; then
    log "keep file present -- leaving NAT alone (operator's call)"
    exit 0
fi

log "TRIGGER: dumping evidence then disabling NAT"
{
    echo '== date'; date
    echo '== show errors'; timeout 10 vppctl show errors | head -80
    echo '== nat44 ei sessions'; timeout 10 vppctl show nat44 ei sessions | head -20
    echo '== nat44 ei interfaces'; timeout 10 vppctl show nat44 ei interfaces | head -10
    echo '== interface counters'; timeout 10 vppctl show interface | head -40
    echo '== features host-enp0s9'; timeout 10 vppctl show features verbose host-enp0s9 | head -60
} >> "$LOG" 2>&1

timeout 10 vppctl nat44 ei plugin disable >> "$LOG" 2>&1
sleep 2
timeout 10 vppctl nat44 ei plugin disable >> "$LOG" 2>&1
log "recovery done; disabling again returned: see lines above"
