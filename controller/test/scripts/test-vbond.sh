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
#  10. 数据面事实下发：vpn_port/transit_port 两个显式端口 + 租户 p-key 环（hmac_id/16 字节）；
#      旧协议版本（v2）被明确拒绝，不是"接受了但字段没人看"
#  11. WG 静态公钥的设备身份担保（keybinding）：控制器验签后存储并 relay 原文；
#      签名正确但覆盖对象不对（拿别人的 tenant/node 签）必须被拒
#  12. 观测回报 /v1/observe：按"我看到的源"归因到具体节点，逐读者渲染；
#      陌生人地址不猜、落在非数据面端口的不算、node_id 冒充被拒、
#      先观测到后申报地址的迟到归因、reset 让消失的源真的消失
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
# 客户端状态目录必须每轮清空：srwg-node1 是常驻容器，上一轮的 dev*.bind.json /
# *.key 会留在里面，而 mapping 会"有 bind.json 就带上"。上一轮的合法 binding
# 混进本轮，就能把"这一轮伪造的 binding 没进注册表"验证成假阳性/假阴性。
dx "$N1" 'rm -rf /opt/vbond && mkdir -p /opt/vbond' >/dev/null
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
    --udp $CTRL_IP:3478 --udp2 $CTRL_IP:3479 --hb 10 \
    --vpn-port 51820 --transit-port 51821 >/tmp/sr-ctrl.log 2>&1"
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
echo "$E1" | grep -q "vpn_port=51820 transit_port=51821" \
    && ok "两个数据面功能端口显式下发（不再有 base+1 那种隐含算术）" \
    || bad "端口下发不对：$(echo "$E1" | grep vpn_port)"
echo "$E1" | grep -qE "p_key hmac_id=1 key_hex_len=32" \
    && ok "下发租户 p-key 环：hmac_id=1、16 字节（hex 32），形状就是 sr-wg key set 的入参" \
    || bad "p-key 环不对：$(echo "$E1" | grep p_key)"
echo "$E1" | grep -q "reflectors=" && ok "响应带反射器地址" || bad "响应里没有反射器地址"

# 旧协议必须被拒，而不是"接受了但字段没人看"：v2 agent 不发端口也不发观测。
E1V2=$(dx "$N1" 'VBOND_PROTVER=2 /opt/vbond-client.sh enroll devv2 claim-a CPE-001')
echo "$E1V2" | grep -q "http=400" && ok "protocol_ver=2 被 400 明确拒绝（不静默兼容）" \
    || bad "旧版本没被拒：$(echo "$E1V2" | head -1)"

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
M=$(cli mapping dev2 "$MAP" 41780 "$CTRL_IP:3478" "10.99.2.2")
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
say "9. WG 静态公钥的设备身份担保（覆盖项 11）"
# 用 dev2 的身份私钥签一个"声称 tenant-b / node 1"的 binding：签名本身是真的，
# 覆盖的对象是假的。控制器如果只检查"有没有签名字段"就会收下，然后把它 relay 给
# 对端 —— 这一条就是用来区分"验签"和"验了个格式"。
cli bind dev2 tenant-b 1 1 >/dev/null || { bad "bind 失败（openssl pkeyutl -rawin 可用？）"; }
FB=$(cli mapping dev2 "10.99.2.2:41780" 41780 "$CTRL_IP:3478" "10.99.2.2")
echo "$FB" | sed 's/^/    /'
echo "$FB" | grep -q '"binding_err":"[^"]' \
    && ok "签名正确但对象不对（冒充 tenant-b/node1）→ 拒绝并报出原因" \
    || bad "伪造的 binding 被接受了：$FB"
# 控制器不能顺手清掉上一次的正确存储：dev2 此刻应当仍然没有可用 binding（第一次就是伪造）。
R2a=$(cli registry dev2)
echo "$R2a" | grep -A1 "node=2 " | grep -q "binding=无" \
    && ok "被拒的 binding 没有进注册表（node2 仍是无担保状态）" \
    || bad "被拒的 binding 出现在注册表里：$R2a"

# 现在换成真身份：tenant-a / node 2。
X2=$(cli bind dev2 | sed -n 's/.*x25519=\([^ ]*\).*/\1/p')
MB=$(cli mapping dev2 "10.99.2.2:41780" 41780 "$CTRL_IP:3478" "10.99.2.2")
echo "$MB" | grep -q '"binding_err":""' && ok "同租户同 node 的 binding 通过校验（binding_err 空）" \
    || bad "正当 binding 被拒：$MB"
# 先重新取一份注册表快照，再取文件：docker cp 拿到的是容器里上一轮的旧 JSON，
# 拿它比对会得出"公钥没 relay"的假结论（本轮就差点被这个坑骗过去）。
cli registry dev2 >/dev/null
docker cp "$N1:/opt/vbond/dev2.reg.json" "$TMP/dev2.reg.json" >/dev/null
RELAY=$(python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
for p in d["peers"]:
    if p["node_id"] == 2:
        b = p.get("key_binding") or {}
        print(b.get("x25519_pub",""))' "$TMP/dev2.reg.json")
[ -n "$X2" ] && [ "$RELAY" = "$X2" ] \
    && ok "注册表 relay 的就是那把 X25519 公钥原文（逐字节一致，控制器没换钥匙）" \
    || bad "relay 的公钥对不上：客户端=$X2 注册表=$RELAY"

# ==================================================================
say "10. 观测回报与归因（覆盖项 12）"
# 归因只看"这个源地址被某个节点在认证信道上申报过"。这里用两个合成的 underlay
# 地址（10.99.x.x）代表两台 CPE 的数据面地址 —— 本段证明的是控制器契约，
# 数据面到控制器的搬运在 e2e（#13）里接。
cli hb dev1 >/dev/null; cli hb dev2 >/dev/null          # 别让 30s TTL 把节点熬成 stale
M1=$(cli mapping dev1 "$N1_IP:41781" 41781 "$CTRL_IP:3478" "10.99.1.1")
echo "$M1" | grep -q '"ok":true' && ok "dev1 上报映射并申报自己的地址 10.99.1.1" || bad "dev1 映射上报失败：$M1"

O1=$(cli observe dev2 0 "10.99.1.1:41781:51820:1")
echo "$O1" | sed 's/^/    /'
echo "$O1" | grep -q "accepted=1" && ok "观测回报被接收（accepted=1）" || bad "观测没被接受：$O1"
echo "$O1" | grep -qE '10\.99\.1\.1:41781 +-> node 1 +unique' \
    && ok "按申报地址唯一归因到 node 1（不是猜的：只有它声称过这个源）" \
    || bad "归因结果不对：$O1"
# 逐读者：dev2 看 dev1 有 seen，dev1 看 dev2 必须没有（dev1 从没观测过 dev2）。
cli registry dev1 >/dev/null
docker cp "$N1:/opt/vbond/dev1.reg.json" "$TMP/dev1.reg.json" >/dev/null
R2=$(cli registry dev2)
PEER2=$(python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
print(next((p.get("observed_mapping","") for p in d["peers"] if p["node_id"]==2), "X"))' "$TMP/dev1.reg.json")
[ -z "$PEER2" ] && ok "dev1 的注册表里 dev2 没有 seen 值：观测按读者渲染，不是一份共享账本" \
    || bad "观测跨读者泄漏：dev1 看到了 dev2 的 seen=$PEER2"
echo "$R2" | grep -q "seen=10.99.1.1:41781" && ok "dev2 的注册表里 dev1 带上了我看到的源" \
    || bad "对端注册表没有观测值：$R2"

# 三个"不许答"的情形。
O2=$(cli observe dev2 0 "198.51.100.9:5555:51820")
echo "$O2" | grep -qE '198\.51\.100\.9:5555 +-> \?' \
    && ok "陌生人地址（没人申报过）不认领，只挂账" || bad "陌生人地址被归因了：$O2"
O3=$(cli observe dev2 0 "10.99.1.1:41781:443")
echo "$O3" | grep -q "accepted=0" && ok "落在非数据面端口(443)的观测不作为隧道证据" \
    || bad "外层目的端口没当角色键用：$O3"
# 冒充：拿 dev1 的证书以 node 99 的身份回报观测。
O403=$(dx "$N1" 'curl -sS -o /dev/null -w "%{http_code}" --cacert /opt/vbond/ca.pem \
    --cert /opt/vbond/dev1.pem --key /opt/vbond/dev1.key -X POST https://ctrl:8444/v1/observe \
    -d "{\"node_id\":99,\"reset\":false,\"observations\":[]}"')
[ "$O403" = "403" ] && ok "node_id 与证书不符的观测回报被 403 拒绝" || bad "冒充 node99 回报观测：http=$O403"
O401=$(dx "$N1" 'curl -sS -o /dev/null -w "%{http_code}" --cacert /opt/vbond/ca.pem \
    -X POST https://ctrl:8444/v1/observe -d "{\"node_id\":1,\"observations\":[]}"')
[ "$O401" = "401" ] && ok "无证书的观测回报被 401 拒绝" || bad "无证书回报观测：http=$O401"

# 迟到归因：先看到一个源，事后才有主人申报地址 —— 两台 CPE 同时入网就是这个次序。
E4=$(cli enroll dev4 claim-a CPE-004)
N4=$(echo "$E4" | sed -n 's/.*node_id=\([0-9]*\).*/\1/p')
O5=$(cli observe dev2 0 "10.99.4.4:41790:51820")
echo "$O5" | grep -qE '10\.99\.4\.4:41790 +-> \?' && ok "dev4 还没申报地址时，它的源先挂账不认领" \
    || bad "没有主人时就把地址认了：$O5"
cli mapping dev4 "10.99.4.4:41790" 41790 "$CTRL_IP:3478" "10.99.4.4" >/dev/null
cli registry dev2 >/dev/null
docker cp "$N1:/opt/vbond/dev2.reg.json" "$TMP/dev2.reg2.json" >/dev/null
LATE=$(python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
print(next((p.get("observed_mapping","") for p in d["peers"] if p["node_id"]==int(sys.argv[2])), ""))' \
    "$TMP/dev2.reg2.json" "${N4:-0}")
[ "$LATE" = "10.99.4.4:41790" ] \
    && ok "dev4 事后申报地址后，挂账的观测被补归因（入网与回报谁先到都行）" \
    || bad "迟到归因没生效：node$N4 observed=$LATE"

# reset：清空后消失的源必须真的消失（否则控制器只会做加法，把死地址当活的发布）。
O6=$(cli observe dev2 1)
echo "$O6" | grep -q "accepted=0" && ok "reset=1 且空表：本轮以清空为准" || bad "reset 回报异常：$O6"
R2c=$(cli registry dev2)
echo "$R2c" | grep -q "seen=10.99.1.1" && bad "reset 之后旧观测还在" || ok "reset 之后旧观测确实被摘掉"

# ==================================================================
echo
echo "=========================================================="
echo " 控制器日志：docker exec $CTRL cat /tmp/sr-ctrl.log"
echo " reflector 观测：docker exec $CTRL cat $REFL_LOG"
echo " RESULT: ${PASS} passed, ${FAIL} failed"
echo "=========================================================="
[ "$FAIL" = "0" ] || dx "$CTRL" 'tail -20 /tmp/sr-ctrl.log' | sed 's/^/    /'
