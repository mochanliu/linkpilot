#!/bin/bash
# test-route.sh - 路由控制面：两个 RR + 三个 CPE 的二进制级 e2e（任务 #34/#35）
#
# 这条脚本跑的是**真二进制**（cmd/sr-rr、cmd/sr-route）和**真 mTLS**
# （自签租户 CA，CPE 证书带 srou://tenant-a/node/N 身份），不是进程内假端。
# 它回答四件事：
#
#   (A) 默认反射：CPE1 的路由经 RR 到达与它有 VPN 隧道（topology 里配的）的
#       CPE2，且 CPE2 的 import RT 有交集才进 bgp 表。
#   (B) RT import 过滤：CPE3 的路由到了 CPE1 的邻居（拓扑上有隧道），但
#       export RT 与 CPE1 vrf5 的 import RT 没有交集 ⇒ 不进表，且
#       dropped_rt_mismatch 计数增加（"没收到"和"收到了但拒收"必须分得开）。
#   (C) 反射策略：reflect_to {"2":[3]} 把 CPE2 的反射目标从拓扑默认的 1
#       换成 3 —— CPE3 收到 CPE2 的路由，CPE1 收不到。
#   (D) 双 RR 主备：杀掉首选 RR 后三个 CPE 都切到 rr-b（active_rr=1）且表不
#       变；首选 RR 回来后切回（active_rr=0）。
#
# 不碰 VPP、不碰数据面：这个增量只验证路由协议本身。FIB 的下发是下一步。
#
# 用法（主机上，容器在跑）:
#   test/scripts/test-route.sh
# 也可以直接进容器跑:
#   docker exec srwg-node1 bash -lc '/workspace/test/scripts/test-route.sh'
set -u

# 在主机上跑时把自己送进 srwg-node1：整个场景只需要 loopback 和 Go 工具链。
if [ ! -d /workspace/controller ]; then
    exec docker exec -i srwg-node1 bash -lc "cd /workspace && test/scripts/test-route.sh"
fi

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "${HERE}/.." && pwd)
MODULE=${MODULE:-/workspace/controller}
WORK=${WORK:-/tmp/sr-route-e2e}
KEEP=${KEEP:-0}

RR_A_PORT=${RR_A_PORT:-18446}
RR_A_STATUS=${RR_A_STATUS:-18447}
RR_B_PORT=${RR_B_PORT:-18456}
RR_B_STATUS=${RR_B_STATUS:-18457}
N1_STATUS=${N1_STATUS:-18461}
N2_STATUS=${N2_STATUS:-18462}
N3_STATUS=${N3_STATUS:-18463}

PASS=0
FAIL=0
ok()   { PASS=$((PASS+1)); echo "  [PASS] $*"; }
bad()  { FAIL=$((FAIL+1)); echo "  [FAIL] $*"; }
info() { echo "== $*"; }
die()  { echo "FATAL: $*" >&2; cleanup; exit 2; }

PIDS=""

cleanup() {
    for p in $PIDS; do
        kill "$p" 2>/dev/null || true
    done
    for p in $PIDS; do
        wait "$p" 2>/dev/null || true
    done
    PIDS=""
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# run_json <url> <python expr over d>：expr 里用 d，返回 0 表示成立。
json_ok() {
    curl -sf --max-time 2 "$1" |
        python3 -c "import json,sys; d=json.load(sys.stdin); sys.exit(0 if ($2) else 1)" 2>/dev/null
}

wait_status() {   # <url> <expr> <label> [timeout_s]
    local url=$1 expr=$2 label=$3 limit=${4:-20} i=0
    while [ "$i" -lt $((limit*5)) ]; do
        if json_ok "$url" "$expr"; then ok "$label"; return 0; fi
        sleep 0.2
        i=$((i+1))
    done
    bad "$label (超时)"
    echo "    最后状态：" >&2
    curl -sf --max-time 2 "$url" | python3 -m json.tool 2>/dev/null | head -40 >&2 || true
    return 1
}

assert_status() {   # <url> <expr> <label>
    if json_ok "$1" "$2"; then ok "$3"; else
        bad "$3"
        echo "    最后状态：" >&2
        curl -sf --max-time 2 "$1" | python3 -m json.tool 2>/dev/null | head -40 >&2 || true
    fi
}

# ---------------------------------------------------------------- 环境与构建

info "准备 $WORK 并编译二进制"
rm -rf "$WORK"
mkdir -p "$WORK/bin" "$WORK/pki"
(cd "$MODULE" && go build -o "$WORK/bin/sr-rr" ./cmd/sr-rr) || die "编译 sr-rr 失败"
(cd "$MODULE" && go build -o "$WORK/bin/sr-route" ./cmd/sr-route) || die "编译 sr-route 失败"

cd "$WORK/pki" || die "进 $WORK/pki 失败"

gen_key() { openssl genpkey -algorithm ED25519 -out "$1" 2>/dev/null || die "生成 $1 失败"; }

info "生成租户 CA、RR 服务端证书、CPE 客户端证书（URI SAN = 身份）"
gen_key ca.key
openssl req -x509 -new -key ca.key -days 30 -subj "/CN=sr-route-test-ca" -out ca.crt 2>/dev/null || die "CA 失败"

gen_key rr.key
openssl req -new -key rr.key -subj "/CN=rr" -out rr.csr 2>/dev/null || die "RR CSR 失败"
cat > server.ext <<'EOF'
basicConstraints=CA:FALSE
keyUsage=digitalSignature
extendedKeyUsage=serverAuth
subjectAltName=IP:127.0.0.1,DNS:localhost
EOF
openssl x509 -req -in rr.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 30 \
    -extfile server.ext -out rr.crt 2>/dev/null || die "RR 证书签发失败"

for n in 1 2 3; do
    d="$WORK/identity/node$n"
    mkdir -p "$d"
    gen_key "$d/device_ed25519.key"
    openssl req -new -key "$d/device_ed25519.key" -subj "/CN=node$n" -out "node$n.csr" 2>/dev/null
    cat > client.ext <<EOF
basicConstraints=CA:FALSE
keyUsage=digitalSignature
extendedKeyUsage=clientAuth
subjectAltName=URI:srou://tenant-a/node/$n
EOF
    openssl x509 -req -in "node$n.csr" -CA ca.crt -CAkey ca.key -CAcreateserial -days 30 \
        -extfile client.ext -out "$d/node_cert.pem" 2>/dev/null || die "node$n 证书签发失败"
    cp ca.crt "$d/ca_cert.pem"
    cat > "$d/state.json" <<EOF
{"serial":"node$n","key_gen":0,"tenant_id":"tenant-a","node_id":$n}
EOF
done

# ---------------------------------------------------------------- 配置

info "写配置（拓扑 1:[2,3]，策略 reflect_to {\"2\":[3]}）"
cat > "$WORK/rr-a.json" <<EOF
{
  "rr_id": "rr-a",
  "tenant": "tenant-a",
  "hold_sec": 6,
  "listen": "127.0.0.1:$RR_A_PORT",
  "status_addr": "127.0.0.1:$RR_A_STATUS",
  "tls": {"cert_file": "$WORK/pki/rr.crt", "key_file": "$WORK/pki/rr.key", "ca_file": "$WORK/pki/ca.crt"},
  "topology": {"1": [2, 3]},
  "reflect_to": {"2": [3]}
}
EOF
sed -e 's/"rr-a"/"rr-b"/' \
    -e "s/127.0.0.1:$RR_A_PORT/127.0.0.1:$RR_B_PORT/" \
    -e "s/127.0.0.1:$RR_A_STATUS/127.0.0.1:$RR_B_STATUS/" \
    "$WORK/rr-a.json" > "$WORK/rr-b.json"

# node1: 枢纽，import 65000:6；node2: export 65000:6（默认应被 1 收，但策略把它
# 改送到 3）；node3: export 65000:7（能到 1，但 RT 不匹配 ⇒ 必须被拒收）。
write_node() {   # <n> <overlays-json> <prefix> <export> <import-csv> <status-port>
    local n=$1 overlays=$2 prefix=$3 export=$4 imports=$5 status=$6
    cat > "$WORK/node$n.json" <<EOF
{
  "tenant": "tenant-a",
  "device_id": $n,
  "identity_dir": "$WORK/identity/node$n",
  "serial": "node$n",
  "hold_sec": 6,
  "rrs": [
    {"id": "rr-a", "addr": "127.0.0.1:$RR_A_PORT"},
    {"id": "rr-b", "addr": "127.0.0.1:$RR_B_PORT"}
  ],
  "overlay_addrs": $overlays,
  "vrfs": [
    {"id": 5, "export_rts": ["$export"], "import_rts": [$imports]}
  ],
  "local_prefixes": [{"vrf_id": 5, "prefix": "$prefix"}],
  "status_addr": "127.0.0.1:$status"
}
EOF
}
write_node 1 '["192.168.99.1"]' 10.10.0.0/16 65000:5 '"65000:6"' "$N1_STATUS"
write_node 2 '["192.168.99.2"]' 10.20.0.0/16 65000:6 '"65000:5"' "$N2_STATUS"
# node3 同时 import 65000:5（收 node1）与 65000:6（收策略改送过来的 node2）。
# 两个 overlay：一条路由带两个下一跳，验证 v2 的集合形状真的在线上跑。
write_node 3 '["192.168.99.3", "192.168.88.3"]' 10.30.0.0/16 65000:7 '"65000:5", "65000:6"' "$N3_STATUS"

# ---------------------------------------------------------------- 启动

start() {   # <bin> <config> <log>
    "$WORK/bin/$1" -config "$2" >"$3" 2>&1 &
    PIDS="$PIDS $!"
    echo $!
}

info "启动两个 RR 与三个 CPE"
RR_A=$(start sr-rr "$WORK/rr-a.json" "$WORK/rr-a.log")
RR_B=$(start sr-rr "$WORK/rr-b.json" "$WORK/rr-b.log")
N1=$(start sr-route "$WORK/node1.json" "$WORK/node1.log")
N2=$(start sr-route "$WORK/node2.json" "$WORK/node2.log")
N3=$(start sr-route "$WORK/node3.json" "$WORK/node3.log")

URL_RR_A="http://127.0.0.1:$RR_A_STATUS/status"
URL_N1="http://127.0.0.1:$N1_STATUS/status"
URL_N2="http://127.0.0.1:$N2_STATUS/status"
URL_N3="http://127.0.0.1:$N3_STATUS/status"

# ---------------------------------------------------------------- 断言

info "(A) RR 收齐三家的宣告（device_id / vrf / next_hops / RT 原样保留）"
wait_status "http://127.0.0.1:$RR_A_STATUS/status" \
    "set(d['routes'])=={'1','2','3'} and all(v==1 for v in d['routes'].values()) and all(s['synced'] for s in d['sessions']) and len(d['sessions'])==3" \
    "rr-a 有 3 个 synced 会话、每家 1 条 adj-rib-in" 20
assert_status "http://127.0.0.1:$RR_A_STATUS/routes" \
    "[r for r in d['1'] if r['device_id']==1 and r['next_hops']==['192.168.99.1'] and len(r['rts'])==1]" \
    "rr-a：device 1 的宣告带本端 overlay 与 RT"
assert_status "http://127.0.0.1:$RR_A_STATUS/routes" \
    "[r for r in d['2'] if r['device_id']==2 and r['next_hops']==['192.168.99.2'] and len(r['rts'])==1]" \
    "rr-a：device 2 的宣告带本端 overlay 与 RT"
assert_status "http://127.0.0.1:$RR_A_STATUS/routes" \
    "len(d['3'])==1 and d['3'][0]['next_hops']==['192.168.88.3', '192.168.99.3'] and len(d['3'][0]['rts'])==1" \
    "rr-a：device 3 用一条路由带两个 overlay（不是两条）"

info "(A) 默认反射 + RT 匹配：node1 路由进入 node2 的 bgp 表"
wait_status "$URL_N2" \
    "d['active_rr']==0 and [ (r['prefix'], r['next_hops']) for r in d['rib'].get('5',[]) ] == [('10.10.0.0/16',['192.168.99.1'])]" \
    "node2 vrf5 = {10.10.0.0/16 via 192.168.99.1}" 25

info "(B) RT import 过滤：node3 的 65000:7 到了 node1 但不进表"
wait_status "$URL_N1" "d['active_rr']==0 and d['dropped_rt_mismatch']>=1" \
    "node1 拒收了一条 RT 不匹配的路由" 25
assert_status "$URL_N1" "d['rib'].get('5',[])==[]" \
    "node1 vrf5 为空（没有可导入的路由，而不是没收到）"
assert_status "$URL_N1" "d['received']>=1" \
    "node1 确实收到了 RR 发来的路由（received>0）"

info "(C) 反射策略：node2 的路由只送到 node3，不送到 node1"
wait_status "$URL_N3" \
    "d['active_rr']==0 and sorted((r['prefix'], r['next_hops']) for r in d['rib'].get('5',[])) == [('10.10.0.0/16',['192.168.99.1']), ('10.20.0.0/16',['192.168.99.2'])]" \
    "node3 同时收到 node1（拓扑）与 node2（策略）" 25
assert_status "$URL_N1" "not [r for r in d['rib'].get('5',[]) if r['prefix']=='10.20.0.0/16']" \
    "node1 没收到 node2 的路由（策略替换了拓扑目标）"

info "(D) 主备切换：杀掉 rr-a，三个 CPE 切到 rr-b 且表不变"
kill "$RR_A" 2>/dev/null || true
for u in "$URL_N1:node1" "$URL_N2:node2" "$URL_N3:node3"; do
    url=${u%:*}; who=${u##*:}
    wait_status "$url" "d['active_rr']==1 and d['sessions'][0]['connected']==False" \
        "$who 切到 rr-b" 15
done
assert_status "$URL_N2" \
    "[ (r['prefix'], r['next_hops']) for r in d['rib'].get('5',[]) ] == [('10.10.0.0/16',['192.168.99.1'])]" \
    "切到 rr-b 后 node2 的表不变"
assert_status "$URL_N3" \
    "sorted(r['prefix'] for r in d['rib'].get('5',[])) == ['10.10.0.0/16','10.20.0.0/16']" \
    "切到 rr-b 后 node3 的表不变"

info "(D) 首选 RR 回来：切回 rr-a"
RR_A=$(start sr-rr "$WORK/rr-a.json" "$WORK/rr-a.log.2")
for u in "$URL_N1:node1" "$URL_N2:node2" "$URL_N3:node3"; do
    url=${u%:*}; who=${u##*:}
    wait_status "$url" "d['active_rr']==0 and d['sessions'][0]['synced']==True" \
        "$who 切回 rr-a" 20
done

info "RR 会话数在切换后仍然正确（没有僵尸会话）"
wait_status "http://127.0.0.1:$RR_A_STATUS/status" \
    "len(d['sessions'])==3 and all(s['synced'] for s in d['sessions'])" \
    "重启后的 rr-a 重新收齐 3 个会话" 20

# ---------------------------------------------------------------- 收尾

echo
echo "== 结果：$PASS passed, $FAIL failed"
if [ "$FAIL" -ne 0 ]; then
    echo "== 日志（最后 20 行）：" >&2
    for f in "$WORK"/*.log*; do
        echo "---- $f" >&2
        tail -20 "$f" >&2
    done
    exit 1
fi
if [ "$KEEP" = "1" ]; then
    echo "== 保留现场：$WORK"
else
    cleanup
    rm -rf "$WORK"
fi
