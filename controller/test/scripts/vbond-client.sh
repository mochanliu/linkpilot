#!/usr/bin/env bash
# vbond-client.sh -- 在 CPE 容器里手工走 vBOND 协议的客户端（不需要 Go agent）。
#
# 存在的理由有两个：
#   1. 让你手敲几条命令就能复现"入网 → 心跳 → 注册表 → 观测回报"，用来定位问题在哪一侧；
#   2. harness 需要一个不掺入 agent 逻辑的第三方证人，才能断言控制器本身的契约。
#
# 身份 = openssl 生成的 Ed25519 密钥对 + 控制器签发的设备证书，
# 落盘在 /opt/vbond/<name>.{key,pem,enroll.json}。
#
# 用法：
#   vbond-client.sh enroll   <name> <claim_code> <serial>
#   vbond-client.sh bind     <name> [tenant] [node_id] [key_gen]
#   vbond-client.sh hb       <name> [node_id_override] [udp_mapping]
#   vbond-client.sh mapping  <name> <ip:port> <local_port> <reflector> [local_addrs_csv]
#   vbond-client.sh observe  <name> <reset:0|1> <src_ip:src_port:dst_port[:verified]> ...
#   vbond-client.sh registry <name>
#   vbond-client.sh watch    <name> [秒数]
#   vbond-client.sh nodeid   <name>
#
# bind 的两个覆盖参数是给"伪造"留的门：tenant/node_id 默认取入网响应里的值，
# 显式传别的值就得到一个签名正确、但覆盖对象不对的 binding —— 用来证明控制器
# 真的在校验，而不是看有没有签名字段。
set -u

BASE=${VBOND_BASE:-/opt/vbond}
URL=${VBOND_URL:-https://ctrl:8444}
CA=${VBOND_CA:-$BASE/ca.pem}
# 协议版本：控制器只谈 wire.ProtocolVersion。留着这个开关是为了能证明"旧版本被明确拒绝"
# 而不是"悄悄接受然后行为不对"。
PROTVER=${VBOND_PROTVER:-3}

cmd=${1:-}
name=${2:-}
[ -n "$cmd" ] && [ -n "$name" ] || { awk 'NR>1 { if ($0=="set -u") exit; sub(/^# ?/,""); print }' "$0"; exit 2; }
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
        -d "{\"serial\":\"$serial\",\"claim_code\":\"$claim\",\"public_key\":\"$pub\",\"protocol_ver\":$PROTVER,\"agent_version\":\"vbond-client\"}")
    echo "http=$code"
    [ "$code" = "200" ] || { echo "enroll 失败，响应体：" >&2; cat "$ENR" >&2; exit 1; }
    python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
open(sys.argv[2],"w").write(d["cert_pem"].replace("\\n","\n"))' "$ENR" "$CRT" 2>/dev/null || { echo "enroll: 响应里没有 cert_pem" >&2; exit 1; }
    echo "tenant=$(jget "$ENR" tenant_id) node_id=$(jget "$ENR" node_id) serial=$serial"
    echo "hb_sec=$(jget "$ENR" heartbeat_sec) vpn_port=$(jget "$ENR" vpn_port) transit_port=$(jget "$ENR" transit_port)"
    python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
for k in d["p_keys"]:
    print("p_key hmac_id=%s key_hex_len=%d" % (k["hmac_id"], len(k["key"])))' "$ENR"
    echo "reflectors=$(jget "$ENR" udp_reflector)"
    ;;

bind)
    # 用设备身份私钥（Ed25519）担保一把 X25519 公钥 —— 那就是这台设备的 WireGuard
    # 静态公钥。签名覆盖的字节必须与 internal/keybinding.Message 逐字节一致，
    # 否则控制器验不过；所以这里独立于 Go 代码用 openssl+python 再实现一遍。
    tenant=${3:-$(jget "$ENR" tenant_id)}
    node=${4:-$(jget "$ENR" node_id)}
    gen=${5:-1}
    [ -n "$tenant" ] && [ "$tenant" != "None" ] || { echo "bind: 还没入网，不知道 tenant" >&2; exit 1; }
    [ -f "$BASE/$name.x25519.key" ] || openssl genpkey -algorithm x25519 -out "$BASE/$name.x25519.key" 2>/dev/null
    xpub=$(openssl pkey -pubout -outform DER -in "$BASE/$name.x25519.key" 2>/dev/null | tail -c 32 | base64 -w0)
    [ -n "$xpub" ] || { echo "bind: 取 X25519 公钥失败（openssl 版本不支持 x25519？）" >&2; exit 1; }
    python3 -c '
import base64,struct,sys
tenant, node, gen, pub, out = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), sys.argv[4], sys.argv[5]
m = (b"srou-keybinding-v1"
     + struct.pack(">H", len(tenant)) + tenant.encode()
     + struct.pack(">H", node) + struct.pack(">I", gen)
     + base64.b64decode(pub))
open(out, "wb").write(m)' "$tenant" "$node" "$gen" "$xpub" "$BASE/$name.bind.msg" || exit 1
    openssl pkeyutl -sign -inkey "$KEY" -rawin -in "$BASE/$name.bind.msg" \
        -out "$BASE/$name.bind.sig" || { echo "bind: 签名失败" >&2; exit 1; }
    python3 -c '
import base64,json,sys
sig = base64.b64encode(open(sys.argv[1],"rb").read()).decode()
json.dump({"x25519_pub": sys.argv[2], "key_gen": int(sys.argv[3]), "sig": sig},
          open(sys.argv[4], "w"))' "$BASE/$name.bind.sig" "$xpub" "$gen" "$BASE/$name.bind.json" || exit 1
    echo "bound tenant=$tenant node=$node key_gen=$gen x25519=$xpub"
    ;;

hb)
    node=${3:-$(jget "$ENR" node_id)}
    map=${4:-}
    body=$(python3 -c '
import json,os,sys
b={"node_id":int(sys.argv[1]),"udp_mapping":sys.argv[2],"uptime_sec":1}
if os.path.exists(sys.argv[3]): b["key_binding"]=json.load(open(sys.argv[3]))
print(json.dumps(b))' "$node" "$map" "$BASE/$name.bind.json")
    code=$(curl_mtls -o "$BASE/$name.hb.json" -w '%{http_code}' -X POST "$URL/v1/heartbeat" -d "$body")
    echo "http=$code"
    cat "$BASE/$name.hb.json"
    ;;

mapping)
    m=${3:-}; lp=${4:-0}; refl=${5:-}; addrs=${6:-}
    body=$(python3 -c '
import json,os,sys
b={"udp_mapping":sys.argv[1],"local_port":int(sys.argv[2]),
   "symmetric_nat":False,"reflector_addr":sys.argv[3]}
a=[x for x in sys.argv[4].split(",") if x]
if a: b["local_addrs"]=a
if os.path.exists(sys.argv[5]): b["key_binding"]=json.load(open(sys.argv[5]))
print(json.dumps(b))' "$m" "$lp" "$refl" "$addrs" "$BASE/$name.bind.json")
    code=$(curl_mtls -o "$BASE/$name.map.json" -w '%{http_code}' -X POST "$URL/v1/mapping" -d "$body")
    echo "http=$code"
    cat "$BASE/$name.map.json"
    ;;

observe)
    # 观测回报：本端数据面"收到过的包的外层源"。格式 src_ip:src_port:dst_port[:verified]
    reset=${3:-0}
    shift 3
    body=$(python3 -c '
import json,os,sys
rows=[]
for a in sys.argv[3:]:
    f=a.split(":")
    if len(f) < 3: continue
    rows.append({"src_ip":f[0],"src_port":int(f[1]),"dst_port":int(f[2]),
                 "verified": len(f)>3 and f[3]=="1"})
b={"node_id":int(sys.argv[1]),"reset":sys.argv[2]=="1","observations":rows}
print(json.dumps(b))' "$(jget "$ENR" node_id)" "$reset" "$@")
    code=$(curl_mtls -o "$BASE/$name.obs.json" -w '%{http_code}' -X POST "$URL/v1/observe" -d "$body")
    echo "http=$code"
    python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
print("accepted=%d epoch=%s" % (d.get("accepted",0), d.get("epoch")))
for a in d.get("attributed") or []:
    print("  %-22s -> node %-3s %s" % (a["mapping"], a["node_id"], a["reason"]))
for a in d.get("unattributed") or []:
    print("  %-22s -> ?      %s" % (a["mapping"], a["reason"]))' "$BASE/$name.obs.json" 2>/dev/null \
        || { echo "响应体：" >&2; cat "$BASE/$name.obs.json" >&2; }
    ;;

registry)
    curl_mtls -o "$BASE/$name.reg.json" -w 'http=%{http_code}\n' "$URL/v1/registry"
    python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
print("tenant=%s self=%s epoch=%s peers=%d" % (d["tenant_id"], d["self"], d["epoch"], len(d["peers"])))
for p in d["peers"]:
    print("  node=%-3s serial=%-10s map=%-20s ports=%s/%s reach=%-8s self=%s" %
          (p["node_id"], p.get("serial","?"), p.get("udp_mapping") or "-",
           p.get("vpn_port") or "-", p.get("transit_port") or "-",
           p.get("reach","-"), p.get("is_self")))
    # seen=是"我"观测到的那个源（按读者渲染），不是这一行的固有属性
    print("        seen=%s verified=%s cert=%s binding=%s" %
          (p.get("observed_mapping") or "-", p.get("observed_verified"),
           ("有" if p.get("cert_pem") else "无"),
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
    print("%-9s tenant=%s node=%s map=%s seen=%s epoch=%s" %
          (e.get("type"), e.get("tenant_id"), p.get("node_id","-"),
           p.get("udp_mapping") or "-", p.get("observed_mapping") or "-", e.get("epoch")))'
    ;;

nodeid)
    jget "$ENR" node_id
    ;;

*)
    echo "未知命令：$cmd" >&2
    exit 2
    ;;
esac
