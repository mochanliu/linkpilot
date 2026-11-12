#!/bin/bash
# nat-phase-split.sh -- which step costs the management path: the pool address
# or the NAT enable itself?
#
# Shape E run (2026-11-12 17:24) carried the data plane (cust4 8/8, 1 session)
# and box-originated traffic (3/3), but the Mac lost ssh for ~30s right at the
# start and got it back at t=+35s. Two candidate causes, and they are 45s
# apart in this script so the Mac-side ssh timeline can attribute the window:
#   phase 1 (t=0)    : add 192.168.1.200/24 as a secondary address on the
#                      outside interface (linux-cp mirrors it into vpp0)
#   phase 2 (t=45)   : nat-probe -keep -pool 192.168.1.200 (NAT on)
set -u
LOG=/root/vmlab/nat-phase-split.log
: > "$LOG"
log() { echo "$(date '+%H:%M:%S') $*" >> "$LOG"; }
POOL=192.168.1.200

log "start; addresses before:"
vppctl show interface address host-enp0s9 >> "$LOG" 2>&1

log "PHASE1 add $POOL/24 (no NAT yet)"
vppctl set interface ip address host-enp0s9 "$POOL"/24 >> "$LOG" 2>&1
echo "phase1 rc=$?" >> "$LOG"
sleep 45

log "PHASE2 nat-probe -keep -pool $POOL"
/root/vmlab/nat-probe -keep -repeat 1 -pool "$POOL" >> "$LOG" 2>&1
echo "phase2 rc=$?" >> "$LOG"
sleep 45

log "cleanup"
vppctl nat44 ei plugin disable >> "$LOG" 2>&1
vppctl set interface ip address del host-enp0s9 "$POOL"/24 >> "$LOG" 2>&1
sleep 1
vppctl nat44 ei plugin disable >> "$LOG" 2>&1
log "done; addresses after:"
vppctl show interface address host-enp0s9 >> "$LOG" 2>&1
