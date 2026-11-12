#!/bin/bash
# nat-of2-test.sh -- decisive test: plain in/out flags + postrouting in2out
# (nat44_ei_add_del_output_interface) on the OUTSIDE interface ONLY.
#
# Expected if the postrouting hypothesis is right:
#   - cust4 -> gateway pings get replies (in2out on inside still works)
#   - sessions created (cust4 + box-originated)
#   - management (ssh from Mac) stays alive the whole time
# Self-cleans: dumps state at ~t+50s, then disables NAT.
set -u
LOG=/root/vmlab/nat-of2-test.log
: > "$LOG"
log() { echo "$(date '+%F %T') $*" >> "$LOG"; }
sec() { echo "== $*" >> "$LOG"; }

log "of2 start: probe with -keep -outpostroute (API path, outside-only postrouting)"
/root/vmlab/nat-probe -keep -repeat 1 -outpostroute >> "$LOG" 2>&1
rc=$?
log "probe rc=$rc (NAT left on for the traffic window)"

sec "hold 50s (cust4 delayed ping + Mac ssh probe window)"
sleep 50

sec "sessions"
vppctl show nat44 ei sessions >> "$LOG" 2>&1
sec "interfaces"
vppctl show nat44 ei interfaces >> "$LOG" 2>&1
sec "errors"
vppctl show errors >> "$LOG" 2>&1
sec "interface counters"
vppctl show interface host-enp0s8 host-enp0s9 >> "$LOG" 2>&1
sec "VPP alive locally"
vppctl show version >> "$LOG" 2>&1

log "disabling NAT"
vppctl nat44 ei plugin disable >> "$LOG" 2>&1
sleep 2
vppctl nat44 ei plugin disable >> "$LOG" 2>&1
log "of2 done"
