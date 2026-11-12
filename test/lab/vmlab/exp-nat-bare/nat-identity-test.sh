#!/bin/bash
# nat-identity-test.sh -- Shape G: plain outside + dedicated pool + identity mapping.
#
# What the previous two shapes measured (2026-11-12):
#   Shape E (plain outside, dedicated pool .200):
#     data plane OK (cust4 8/8, 1 session), box traffic OK, but every
#     session-less inbound packet is dropped at nat44-ei-out2in "no translation"
#     (+1795 in 45s) -- including ssh to the box's own WAN address. Management
#     dies for as long as NAT is on.
#   Shape D/of4 (outside as output-interface, dedicated pool):
#     data plane OK, management OK, but box-originated traffic goes through
#     ip4-output nat44-ei-in2out-output and 1603 packets died "out of ports"
#     (port allocation for the box's own sockets) -- that is the overlay's
#     BFD/wg keepalive cadence, i.e. the tunnels would flap.
#
# Shape G keeps the simple arc shape (no ip4-output node) and gives the box's
# own WAN address an address-only IDENTITY mapping:
#   - inbound to .104 reverse-resolves to the identity mapping instead of being
#     dropped as "no translation"
#   - box-originated sockets are never translated (no output feature), and even
#     if they were, an identity mapping needs no port allocation
#   - customer traffic uses the dedicated pool .200 as before
set -u
LOG=/root/vmlab/nat-identity-test.log
: > "$LOG"
log() { echo "$(date '+%H:%M:%S') $*" >> "$LOG"; }
sec() { echo "== $*" >> "$LOG"; }
POOL=192.168.1.200
WANADDR=192.168.1.104

sec "baseline nat44 error counters"
vppctl show errors 2>/dev/null | grep -E 'nat44' >> "$LOG" 2>&1

sec "secondary pool address on host-enp0s9 ($POOL/24)"
vppctl set interface ip address host-enp0s9 "$POOL"/24 >> "$LOG" 2>&1
echo "secaddr rc=$?" >> "$LOG"

log "probe: -keep -pool $POOL -identity $WANADDR (plain inside/outside)"
/root/vmlab/nat-probe -keep -repeat 1 -pool "$POOL" -identity "$WANADDR" >> "$LOG" 2>&1
log "probe rc=$? (NAT on)"

sec "readback: interfaces"
vppctl show nat44 ei interfaces >> "$LOG" 2>&1
sec "readback: pool"
vppctl show nat44 ei addresses >> "$LOG" 2>&1
sec "readback: identity mappings"
vppctl show nat44 ei identity mappings >> "$LOG" 2>&1

sec "hold 60s (cust4 ping + Mac ssh window)"
sleep 60

sec "sessions"
vppctl show nat44 ei sessions >> "$LOG" 2>&1

sec "box-originated path: ping gateway with WAN source"
timeout 12 ping -c3 -W2 -I "$WANADDR" 192.168.1.1 >> "$LOG" 2>&1

sec "nat44 error counters at end (deltas vs baseline)"
vppctl show errors 2>/dev/null | grep -E 'nat44' >> "$LOG" 2>&1

log "cleanup: disable NAT, drop secondary address"
vppctl nat44 ei plugin disable >> "$LOG" 2>&1
vppctl set interface ip address del host-enp0s9 "$POOL"/24 >> "$LOG" 2>&1
sleep 2
vppctl nat44 ei plugin disable >> "$LOG" 2>&1
log "identity test done"
