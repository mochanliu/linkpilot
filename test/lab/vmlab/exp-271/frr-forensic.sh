#!/bin/bash
# #271 同口径取证：每盒一份，换件前后各跑一次。只读取，不改任何状态。
B=${1:?CPE1|CPE2|CPE3|CPE4}
S=/run/sr-frr/control.sock
echo "########## $B  $(date -u +%FT%TZ)  ##########"

echo "--- [1] sr-frr /status"
curl -s --unix-socket $S http://x/status | python3 -c '
import json,sys
d=json.load(sys.stdin)
for k in ("zebra_connected","export_vrfs","injected","reported","dropped_loop","dropped_unknown_vrf","skipped","subscriptions","import_rules","last_error","pid"):
    if k in d: print("  %s = %s" % (k, json.dumps(d[k],ensure_ascii=False)))
extra=sorted(set(d)-{"config_hash","control_socket","state_file"}-{k for k in ("zebra_connected","export_vrfs","injected","reported","dropped_loop","dropped_unknown_vrf","skipped","subscriptions","import_rules","last_error","pid")})
if extra: print("  其它字段 =", ",".join(extra))
'

echo "--- [2] sr-frr 账本 state.json（= 已注入内核的那份）"
python3 -c 'import json;print("  "+json.dumps(json.load(open("/var/lib/sr-frr/state.json")).get("routes"),ensure_ascii=False))' 2>/dev/null

echo "--- [3] sr-route /status：local / rib / dropped_own_route / pending"
curl -s http://127.0.0.1:19001/status | python3 -c '
import json,sys
d=json.load(sys.stdin)
print("  local(本盒上报) =", json.dumps(d.get("local"),ensure_ascii=False))
rib=d.get("rib") or {}
for t,rows in sorted(rib.items()):
    print("  rib[%s] %d 条 ="%(t,len(rows)), json.dumps([{ "p":r["prefix"],"nh":r.get("next_hops"),"dev":r.get("device_id")} for r in rows],ensure_ascii=False))
for k in ("dropped_own_route","dropped_no_path","dropped_rt_mismatch","dropped_invalid","withdrawn","imported","received"):
    print("  %s = %s"%(k,d.get(k)))
print("  pending =", json.dumps(d.get("pending"),ensure_ascii=False)[:600])
'

echo "--- [4] 内核路由（VPN0 落 main/254）"
ip route show table main | sed 's/^/  main: /'
for t in 1 2 100 101; do
  ip route show table $t 2>/dev/null | grep -v "^$" | sed "s/^/  table$t: /"
done

echo "--- [5] zebra 视角：172.16.3.1 装在哪、BGP 表里有没有"
vtysh -c "show ip route 172.16.3.1 32" 2>&1 | sed 's/^/  /'
vtysh -c "show ip bgp 172.16.3.1" 2>&1 | sed 's/^/  /' | head -20

echo "--- [6] VPP FIB：172.16.3.1 与 172.16.4.1"
vppctl show ip fib 2>/dev/null | grep -A4 -E "^([0-9.]+ )?172\.16\.[34]\.1/32" | sed 's/^/  /'

echo "--- [7] BGP 邻居 uptime + pid"
vtysh -c "show bgp summary" 2>&1 | sed 's/^/  /'
echo "  bgpd pid=$(pidof bgpd) zebra pid=$(pidof zebra) vpp pid=$(pidof vpp)"

echo "--- [8] sr-frr 最近 30 行日志"
grep -a "sr-frr" /var/log/sr-box/bridge.log 2>/dev/null | tail -30 | sed 's/^/  /'
