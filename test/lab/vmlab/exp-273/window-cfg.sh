#!/bin/bash
# 量「配置热更这一枪」的客户可见时长：探测期间改 prefix-list 的一条目并重新发布，
# 全程不重启任何进程（走 控制台草稿 -> 显式发布 -> agent 推送 -> sr-apply 第 5 段 -> sr-frr PUT /config）。
# 用法：window-cfg.sh <标签> <要加的那条前缀|"RESET"表示回到只有 172.16.12.1/32>
#   会话 cookie 不放仓库：先自建一份 -> curl -c $WORK/cj.txt -X POST http://127.0.0.1:8088/api/login ...
TAG=${1:?标签}
ENTRY=${2:?要加的条目}
WORK=${WORK:-/tmp/p273}
PB=${PB:-cust12}; SRC=${SRC:-172.16.12.1}; DST=${DST:-172.16.3.1}
API=${API:-http://127.0.0.1:8088}
PL=${PL:-pl3}
CJ=${CJ:-$WORK/cj.txt}
LOG=$WORK/window-cfg-$TAG.txt
RAW=$WORK/probe-cfg-$TAG.raw
mkdir -p $WORK

before(){ for B in CPE1 CPE2; do ssh -n -o BatchMode=yes $B "printf '  $B hash='; curl -s --unix-socket /run/sr-frr/control.sock http://x/status | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get(\"config_hash\",\"\")[:12],\"pid=\",d.get(\"pid\"),\"rep=\",d.get(\"reported\"))'"; done; }

echo "== $TAG $(date -u +%FT%TZ) 探测 $PB: -I $SRC -> $DST（改动=node1/node2 的 to_sdwan 白名单条目）" | tee $LOG
before | tee -a $LOG

ssh -n -o BatchMode=yes $PB "end=\$((SECONDS+75)); while [ \$SECONDS -lt \$end ]; do printf \"%s \" \$(date +%s.%3N); timeout 1 ping -c1 -W1 -I $SRC $DST >/dev/null 2>&1 && echo ok || echo FAIL; done" > $RAW 2>/dev/null &
PP=$!
sleep 6
if [ "$ENTRY" = "RESET" ]; then
  BODY="{\"id\":\"$PL\",\"name\":\"本站客户前缀\",\"entries\":[{\"prefix\":\"172.16.12.1/32\"}]}"
else
  BODY="{\"id\":\"$PL\",\"name\":\"本站客户前缀\",\"entries\":[{\"prefix\":\"172.16.12.1/32\"},{\"prefix\":\"$ENTRY\"}]}"
fi
echo "-- 第 6 秒：改 $PL -> $BODY 并重新发布" | tee -a $LOG
curl -s -b $CJ -H 'Content-Type: application/json' -X PUT $API/api/prefix-lists -d "$BODY" \
  | python3 -c 'import json,sys;d=json.load(sys.stdin);print("  PUT 结果 ok=%s entries=%s"%(d.get("ok"),json.dumps((d.get("list") or {}).get("entries"),ensure_ascii=False)))' | tee -a $LOG
curl -s -b $CJ -H 'Content-Type: application/json' -X POST $API/api/leak/republish -d "{\"prefix_list_id\":\"$PL\",\"apply\":true}" \
  | python3 -c 'import json,sys;d=json.load(sys.stdin);print("  发布 blocked=%s results=%s"%(d.get("blocked"),json.dumps(d.get("results"),ensure_ascii=False)))' | tee -a $LOG
wait $PP
python3 - "$TAG" "$WORK" <<'PY' >> $LOG
import sys
tag,work=sys.argv[1],sys.argv[2]
rows=[]
for ln in open(f"{work}/probe-cfg-{tag}.raw"):
    p=ln.split()
    if len(p)==2: rows.append((float(p[0]), p[1].lower()))
fails=[t for t,s in rows if s=="fail"]
print(f"总探测 {len(rows)} 发，FAIL {len(fails)} 发")
if fails:
    runs=[]; st=pre=fails[0]
    for t in fails[1:]:
        if t-pre>2.0: runs.append((st,pre)); st=t
        pre=t
    runs.append((st,pre))
    for a,b in runs: print(f"  断流段 {a:.3f} → {b:.3f}  {(b-a):.1f}s")
    print(f"最长断流 {max(b-a for a,b in runs):.1f}s")
PY
echo "-- 改动后：" | tee -a $LOG
before | tee -a $LOG
ssh -n CPE1 'python3 -c "import json;d=json.load(open(\"/etc/sr-frr.json\"));print(\"  CPE1 落盘 allow=\",json.dumps(d[\"import_rules\"][0][\"prefixes\"],ensure_ascii=False))"' | tee -a $LOG
cat $LOG
