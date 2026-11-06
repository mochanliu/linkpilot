#!/bin/bash
# 三态（A=带注入闸门的旧产物 / B=闸门撤掉后 / C=闸门撤掉 + 配置收口后）同一次读数。只读，不动任何状态。
# 用法：box-layers.sh <CPE1|CPE2|CPE3|CPE4>
B=${1:?CPE1|CPE2|CPE3|CPE4}
S=/run/sr-frr/control.sock
echo "########## $B  $(date -u +%FT%TZ) ##########"
echo "--- sr-frr /status"
ssh -n $B "curl -s --unix-socket $S http://x/status" | python3 -c '
import json,sys
d=json.load(sys.stdin)
for k in ("zebra_connected","injected","skipped","reported","dropped_loop","dropped_unknown_vrf","local_withheld","export_frozen","export_frozen_held","subscriptions","last_error","pid"):
    if k in d: print("  %s = %s"%(k,json.dumps(d[k],ensure_ascii=False)))
print("  import_rules =",json.dumps(d.get("import_rules"),ensure_ascii=False))
'
echo "--- sr-frr 账本（已注入内核的那份）"
ssh -n $B 'python3 -c "import json;print(\"  \"+json.dumps(json.load(open(\"/var/lib/sr-frr/state.json\")).get(\"routes\"),ensure_ascii=False))"' 2>/dev/null
echo "--- sr-route local / rib / dropped_own_route"
ssh -n $B 'curl -s http://127.0.0.1:19001/status' | python3 -c '
import json,sys
d=json.load(sys.stdin)
print("  local =",json.dumps(d.get("local"),ensure_ascii=False))
for t,rows in sorted((d.get("rib") or {}).items()):
    print("  rib[%s] ="%t, json.dumps([{"p":r["prefix"],"nh":r.get("next_hops"),"dev":r.get("device_id")} for r in rows],ensure_ascii=False))
print("  dropped_own_route =",d.get("dropped_own_route"))
'
echo "--- 落盘的 sr-frr 配置（第 5 段）"
ssh -n $B 'python3 -c "
import json
d=json.load(open(\"/etc/sr-frr.json\"))
print(\"  export_vrfs =\",json.dumps(d.get(\"export_vrfs\"),ensure_ascii=False))
for r in d.get(\"import_rules\",[]):
    print(\"  import vpn%s sources=%s allow=%s\"%(r.get(\"vpn\"),r.get(\"sources\"),json.dumps(r.get(\"prefixes\"),ensure_ascii=False)))
"'
echo "--- 内核 main 表 + zebra 视角"
ssh -n $B 'ip route show table main | sed "s/^/  main: /"; for p in 172.16.3.1/32 172.16.4.1/32 172.16.12.1/32; do echo "  == $p"; vtysh -c "show ip route $p" 2>&1 | sed "s/^/    /" | head -8; done; echo "  --- redistribute 可见集来源统计"; vtysh -c "show ip route summary" 2>&1 | sed "s/^/    /"'
