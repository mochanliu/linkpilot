#!/bin/bash
# 200ms 一发，两个口径：
#   local=N  sr-route /status 的 local 条数（= sr-frr 每轮 PUT 上去的那份，本盒对外宣称的客户侧前缀集）
#   frr=M true  sr-frr /status 的 reported 与 zebra_connected
# 在盒子本地跑，避免把 ssh 往返算进采样间隔。用法：sample-local.sh [总秒数=70]
end=$((SECONDS + ${1:-70}))
while [ $SECONDS -lt $end ]; do
  n=$(curl -s --max-time 0.4 http://127.0.0.1:19001/status |
    python3 -c 'import json,sys;print(len(json.load(sys.stdin).get("local") or []))' 2>/dev/null)
  f=$(curl -s --max-time 0.4 --unix-socket /run/sr-frr/control.sock http://x/status |
    python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get("reported"),d.get("zebra_connected"))' 2>/dev/null)
  printf '%s local=%s frr(%s)\n' "$(date +%s.%3N)" "${n:-ERR}" "${f:-ERR}"
  sleep 0.2
done
