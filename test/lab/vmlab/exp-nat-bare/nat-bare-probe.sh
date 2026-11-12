#!/bin/bash
# nat-bare-probe.sh -- self-contained NAT data-plane probe, runs ON the box.
#
# Why self-contained: enabling nat44-ei on these interfaces kills the
# management path within 1-2s (measured 2026-11-12 four times, incl. a
# stock-plugin/stock-CLI bare run), so a test driven over ssh cannot observe
# the data plane. The Mac driver arms a delayed ping on cust4 (the real
# customer VM) BEFORE this runs; this probe enables NAT, waits through the
# customer-traffic window, then captures sessions/errors -- and disables NAT
# again so the management path returns without needing the watchdog.
#
# Inside source: cust4 (172.31.4.2) -> host-enp0s8, the NAT inside interface.
# Outside target: 192.168.1.1 = the live WAN gateway.
#
# What the log has to answer:
#   1. does in2out create a session for customer->gateway traffic at all
#   2. does the customer see replies (data plane works, or not)
#   3. which node drops what, before/after the customer window
#   4. arc order: dumped once right after enable (ordering is the evidence)
set -u
LOG=/root/vmlab/nat-bare-probe.log
KEEP=/root/vmlab/nat-bare-keep
: > "$LOG"
log() { echo "$(date '+%F %T') $*" >> "$LOG"; }
sec() { echo "== $*" >> "$LOG"; }

log "probe start (single-thread VPP; stock nat44-ei; CLI config)"

sec "enable NAT via CLI (users 4096 / user-sessions 256 / sessions 16384)"
{
    vppctl nat44 ei plugin enable users 4096 user-sessions 256 sessions 16384
    echo "enable rc=$?"
    vppctl set interface nat44 ei in host-enp0s8 out host-enp0s9
    echo "iface rc=$?"
    vppctl nat44 ei add interface address host-enp0s9
    echo "pool rc=$?"
} >> "$LOG" 2>&1
sleep 2

sec "readback"
{ vppctl show nat44 ei interfaces; vppctl show nat44 ei addresses; } >> "$LOG" 2>&1

sec "arc order: ip4-unicast on host-enp0s9 (full dump)"
vppctl show features verbose host-enp0s9 > /root/vmlab/nat-bare-arc.txt 2>&1
log "arc dump: $(wc -l < /root/vmlab/nat-bare-arc.txt) lines -> /root/vmlab/nat-bare-arc.txt"

sec "errors right after enable"
vppctl show errors >> "$LOG" 2>&1

sec "customer window: wait 45s (cust4 delayed ping fires in here)"
log "waiting 45s for cust4 traffic"
sleep 45

sec "sessions after customer traffic"
vppctl show nat44 ei sessions >> "$LOG" 2>&1
vppctl show nat44 ei interfaces >> "$LOG" 2>&1

sec "errors after customer traffic"
vppctl show errors >> "$LOG" 2>&1

sec "interface counters (drop ratio on the two NAT ports)"
vppctl show interface host-enp0s8 host-enp0s9 >> "$LOG" 2>&1

sec "VPP still alive locally? (vppctl answers = yes)"
vppctl show version >> "$LOG" 2>&1

if [ -e "$KEEP" ]; then
    log "keep file present -- leaving NAT on"
    exit 0
fi
log "disabling NAT (no keep file) so the management path comes back"
vppctl nat44 ei plugin disable >> "$LOG" 2>&1
sleep 2
vppctl nat44 ei plugin disable >> "$LOG" 2>&1
log "probe done"
