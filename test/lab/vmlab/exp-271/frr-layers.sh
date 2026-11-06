#!/bin/bash
# #271 三条前缀在每盒的四层落点：内核 / zebra / VPP FIB / sr-route RIB。换件前后同一口径。
B=${1:?CPE1|CPE2|CPE3|CPE4}
echo "########## $B $(date -u +%FT%TZ) ##########"
RIB=$(curl -s http://127.0.0.1:19001/status)
for P in 172.16.12.1 172.16.3.1 172.16.4.1; do
  echo "=== $P/32"
  echo "  [内核 main] $(ip route show table main $P/32 | tr '\n' ' ')"
  echo "  [zebra]     $(vtysh -c "show ip route $P/32" 2>/dev/null | sed -n '2,3p' | tr -s ' ' | tr '\n' '|')"
  echo "  [VPP FIB]   $(vppctl show ip fib $P/32 2>/dev/null | grep -E "ipv4 via|mpls via|drop" | head -2 | sed 's/^ *//' | tr '\n' '|')"
  echo "  [sr-route]  $(echo "$RIB" | python3 -c "
import json,sys
p='$P/32'
d=json.load(sys.stdin)
rows=[r for t,v in (d.get('rib') or {}).items() for r in v if r['prefix']==p]
print('rib=', json.dumps([{ 'dev':r.get('device_id'),'nh':r.get('next_hops')} for r in rows],ensure_ascii=False), '| local=', p in [x['prefix'] for x in (d.get('local') or [])])
")"
done
curl -s --unix-socket /run/sr-frr/control.sock http://x/status > /tmp/srfrr_status.json
echo "  [sr-frr 账本] $(python3 -c 'import json;print(",".join(r["prefix"] for r in json.load(open("/var/lib/sr-frr/state.json")).get("routes",[])))' 2>/dev/null)"
echo "  [sr-frr 状态] $(python3 /tmp/frr-status-line.py 2>&1 | tail -1)"
