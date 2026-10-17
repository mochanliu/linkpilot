#!/usr/bin/env bash
# vbond-client.sh -- 在 CPE 容器里手工走 vBOND 协议的客户端（不需要 Go agent）。
#
# 存在的理由有两个：
#   1. 让你手敲几条命令就能复现"入网 → 心跳 → 注册表"，用来定位问题在哪一侧；
#   2. harness 需要一个不掺入 agent 逻辑的第三方证人，才能断言控制器本身的契约。
#
# 身份 = openssl 生成的 Ed25519 密钥对 + 控制器签发的设备证书，
# 落盘在 /opt/vbond/<name>.{key,pem,enroll.json}。
#
# 用法：
#   vbond-client.sh enroll   <name> <claim_code> <serial>
#   vbond-client.sh hb       <name> [node_id_override] [udp_mapping]
#   vbond-client.sh mapping  <name> <ip:port> <local_port> <reflector>
#   vbond-client.sh registry <name>
#   vbond-client.sh watch    <name> [秒数]
#   vbond-client.sh nodeid   <name>
set -u

BASE=${VBOND_BASE:-/opt/vbond}
URL=${VBOND_URL:-https://ctrl:8444}
CA=${VBOND_CA:-$BASE/ca.pem}

cmd=${1:-}
name=${2:-}
[ -n "$cmd" ] && [ -n "$name" ] || { sed -n '2,20p' "$0"; exit 2; }
mkdir -p "$BASE"

KEY="$BASE/$name.key"
CRT="$BASE/$name.pem"
ENR="$BASE/$name.enroll.json"

# jget <file> <dotted.key> —— 从 JSON 里取一个顶层/嵌套字段（没有 jq，用 python3）
jget() { python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
for k in sys.argv[2].split("."):
    d = d[k]
print(d if not isinstance(d,(dict,list)) else json.dumps(d))' "$1" "$2"; }

curl_tls() { # 无客户端证书（入网前）
    curl -sS --max-time 8 --cacert "$CA" "$@"
}
curl_mtls() { # 带设备证书（入网后）
    curl -sS --max-time 8 --cacert "$CA" --cert "$CRT" --key "$KEY" "$@"
}

case "$cmd" in
enroll)
    claim=${3:-}; serial=${4:-}
    [ -f "$KEY" ] || openssl genpkey -algorithm ed25519 -out "$KEY" 2>/dev/null || { echo "enroll: genpkey 失败" >&2; exit 1; }
    pub=$(openssl pkey -pubout -outform DER -in "$KEY" 2>/dev/null | tail -c 32 | base64 -w0)
    [ -n "$pub" ] || { echo "enroll: 取公钥失败" >&2; exit 1; }
    code=$(curl_tls -o "$ENR" -w '%{http_code}' -X POST "$URL/v1/enroll" \
        -d "{\"serial\":\"$serial\",\"claim_code\":\"$claim\",\"public_key\":\"$pub\",\"protocol_ver\":2,\"agent_version\":\"vbond-client\"}")
    echo "http=$code"
    [ "$code" = "200" ] || { echo "enroll 失败，响应体：" >&2; cat "$ENR" >&2; exit 1; }
    python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
open(sys.argv[2],"w").write(d["cert_pem"].replace("\\n","\n"))' "$ENR" "$CRT" 2>/dev/null || { echo "enroll: 响应里没有 cert_pem" >&2; exit 1; }
    echo "tenant=$(jget "$ENR" tenant_id) node_id=$(jget "$ENR" node_id) serial=$serial"
    echo "hb_sec=$(jget "$ENR" heartbeat_sec) opcode_base=$(jget "$ENR" opcode_base) root_key_len=$(python3 -c 'import json,sys;print(len(json.load(open(sys.argv[1]))["root_key"]))' "$ENR")"
    echo "reflectors=$(jget "$ENR" udp_reflector)"
    ;;

hb)
    node=${3:-$(jget "$ENR" node_id)}
    map=${4:-}
    code=$(curl_mtls -o "$BASE/$name.hb.json" -w '%{http_code}' -X POST "$URL/v1/heartbeat" \
        -d "{\"node_id\":$node,\"udp_mapping\":\"$map\",\"uptime_sec\":1}")
    echo "http=$code"
    cat "$BASE/$name.hb.json"
    ;;

mapping)
    m=${3:-}; lp=${4:-0}; refl=${5:-}
    code=$(curl_mtls -o "$BASE/$name.map.json" -w '%{http_code}' -X POST "$URL/v1/mapping" \
        -d "{\"udp_mapping\":\"$m\",\"local_port\":$lp,\"symmetric_nat\":false,\"reflector_addr\":\"$refl\"}")
    echo "http=$code"
    cat "$BASE/$name.map.json"
    ;;

registry)
    curl_mtls -o "$BASE/$name.reg.json" -w 'http=%{http_code}\n' "$URL/v1/registry"
    python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
print("tenant=%s self=%s epoch=%s peers=%d" % (d["tenant_id"], d["self"], d["epoch"], len(d["peers"])))
for p in d["peers"]:
    print("  node=%-3s serial=%-10s map=%-20s reach=%-8s self=%s" %
          (p["node_id"], p.get("serial","?"), p.get("udp_mapping") or "-", p.get("reach","-"), p.get("is_self")))
    print("        srou_ip=%s cert=%s binding=%s" %
          (p.get("srou_ip") or "-", ("有" if p.get("cert_pem") else "无"),
           "有" if p.get("key_binding") else "无"))' "$BASE/$name.reg.json"
    ;;

watch)
    secs=${3:-3}
    curl_mtls -N --max-time "$secs" "$URL/v1/watch" 2>/dev/null |
        python3 -c '
import json,sys
for line in sys.stdin:
    line=line.strip()
    if not line: continue
    e=json.loads(line)
    p=e.get("peer",{})
    print("%-9s tenant=%s node=%s map=%s epoch=%s" %
          (e.get("type"), e.get("tenant_id"), p.get("node_id","-"), p.get("udp_mapping") or "-", e.get("epoch")))'
    ;;

nodeid)
    jget "$ENR" node_id
    ;;

*)
    echo "未知命令：$cmd" >&2
    exit 2
    ;;
esac
