#!/bin/bash
# nat-plainpool-test.sh -- Shape E: plain inside + plain outside + dedicated pool.
#
# Why this shape and not of4's output-interface one: of4 (inside plain + outside
# nat44_ei_add_del_output_interface + dedicated pool) carried the data plane
# (cust4 8/8, sessions built) and kept management alive, but box-originated
# traffic died at in2out-output-slowpath with "out of ports" 1603 times. The
# box has no business being translated at all: its WAN address is a real address
# with a return route, so translating it only creates a second identity that
# can fail. The only thing that actually killed management in Test A was
# pool == the box's own WAN address (out2in "no translation"), so a dedicated
# pool with BOTH interfaces in their plain roles should be enough.
#
# Shape under test:
#   host-enp0s8  plain inside  (in2out on input)
#   host-enp0s9  plain outside (out2in on input)  <-- NOT an output interface
#   192.168.1.200 dedicated pool address, secondary on host-enp0s9
#
# Self-cleans: dumps state, then disables NAT and removes the secondary address.
set -u
LOG=/root/vmlab/nat-plainpool-test.log
: > "$LOG"
log() { echo "$(date '+%F %T') $*" >> "$LOG"; }
sec() { echo "== $*" >> "$LOG"; }
POOL=192.168.1.200
WANADDR=192.168.1.104

sec "secondary pool address on host-enp0s9 ($POOL/24)"
vppctl set interface ip address host-enp0s9 "$POOL"/24 >> "$LOG" 2>&1
echo "secaddr rc=$?" >> "$LOG"

log "probe: -keep -pool $POOL (plain inside/outside, NO outpostroute)"
/root/vmlab/nat-probe -keep -repeat 1 -pool "$POOL" >> "$LOG" 2>&1
log "probe rc=$? (NAT on)"

sec "readback: interfaces"
vppctl show nat44 ei interfaces >> "$LOG" 2>&1
sec "readback: pool"
vppctl show nat44 ei addresses >> "$LOG" 2>&1

sec "hold 60s (cust4 ping + Mac ssh window)"
sleep 60

sec "sessions"
vppctl show nat44 ei sessions >> "$LOG" 2>&1

sec "box-originated path: ping gateway with WAN source"
timeout 12 ping -c3 -W2 -I "$WANADDR" 192.168.1.1 >> "$LOG" 2>&1

sec "errors"
vppctl show errors >> "$LOG" 2>&1

log "cleanup: disable NAT, drop secondary address"
vppctl nat44 ei plugin disable >> "$LOG" 2>&1
vppctl set interface ip address host-enp0s9 "$POOL"/24 del >> "$LOG" 2>&1
sleep 2
vppctl nat44 ei plugin disable >> "$LOG" 2>&1
log "plainpool done"
