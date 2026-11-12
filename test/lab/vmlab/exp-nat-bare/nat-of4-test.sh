#!/bin/bash
# nat-of4-test.sh -- Test D: dedicated pool address + output interface.
#
# Shape under test (the standard CGNAT shape):
#   - host-enp0s8  : plain inside interface (in2out on input)
#   - host-enp0s9  : OUTPUT interface (postrouting in2out + out2in)
#   - 192.168.1.200: dedicated pool address (secondary address on host-enp0s9,
#                    NOT the interface's own address -- "pool = interface own
#                    address" produced in2out-output-slowpath "out of ports"
#                    907 times in of3, measured 2026-11-12)
# Self-cleans: dumps state, then disables NAT and removes the secondary address.
set -u
LOG=/root/vmlab/nat-of4-test.log
: > "$LOG"
log() { echo "$(date '+%F %T') $*" >> "$LOG"; }
sec() { echo "== $*" >> "$LOG"; }

sec "secondary pool address on host-enp0s9"
vppctl set interface ip address host-enp0s9 192.168.1.200/24 >> "$LOG" 2>&1
echo "secaddr rc=$?" >> "$LOG"

log "probe: -keep -outpostroute -pool 192.168.1.200"
/root/vmlab/nat-probe -keep -repeat 1 -outpostroute -pool 192.168.1.200 >> "$LOG" 2>&1
log "probe rc=$? (NAT on)"

sec "readback: pool"
vppctl show nat44 ei addresses >> "$LOG" 2>&1
sec "readback: interfaces"
vppctl show nat44 ei interfaces >> "$LOG" 2>&1

sec "hold 50s (cust4 ping + Mac ssh window)"
sleep 50

sec "sessions"
vppctl show nat44 ei sessions >> "$LOG" 2>&1
sec "errors"
vppctl show errors >> "$LOG" 2>&1

log "cleanup: disable NAT, drop secondary address"
vppctl nat44 ei plugin disable >> "$LOG" 2>&1
vppctl set interface ip address host-enp0s9 192.168.1.200/24 del >> "$LOG" 2>&1
sleep 2
vppctl nat44 ei plugin disable >> "$LOG" 2>&1
log "of4 done"
