#!/bin/bash
# test-fib.sh - 控制面路由落到 VPP，并随 BFD 增删（任务 #38/#39/#40）
#
# 这条脚本把三段真东西接在一起，回答"路由真的在转发吗"：
#
#   (A) 真 RR + 两个真 sr-route：node2 宣告 10.20.0.0/16，经 RR 反射进 node1
#       的 RIB（sr-route 的 /status 就是 FIB 加载器的输入）。
#   (B) 真 sr-fib + 真 VPP：node1 的 VPP 里应出现 VRF 250 的 10.20.0.0/16，
#       出口是 wg0、压标签 266（= 16 + VRF 250），并登记本地标签 266。
#   (C) 真 BFD（node2 是真容器）：两端会话 Up 时路由保留；对端撤会话、本端
#       Down 后该下一跳的路径从 FIB 消失（标签保留）；对端恢复后路径回来。
#       最后杀掉 node2 的 sr-route，让控制面撤销这条路由，FIB 也必须跟着撤。
#
# 为什么可达性走本机 BFD 而不是北向：BFD 会话就在这台 CPE 的 VPP 里，状态是
# 最新的；北向那份是上一个上报窗口的快照。两者不一致时，转发该信本机。
#
# 前置：**必须先跑过 test/scripts/test-e2e.sh --keep**（wg0@51820 单隧道已通，
# 三个容器在跑，node1/node2 的 overlay 互相可达）。脚本只在 node1 的 /tmp 下
# 放文件与进程，收尾时用 `sr-fib -reset` 把 VRF 250 整份 FIB 状态撤掉。
#
# 用法:
#   test/scripts/test-fib.sh
set -u

N1=srwg-node1
N2=srwg-node2
WG_IF=${WG_IF:-wg0}
OVERLAY_N1=${OVERLAY_N1:-192.168.99.1}
OVERLAY_N2=${OVERLAY_N2:-192.168.99.2}

MODULE=/workspace/controller
WORK=${WORK:-/tmp/sr-fib-e2e}

VRF=${VRF:-250}
LABEL=$((16 + VRF))
REMOTE_PFX=${REMOTE_PFX:-10.20.0.0/16}   # node2 宣告、node1 要装进 VRF 的前缀
LOCAL_PFX=${LOCAL_PFX:-10.10.0.0/16}     # node1 自己的前缀（只走控制面，不进 VRF）

RR_PORT=${RR_PORT:-18546}
RR_STATUS=${RR_STATUS:-18547}
N1_STATUS=${N1_STATUS:-18561}
N2_STATUS=${N2_STATUS:-18562}

BFD_TX=${BFD_TX:-50000}
BFD_RX=${BFD_RX:-50000}
BFD_MULT=${BFD_MULT:-3}

PASS=0
FAIL=0
ok()   { PASS=$((PASS+1)); echo "  [PASS] $*"; }
bad()  { FAIL=$((FAIL+1)); echo "  [FAIL] $*"; }
info() { echo "== $*"; }
die()  { echo "FATAL: $*" >&2; exit 2; }

dx()  { docker exec "$1" bash -lc "$2"; }
vpp() { docker exec "$1" bash -lc "vppctl $2" 2>/dev/null | tr -d '\r'; }

# ---------------------------------------------------------------- 清理

kill_one() {   # <name> -> 停 WORK/<name>.pid
    dx "$N1" "if [ -f $WORK/$1.pid ]; then kill \$(cat $WORK/$1.pid) 2>/dev/null || true; rm -f $WORK/$1.pid; fi" >/dev/null 2>&1
}
bfd_del() {    # <container> <local> <peer>
    vpp "$1" "bfd udp session del multihop local-addr $2 peer-addr $3" >/dev/null 2>&1 || true
}
cleanup() {
    # 先停 sr-fib：它还在收敛的话会跟 reset 抢同一张表。
    kill_one fib
    kill_one node1
    kill_one node2
    kill_one rr
    sleep 0.3
    bfd_del "$N1" "$OVERLAY_N1" "$OVERLAY_N2"
    bfd_del "$N2" "$OVERLAY_N2" "$OVERLAY_N1"
    dx "$N1" "if [ -x $WORK/bin/sr-fib ]; then $WORK/bin/sr-fib -config $WORK/fib.json -reset; fi" >/dev/null 2>&1 || true
    dx "$N1" "rm -rf $WORK" >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# ---------------------------------------------------------------- 断言工具

vpp_has_table() { vpp "$N1" "show ip fib table $VRF" | grep -q "^ipv4-VRF:$VRF"; }
fib_route_present() { vpp "$N1" "show ip fib table $VRF" | grep -q "^$REMOTE_PFX"; }
fib_route_absent()  { ! fib_route_present; }
label_present()     { vpp "$N1" "show mpls fib" | grep -q "^$LABEL:"; }
label_absent()      { ! label_present; }
table_absent()      { ! vpp_has_table; }

# 路由必须带着 VPN 标签走 wg0 出去，而不只是"前缀在表里"。
# `mpls-label[@N]` 里的 N 是 VPP 内部的 label-stack 下标：首次安装实测是 @1，
# 撤掉重装后又是 @0，所以只认下标存在、不认具体值。
fib_path_labeled() {
    local out
    out=$(vpp "$N1" "show ip fib table $VRF")
    printf '%s\n' "$out" | grep -A8 "^$REMOTE_PFX" | grep -qE "mpls-label\[@[0-9]+\]:\[$LABEL:" &&
        printf '%s\n' "$out" | grep -A8 "^$REMOTE_PFX" | grep -qF "mpls via $OVERLAY_N2 $WG_IF"
}

wait_fn() {   # <label> <timeout_s> <fn>
    local label=$1 limit=$2 fn=$3 i=0
    while [ "$i" -lt $((limit * 5)) ]; do
        if "$fn"; then ok "$label"; return 0; fi
        sleep 0.2
        i=$((i + 1))
    done
    bad "$label (超时)"
    vpp "$N1" "show ip fib table $VRF" | head -14 >&2
    return 1
}

status_json() { dx "$N1" "curl -sf --max-time 2 http://127.0.0.1:$1/status"; }
wait_json() {   # <port> <python expr over d> <label> [timeout_s]
    local port=$1 expr=$2 label=$3 limit=${4:-20} i=0
    while [ "$i" -lt $((limit * 5)) ]; do
        if status_json "$port" |
            python3 -c "import json,sys; d=json.load(sys.stdin); sys.exit(0 if ($expr) else 1)" 2>/dev/null; then
            ok "$label"
            return 0
        fi
        sleep 0.2
        i=$((i + 1))
    done
    bad "$label (超时)"
    status_json "$port" | python3 -m json.tool 2>/dev/null | head -30 >&2
    return 1
}

bfd_add() {   # <container> <local> <peer>
    vpp "$1" "bfd udp session add multihop local-addr $2 peer-addr $3 desired-min-tx $BFD_TX required-min-rx $BFD_RX detect-mult $BFD_MULT" >/dev/null
}
bfd_wait_up() {   # 状态只能从 API dump 读（CLI 只打印计数），借 Go 探针。
    dx "$N1" "cd $MODULE && SRWG_VPP_SOCK=/run/vpp/api.sock \
        SRWG_OVERLAY_LOCAL=$OVERLAY_N1 SRWG_OVERLAY_PEER=$OVERLAY_N2 \
        SRWG_BFD_WANT=up SRWG_BFD_WAIT_MS=6000 \
        go test ./internal/vppapi -run TestLiveBFDState -count=1 -v" 2>&1 |
        grep -q "bfd state=up"
}

# ---------------------------------------------------------------- 0 前置

info "0. 前置：wg0 与 overlay 通路（需要先跑 test-e2e.sh --keep）"
[ -n "$(vpp "$N1" "show interface $WG_IF" | awk -v n="$WG_IF" '$1==n {print $2; exit}')" ] ||
    die "$N1 没有 $WG_IF：先跑 test/scripts/test-e2e.sh --keep"
[ -n "$(vpp "$N2" "show interface $WG_IF" | awk -v n="$WG_IF" '$1==n {print $2; exit}')" ] ||
    die "$N2 没有 $WG_IF：先跑 test/scripts/test-e2e.sh --keep"
vpp "$N1" "ping $OVERLAY_N2 repeat 3 interval 0.2" | grep -q '0% packet loss' ||
    die "node1 -> node2 overlay 不通"
vpp "$N2" "ping $OVERLAY_N1 repeat 3 interval 0.2" | grep -q '0% packet loss' ||
    die "node2 -> node1 overlay 不通"
ok "wg0 存在，overlay 双向可达"

# ---------------------------------------------------------------- 1 准备

info "1. 在 $N1 准备 $WORK（PKI、配置、二进制）"
dx "$N1" "rm -rf $WORK && mkdir -p $WORK/bin $WORK/pki $WORK/identity" || die "建 $WORK 失败"

docker exec -i -e WORK="$WORK" -e RR_PORT="$RR_PORT" -e RR_STATUS="$RR_STATUS" \
    -e N1_STATUS="$N1_STATUS" -e N2_STATUS="$N2_STATUS" \
    -e VRF="$VRF" -e OVERLAY_N1="$OVERLAY_N1" -e OVERLAY_N2="$OVERLAY_N2" \
    -e LOCAL_PFX="$LOCAL_PFX" -e REMOTE_PFX="$REMOTE_PFX" \
    -e WG_IF="$WG_IF" "$N1" bash -s <<'EOS' || die "写配置失败"
set -e
cd "$WORK/pki"
gen_key() { openssl genpkey -algorithm ED25519 -out "$1" 2>/dev/null; }
gen_key ca.key
openssl req -x509 -new -key ca.key -days 30 -subj "/CN=sr-fib-e2e-ca" -out ca.crt 2>/dev/null
gen_key rr.key
openssl req -new -key rr.key -subj "/CN=rr" -out rr.csr 2>/dev/null
cat > server.ext <<EOF
basicConstraints=CA:FALSE
keyUsage=digitalSignature
extendedKeyUsage=serverAuth
subjectAltName=IP:127.0.0.1,DNS:localhost
EOF
openssl x509 -req -in rr.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 30 \
    -extfile server.ext -out rr.crt 2>/dev/null

for n in 1 2; do
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
        -extfile client.ext -out "$d/node_cert.pem" 2>/dev/null
    cp ca.crt "$d/ca_cert.pem"
    cat > "$d/state.json" <<EOF
{"serial":"node$n","key_gen":0,"tenant_id":"tenant-a","node_id":$n}
EOF
done

cat > "$WORK/rr.json" <<EOF
{
  "rr_id": "rr-a",
  "tenant": "tenant-a",
  "hold_sec": 6,
  "listen": "127.0.0.1:$RR_PORT",
  "status_addr": "127.0.0.1:$RR_STATUS",
  "tls": {"cert_file": "$WORK/pki/rr.crt", "key_file": "$WORK/pki/rr.key", "ca_file": "$WORK/pki/ca.crt"},
  "topology": {"2": [1]}
}
EOF

write_node() {   # <n> <overlay> <prefix> <export> <import> <status>
    cat > "$WORK/node$1.json" <<EOF
{
  "tenant": "tenant-a",
  "device_id": $1,
  "identity_dir": "$WORK/identity/node$1",
  "serial": "node$1",
  "hold_sec": 6,
  "rrs": [{"id": "rr-a", "addr": "127.0.0.1:$RR_PORT"}],
  "overlay_addrs": ["$2"],
  "vrfs": [{"id": $VRF, "export_rts": ["$4"], "import_rts": ["$5"]}],
  "local_prefixes": [{"vrf_id": $VRF, "prefix": "$3"}],
  "status_addr": "127.0.0.1:$6"
}
EOF
}
write_node 1 "$OVERLAY_N1" "$LOCAL_PFX"  65000:250 65000:251 "$N1_STATUS"
write_node 2 "$OVERLAY_N2" "$REMOTE_PFX" 65000:251 65000:250 "$N2_STATUS"

cat > "$WORK/fib.json" <<EOF
{
  "status_url": "http://127.0.0.1:$N1_STATUS/status",
  "vpp_socket": "/run/vpp/api.sock",
  "poll_sec": 1,
  "vrfs": [$VRF],
  "tunnels": [{"next_hop": "$OVERLAY_N2", "interface": "$WG_IF"}]
}
EOF

cat > "$WORK/start.sh" <<'EOF'
#!/bin/bash
set -e
cd "$(dirname "$0")"
nohup ./bin/sr-rr    -config rr.json    > rr.log    2>&1 & echo $! > rr.pid
nohup ./bin/sr-route -config node1.json > node1.log 2>&1 & echo $! > node1.pid
nohup ./bin/sr-route -config node2.json > node2.log 2>&1 & echo $! > node2.pid
nohup ./bin/sr-fib   -config fib.json   > fib.log   2>&1 & echo $! > fib.pid
EOF
chmod +x "$WORK/start.sh"
EOS

dx "$N1" "cd $MODULE && go build -o $WORK/bin/sr-rr ./cmd/sr-rr &&
    go build -o $WORK/bin/sr-route ./cmd/sr-route &&
    go build -o $WORK/bin/sr-fib ./cmd/sr-fib" || die "编译失败"
ok "三个二进制已编译"

# 上一轮如果死在半路，VRF 表会留着。只有它确实像本脚本的残留才清。
if vpp_has_table; then
    if label_present || fib_route_present; then
        info "清理上一轮残留（标签 $LABEL 或前缀 $REMOTE_PFX 存在）"
        dx "$N1" "$WORK/bin/sr-fib -config $WORK/fib.json -reset" >/dev/null || die "清理残留失败"
    else
        die "VRF $VRF 已被别的进程占用（既无标签 $LABEL 也无前缀 $REMOTE_PFX）"
    fi
fi
bfd_del "$N1" "$OVERLAY_N1" "$OVERLAY_N2"
bfd_del "$N2" "$OVERLAY_N2" "$OVERLAY_N1"
ok "VRF $VRF 干净、两端无本脚本的 BFD 会话"

# ---------------------------------------------------------------- 2 启动

info "2. 启动 rr / node1 / node2 / sr-fib（全部在 $N1 的 loopback 上）"
dx "$N1" "bash $WORK/start.sh" || die "启动失败"
sleep 0.5

wait_json "$N1_STATUS" \
    "[(r['prefix'], r['next_hop']) for r in d['rib'].get('$VRF', [])] == [('$REMOTE_PFX', '$OVERLAY_N2')]" \
    "node1 的 RIB 收到 node2 的 $REMOTE_PFX（next_hop=$OVERLAY_N2）" 25
wait_json "$N2_STATUS" \
    "[(r['prefix'], r['next_hop']) for r in d['rib'].get('$VRF', [])] == [('$LOCAL_PFX', '$OVERLAY_N1')]" \
    "node2 的 RIB 收到 node1 的 $LOCAL_PFX（反向也通）" 25

# ---------------------------------------------------------------- 3 落 FIB

info "(A) RIB → VPP：前缀、VPN 标签、出口三者同时正确"
wait_fn "VRF $VRF 里出现 $REMOTE_PFX" 10 fib_route_present
wait_fn "该路由压标签 $LABEL 并经 $WG_IF 发往 $OVERLAY_N2" 10 fib_path_labeled
wait_fn "本地标签 $LABEL 已登记（收端弹栈后查 VRF $VRF）" 10 label_present

# ---------------------------------------------------------------- 4 BFD

info "(B) 没有 BFD 会话 = 没有可达性信息 ⇒ 路径保留（fail-open）"
fib_route_present && ok "无会话时路由仍在（没跑 BFD 的 CPE 不会被黑洞）" ||
    bad "无会话时路由缺失（fail-open 被破坏）"

info "(B) 两端会话 Up：路由保持"
bfd_add "$N1" "$OVERLAY_N1" "$OVERLAY_N2"
bfd_add "$N2" "$OVERLAY_N2" "$OVERLAY_N1"
sleep 1
if bfd_wait_up; then ok "BFD 状态探针：up"; else bad "BFD 到不了 up"; fi
sleep 1.5   # 给 sr-fib 一个轮询周期确认它没把路径当成 down
fib_route_present && fib_path_labeled && ok "Up 之后路由与标签路径仍在" ||
    bad "Up 之后路由缺失或路径不对"

info "(B) 对端撤会话 => 本端 Down => 该下一跳的路径从 FIB 消失，标签保留"
bfd_del "$N2" "$OVERLAY_N2" "$OVERLAY_N1"
wait_fn "路由从 VRF $VRF 消失（BFD down）" 10 fib_route_absent
label_present && ok "本地标签 $LABEL 仍在（只失效路径，不拆收端）" ||
    bad "标签被一起撤掉了（收端会断）"

info "(B) 对端恢复 => Up => 路径回来"
bfd_add "$N2" "$OVERLAY_N2" "$OVERLAY_N1"
if bfd_wait_up; then ok "BFD 状态探针：恢复 up"; else bad "BFD 恢复不了 up"; fi
wait_fn "路由重新出现在 VRF $VRF" 10 fib_route_present
wait_fn "恢复后仍压标签 $LABEL 经 $WG_IF" 10 fib_path_labeled

# ---------------------------------------------------------------- 5 撤销

info "(C) 控制面撤销：停掉 node2 的 sr-route，RR 撤回路由，FIB 跟着撤"
kill_one node2
wait_json "$N1_STATUS" "d['rib'].get('$VRF', []) == []" "node1 的 RIB 里 $REMOTE_PFX 已被撤回" 20
wait_fn "sr-fib 把撤回落到 VRF $VRF" 10 fib_route_absent

# ---------------------------------------------------------------- 6 收尾

info "6. 收尾：停进程、撤 FIB 状态"
kill_one fib
sleep 0.3
label_present && ok "reset 前标签 $LABEL 仍由 sr-fib 装着" || bad "标签意外消失"
dx "$N1" "$WORK/bin/sr-fib -config $WORK/fib.json -reset" | sed 's/^/     /'
wait_fn "reset 后标签 $LABEL 已撤" 5 label_absent
wait_fn "reset 后 VRF $VRF 表已撤" 5 table_absent

echo
echo "== 结果：$PASS passed, $FAIL failed"
if [ "$FAIL" -ne 0 ]; then
    echo "== 日志（最后 25 行）：" >&2
    for f in fib node1 node2 rr; do
        echo "---- $f.log" >&2
        dx "$N1" "tail -25 $WORK/$f.log" >&2 2>/dev/null || true
    done
    exit 1
fi
