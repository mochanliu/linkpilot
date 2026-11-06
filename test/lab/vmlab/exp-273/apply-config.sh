#!/bin/bash
# 态 C：走产品路径（控制台草稿 -> 显式发布 -> agent 推送 -> sr-apply 第 5 段热更 -> sr-frr PUT /config）
# 只做 owner 点名的那一句：CPE1 / CPE2 只对 RR 播自己客户侧的路由。
# 会话 cookie 不放仓库：先自建 -> curl -c $WORK/cj.txt -X POST http://127.0.0.1:8088/api/login -d '{...}'
WORK=${WORK:-/tmp/p273}
CJ=${CJ:-$WORK/cj.txt}
API=${API:-http://127.0.0.1:8088}
export WORK CJ API
mkdir -p $WORK && cd $WORK || exit 1

echo "== 1) 新建 prefix-list：只放本站自己客户侧的 172.16.12.1/32"
curl -s -b $CJ -H 'Content-Type: application/json' -X POST $API/api/prefix-lists \
  -d '{"name":"本站客户前缀","entries":[{"prefix":"172.16.12.1/32"}]}' \
  > pl-create.json
python3 - <<'PY'
import json, os
d=json.load(open(os.environ.get('WORK','/tmp/p273')+'/pl-create.json'))
pl=d.get('list') or {}
print('  id=%s name=%s entries=%s' % (pl.get('id'),pl.get('name'),json.dumps(pl.get('entries'),ensure_ascii=False)))
if not pl.get('id'):
    print('  响应：', json.dumps(d,ensure_ascii=False)[:400])
PY
PL=$(python3 -c "import json,os;d=json.load(open(os.environ.get('WORK','/tmp/p273')+'/pl-create.json'));print((d.get('list') or {}).get('id',''))")
[ -z "$PL" ] && { echo "  没拿到 list id，停止"; exit 1; }
echo "  PL=$PL"

echo "== 2) 把 node1 / node2 的 CPE-SDWAN（to_sdwan）行指向它"
for N in 1 2; do
  curl -s -b $CJ -H 'Content-Type: application/json' -X PUT $API/api/leak/rows \
    -d "{\"node_id\":$N,\"direction\":\"to_sdwan\",\"vpn\":0,\"protocols\":[\"ospf\"],\"prefix_list\":\"$PL\"}" \
    > row-$N.json
  python3 -c "
import json,os;d=json.load(open(os.environ.get('WORK','/tmp/p273')+'/row-$N.json'))
r=d.get('rule')
print('  node$N ->', json.dumps(r,ensure_ascii=False) if r else json.dumps(d,ensure_ascii=False)[:300])
"
done

echo "== 3) 预览发布（apply=false）"
curl -s -b $CJ -H 'Content-Type: application/json' -X POST $API/api/leak/republish \
  -d "{\"prefix_list_id\":\"$PL\",\"apply\":false}" > repub-preview.json
python3 - <<'PY'
import json,os
d=json.load(open(os.environ.get('WORK','/tmp/p273')+'/repub-preview.json'))
print('  blocked=%s' % d.get('blocked'))
for r in d.get('Rows',d.get('rows',[])):
    print('   node %s will_publish=%s reason=%s' % (r.get('node_id'),r.get('will_publish'),r.get('reason')))
    for c in (r.get('checks') or []):
        print('      [%s] %s' % (c.get('severity'),c.get('text')))
PY

echo "== 4) 真正发布（apply=true）"
curl -s -b $CJ -H 'Content-Type: application/json' -X POST $API/api/leak/republish \
  -d "{\"prefix_list_id\":\"$PL\",\"apply\":true}" > repub-apply.json
python3 - <<'PY'
import json,os
d=json.load(open(os.environ.get('WORK','/tmp/p273')+'/repub-apply.json'))
print('  blocked=%s' % d.get('blocked'))
for r in d.get('results',[]):
    print('   node %s published=%s version=%s' % (r.get('node_id'),r.get('published'),r.get('version')))
PY
