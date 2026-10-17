#!/usr/bin/env bash
# test-vbond.sh -- vBOND 控制面 acceptance harness（在 Docker 里跑，不需要 VPP）
#
# 拓扑：复用数据面实验室已有的 docker_net_ctrl（10.10.0.0/24）。
#   srwg-ctrl  10.10.0.5  控制器 + 两个 RFC 5389 reflector
#   srwg-node1 10.10.0.2  充当 CPE，用 vbond-client.sh 走协议
# 同网段互通 ⇒ 观测到的映射必须等于真实源，这才敢把"两个证人一致"当断言。
#
# 覆盖的断言：
#   1. 控制器模块零第三方依赖，go vet/build/test 全绿
#   2. 首次启动自建 CA，TLS 控制面可握手
#   3. claim code 入网 → 签发设备证书，证书能用 CA 验证通过
#   4. 同 serial 重复入网是幂等的（node_id 不变）
#   5. 同租户第二台拿到递增 node_id；错误 claim code 被 403 拒绝
#   6. 心跳按证书认人：node_id 与证书不符 → 403；无证书 → 401
#   7. 注册表按租户隔离：tenant-b 的设备只看得到自己
#   8. STUN：客户端解码出的映射 == reflector 自己记下的观测源（两个证人）
#      固定源端口时映射端口即源端口；两个 reflector 给出同一端口 ⇒ 非对称 NAT 判定为 false
#   9. 映射上报后，同租户对端在注册表和 watch 流里都能看到
#
# 用法：test-vbond.sh [--keep]     --keep：跑完保留 srwg-ctrl 容器与日志
set -u

KEEP=0
for a in "$@"; do
    [ "$a" = "--keep" ] && KEEP=1
    case "$a" in -h|--help)
        awk 'NR>1 { if ($0=="set -u") exit; sub(/^# ?/,""); print }' "$0"; exit 0;; esac
done

CTRL=srwg-ctrl
N1=srwg-node1
NET=docker_net_ctrl
CTRL_IP=10.10.0.5
N1_IP=10.10.0.2
REPO=$(cd "$(dirname "$0")/../../.." && pwd)
CTRL_DIR=$REPO/controller
TMP=/tmp/srwg-vbond
REFL_LOG=/tmp/stun-observed.log

PASS=0
FAIL=0
say()  { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
info() { printf '   %s\n' "$*"; }
ok()   { PASS=$((PASS+1)); printf '   \033[1;32m[PASS]\033[0m %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '   \033[1;31m[FAIL]\033[0m %s\n' "$*"; }

dx()  { docker exec "$1" bash -lc "$2" 2>&1; }
dxd() { docker exec -d "$1" bash -lc "$2"; }   # 后台常驻：docker exec -d，不随本脚本退出而死
# 在 node1 里跑手工客户端；输出统一收敛成 stdout，失败靠调用方读 http= 行
cli() { dx "$N1" "/opt/vbond-client.sh $*"; }

cleanup() {
    if [ "$KEEP" = "1" ]; then
        echo
        info "--keep：保留 $CTRL，日志在 $TMP/，控制器数据在容器内 /tmp/ctrl-data"
        return
    fi
    docker rm -f "$CTRL" >/dev/null 2>&1 || true
}
trap cleanup EXIT

mkdir -p "$TMP"
rm -f "$TMP"/*.json "$TMP"/*.log 2>/dev/null || true

[ "$(docker ps -q -f name=^${N1}$)" ] || { echo "srwg-node1 没在跑，先起数据面实验室"; exit 1; }
[ -d "$CTRL_DIR" ] || { echo "找不到 $CTRL_DIR"; exit 1; }

# ==================================================================
say "1. 控制器构建（零第三方依赖，容器里的 Go 1.22.5）"
docker rm -f "$CTRL" >/dev/null 2>&1 || true
docker run -d --name "$CTRL" --network "$NET" --ip "$CTRL_IP" \
    --network-alias ctrl -v "$CTRL_DIR":/workspace/controller \
    srwg-dev:latest sleep infinity >/dev/null || { bad "srwg-ctrl 起不来"; exit 1; }

BUILD=$(dx "$CTRL" 'cd /workspace/controller && go build ./... 2>&1 && go vet ./... 2>&1 && go test ./... 2>&1')
if echo "$BUILD" | grep -qE 'FAIL|error|cannot'; then
    bad "构建/单测失败"; echo "$BUILD" | sed 's/^/    /'
else
    ok "go build + vet + test 全绿：$(echo "$BUILD" | grep -c '^ok') 个包有测试并通过"
fi
BIN=$(dx "$CTRL" 'cd /workspace/controller && go build -o /tmp/sr-ctrl ./cmd/sr-ctrl && go build -o /tmp/sr-stunprobe ./cmd/sr-stunprobe && echo BIN_OK')
[ "$BIN" = "BIN_OK" ] && ok "两个二进制产出（sr-ctrl / sr-stunprobe）" || { bad "二进制构建失败：$BIN"; exit 1; }
dx "$N1" 'mkdir -p /opt/vbond' >/dev/null
docker cp "$CTRL:/tmp/sr-stunprobe" "$TMP/sr-stunprobe" >/dev/null && \
docker cp "$TMP/sr-stunprobe" "$N1:/opt/vbond/sr-stunprobe" >/dev/null
docker cp "$CTRL_DIR/test/scripts/vbond-client.sh" "$N1:/opt/vbond-client.sh" >/dev/null
dx "$N1" 'chmod +x /opt/vbond-client.sh /opt/vbond/sr-stunprobe' >/dev/null

# ==================================================================
say "2. 起 reflector 与控制器"
dx "$CTRL" "rm -f $REFL_LOG"
dxd "$CTRL" "python3 /workspace/controller/test/stun_reflector.py 3478 >/tmp/refl1.log 2>&1"
dxd "$CTRL" "python3 /workspace/controller/test/stun_reflector.py 3479 >/tmp/refl2.log 2>&1"
# 注意 --udp/--udp2 现在是"对外公布的地址"，控制器自己不再监听 UDP：
# 公布 0.0.0.0 会让 agent 拿到一个没法用的地址，所以这里必须给真实 IP。
dxd "$CTRL" "/tmp/sr-ctrl --data /tmp/ctrl-data --listen 0.0.0.0:8444 \
    --udp $CTRL_IP:3478 --udp2 $CTRL_IP:3479 --hb 3 >/tmp/sr-ctrl.log 2>&1"
sleep 2

if dx "$CTRL" 'pgrep -f stun_reflector >/dev/null && pgrep -f sr-ctrl >/dev/null && echo UP' | grep -q UP; then
    ok "控制器与两个 reflector 都在跑"
else
    bad "控制器或 reflector 没起来"; dx "$CTRL" 'cat /tmp/sr-ctrl.log' | sed 's/^/    /'; exit 1
fi
CA_OK=$(dx "$CTRL" 'test -s /tmp/ctrl-data/ca.pem && echo yes')
[ "$CA_OK" = "yes" ] && ok "首次启动自建 CA（bootstrap 信任锚）" || bad "没有 CA"

docker cp "$CTRL:/tmp/ctrl-data/ca.pem" "$TMP/ca.pem" >/dev/null
docker cp "$TMP/ca.pem" "$N1:/opt/vbond/ca.pem" >/dev/null
HANDSHAKE=$(dx "$N1" 'curl -sS -o /dev/null -w "%{http_code}" --cacert /opt/vbond/ca.pem https://ctrl:8444/v1/registry')
if echo "$HANDSHAKE" | grep -q "401"; then
    ok "TLS 控制面可握手，且无证书访问被 401 拒绝"
elif echo "$HANDSHAKE" | grep -qi "ssl|certificate"; then
    bad "TLS 握手失败：$HANDSHAKE"
else
    bad "无证书访问 registry 返回的不是 401：$HANDSHAKE"
fi

# ==================================================================
say "3. 首台 CPE 入网（claim-a → tenant-a）"
E1=$(cli enroll dev1 claim-a CPE-001)
echo "$E1" | sed 's/^/    /'
echo "$E1" | grep -q "http=200" && ok "入网返回 200" || bad "入网没返回 200"
echo "$E1" | grep -q "tenant=tenant-a node_id=1" && ok "claim-a 解析到 tenant-a，node_id=1" \
    || bad "租户或 node_id 不对：$(echo "$E1" | grep tenant=)"
CERT_OK=$(dx "$N1" 'openssl verify -CAfile /opt/vbond/ca.pem /opt/vbond/dev1.pem')
echo "$CERT_OK" | grep -q "OK" && ok "设备证书能用 CA 验证通过（链到租户）" \
    || bad "证书链验证失败：$CERT_OK"
echo "$E1" | grep -qE "root_key_len=32" && ok "响应带 16 字节租户密钥（hex 32）+ 反射器地址" \
    || bad "root_key 长度不对：$(echo "$E1" | grep root_key_len)"

E1b=$(cli enroll dev1 claim-a CPE-001)
if echo "$E1b" | grep -q "tenant=tenant-a node_id=1"; then
    ok "同 serial 重复入网是幂等的（node_id 仍为 1）"
else
    bad "重复入网换了身份：$(echo "$E1b" | grep tenant=)"
fi

# ==================================================================
say "4. 第二台同租户 + 错误 claim"
E2=$(cli enroll dev2 claim-b CPE-002)
echo "$E2" | grep -q "node_id=2" && ok "同租户第二台拿到 node_id=2" || bad "node_id 不对：$E2"
EBAD=$(cli enroll devX bad-claim CPE-009)
echo "$EBAD" | grep -q "http=403" && ok "未知 claim code 被 403 拒绝" || bad "错误 claim 没被拒：$EBAD"

# ==================================================================
say "5. 心跳按证书认人"
H1=$(cli hb dev1)
echo "$H1" | sed 's/^/    /'
echo "$H1" | grep -q '"peer_count":2' && ok "心跳成功，peer_count=2（含自己，同租户两台）" \
    || bad "peer_count 不是 2：$H1"
HWRONG=$(cli hb dev1 99)
echo "$HWRONG" | grep -q "http=403" && ok "拿 node1 的证书冒充 node 99 被 403 拒绝" \
    || bad "node_id 校验没生效：$HWRONG"

# ==================================================================
say "6. 注册表按租户隔离"
E3=$(cli enroll dev3 claim-c CPE-003)
echo "$E3" | grep -q "tenant=tenant-b" && ok "claim-c 属于 tenant-b" || bad "跨租户 claim 落错租户：$E3"
R3=$(cli registry dev3)
echo "$R3" | sed 's/^/    /'
echo "$R3" | grep -q "peers=1" && ok "tenant-b 的设备在注册表里只看得到自己（隔离生效）" \
    || bad "跨租户泄漏：$R3"
R1=$(cli registry dev1)
echo "$R1" | grep -q "peers=2" && ok "dev1 看到同租户 2 个节点" || bad "dev1 注册表数量不对：$R1"

# ==================================================================
say "7. STUN：客户端解码 == reflector 观测"
P1=$(dx "$N1" '/opt/vbond/sr-stunprobe -reflector '"$CTRL_IP"':3478 -tries 5')
echo "    $P1"
TXID=$(echo "$P1" | sed -n 's/.*txid=\([0-9a-f]*\).*/\1/p')
OBS=$(echo "$P1" | sed -n 's/.*observed=\([0-9.]*:[0-9]*\).*/\1/p')
if [ -n "$OBS" ]; then ok "拿到映射 $OBS"; else bad "STUN 探针没拿到映射：$P1"; fi
# reflector 自己记的账（第二个证人）：同一 txid 的 src/port 必须与客户端解码一致
WIT=""
if [ -n "$TXID" ]; then WIT=$(dx "$CTRL" "grep '\"txid\": \"$TXID\"' $REFL_LOG | tail -1"); fi
echo "    reflector 记录：$WIT"
WIP=$(echo "$WIT" | python3 -c 'import json,sys
l=sys.stdin.read().strip()
print(json.loads(l)["src"] if l else "")' 2>/dev/null)
WPORT=$(echo "$WIT" | python3 -c 'import json,sys
l=sys.stdin.read().strip()
print(json.loads(l)["port"] if l else "")' 2>/dev/null)
[ "$WIP" = "$N1_IP" ] && ok "reflector 观测到的 IP = $N1_IP（真实源，不是猜的）" \
    || bad "reflector 观测 IP = '$WIP'，期望 $N1_IP"
if [ "${OBS%%:*}:$WPORT" = "$OBS" ]; then
    ok "客户端解出的端口 = reflector 记的端口 $WPORT（两个证人一致）"
else
    bad "端口不一致：客户端 ${OBS##*:} vs reflector $WPORT"
fi

# 固定源端口：同网段没有 NAT，映射端口就应当等于源端口
P2=$(dx "$N1" '/opt/vbond/sr-stunprobe -reflector '"$CTRL_IP"':3478 -local :41780 -tries 3')
echo "    $P2"
echo "$P2" | grep -q "observed=$N1_IP:41780" && ok "固定源端口 41780 → 映射 $N1_IP:41780（无 NAT 时端口守恒）" \
    || bad "固定源端口的映射不对：$P2"
# 两个 reflector 给出同一个映射端口 ⇒ 端点无关 NAT，打洞可行
P3=$(dx "$N1" '/opt/vbond/sr-stunprobe -reflector '"$CTRL_IP"':3479 -local :41780 -tries 3')
echo "    $P3"
if echo "$P3" | grep -q "observed=$N1_IP:41780"; then
    ok "第二个 reflector 同端口 ⇒ 对称 NAT 判定为 false"
else
    bad "第二个 reflector 给出不同映射（对称判定会翻转）：$P3"
fi

# ==================================================================
say "8. 映射上报 → 对端可见 + watch 推送"
MAP="$N1_IP:41780"
docker exec "$N1" /opt/vbond-client.sh watch dev1 6 > "$TMP/watch.txt" 2>&1 &
WPID=$!
sleep 1
M=$(cli mapping dev2 "$MAP" 41780 "$CTRL_IP:3478")
echo "$M" | sed 's/^/    /'
echo "$M" | grep -q '"ok":true' && ok "dev2 上报映射被接受" || bad "映射上报失败：$M"
wait $WPID || true
W=$(cat "$TMP/watch.txt")
R1b=$(cli registry dev1)
echo "$R1b" | grep -q "$MAP" && ok "dev1 的注册表里看到 dev2 的新映射 $MAP" \
    || bad "映射没进对端注册表：$R1b"
echo "$W" | sed 's/^/    /'
echo "$W" | grep -q "update" && ok "watch 流收到 update 事件（映射变更走推送，不用轮询）" \
    || bad "watch 流没有 update 事件"
echo "$W" | grep -q "snapshot" && ok "watch 建连即发 snapshot，无需额外的'发现对端'往返" \
    || bad "watch 没有 snapshot"

# ==================================================================
echo
echo "=========================================================="
echo " 控制器日志：docker exec $CTRL cat /tmp/sr-ctrl.log"
echo " reflector 观测：docker exec $CTRL cat $REFL_LOG"
echo " RESULT: ${PASS} passed, ${FAIL} failed"
echo "=========================================================="
[ "$FAIL" = "0" ] || dx "$CTRL" 'tail -20 /tmp/sr-ctrl.log' | sed 's/^/    /'
