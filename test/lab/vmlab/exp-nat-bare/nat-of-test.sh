#!/bin/bash
# nat-of-test.sh -- does output-feature keep the management path alive?
# Runs detached on the box (systemd-run) so a broken ssh never kills it.
#
# Sequence: enable NAT -> interfaces WITH output-feature -> pool -> dumps ->
# hold 90s (Mac probes ssh + cust4 ping in this window) -> disable.
# Everything lands in /root/vmlab/nat-of-test.log.
set -u
LOG=/root/vmlab/nat-of-test.log
: > "$LOG"
log() { echo "$(date '+%F %T') $*" >> "$LOG"; }
sec() { echo "== $*" >> "$LOG"; }

log "of-test start"
sec "enable"
{
    vppctl nat44 ei plugin enable users 4096 user-sessions 256 sessions 16384; echo "enable rc=$?"
    vppctl set interface nat44 ei in host-enp0s8 out host-enp0s9 output-feature; echo "iface+of rc=$?"
    vppctl nat44 ei add interface address host-enp0s9; echo "pool rc=$?"
} >> "$LOG" 2>&1
sleep 2

sec "readback: interfaces"
vppctl show nat44 ei interfaces >> "$LOG" 2>&1
sec "readback: output features on host-enp0s9 (ip4-output arc)"
vppctl show interface features host-enp0s9 >> "$LOG" 2>&1
sec "readback: arc dump"
vppctl show features verbose host-enp0s9 > /root/vmlab/nat-of-arc.txt 2>&1
log "arc: $(wc -l < /root/vmlab/nat-of-arc.txt) lines"

sec "hold 90s (customer ping + Mac ssh probe window)"
sleep 90

sec "sessions after window"
vppctl show nat44 ei sessions >> "$LOG" 2>&1
sec "errors after window"
vppctl show errors >> "$LOG" 2>&1
sec "VPP alive locally"
vppctl show version >> "$LOG" 2>&1

log "disabling NAT"
vppctl nat44 ei plugin disable >> "$LOG" 2>&1
sleep 2
vppctl nat44 ei plugin disable >> "$LOG" 2>&1
log "of-test done"
