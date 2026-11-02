#!/bin/bash
# test-rr-mesh.sh - O9-b2 b2b 判据：RR 之间的网格是控制器拉出来的，而且只增不改
#
# 这条脚本在 lab 里起**三个真的 sr-rr 进程**和**两台真的设备**，全部从控制器的
# RR 库拿身份、拿邻居清单，量四件事：
#
#   (1) 一个租户两条 RR 行：网格自己长出来，方向由 id 次序决定 —— 小 id 拨大 id。
#       判据：每台 RR 对同一个对端**只有一条会话**（不是两条各拨一次），拨的那端
#       dialing=true、被拨的一端 dialing=false，两边都 connected 且 synced。
#   (2) 反射真的跨过了网格：node1 只挂 RR-A、node2 只挂 RR-B，两台从来没连过对方
#       的 RR。node1 却学到了 node2 的前缀 —— 那只能是 RR-B 从网格里把路由反射给
#       RR-A 的。反面同时钉住：RR-A 的会话表里没有 device 2 的客户端会话。
#   (3) **只增不改**（O9 的硬要求）：再加入第三条 RR 行，RR-A↔RR-B 那条会话的
#       since 一动不动，两台设备的路由表一条不缺、withdrawn 不涨。新 RR 从网格快照
#       拿到全表，两台设备一个字都不用改。
#   (4) **删一行 = 这台 RR 不再存在**：删掉第三条行之后，它下一次拉 /v1/rr/config
#       拿到 404，进程以 1 退出；RR-A/RR-B 在下一轮轮询（≤3s）里把它摘掉，各自的
#       RR-A↔RR-B 会话仍然没重建，两台设备的表还是满的。
#
# 身份从哪里来：三条 RR 行都由租户账号现场调用 /api/rrs/identity 签发，私钥只写进
# $WORK/id/<rr_id>/（0600，随 $WORK 一起删）。脚本只在输出里打印指纹，不打印任何
# PEM、口令、会话 cookie；租户口令只从环境变量进来，经 stdin 进容器，不进 argv、
# 不落盘。
#
# 必须在**宿主机**上跑（需要 docker CLI），并且要给一个租户账号：
#   TENANT_USER=<租户账号> TENANT_PASS=<口令> test/scripts/test-rr-mesh.sh [--keep]
#
# lab 里正在跑的 sr-rr / sr-agent / sr-route 一概不动：本脚本另起进程、另用端口、
# 另用 $WORK。RR 行是租户的 RR 库，测试结束会删干净（只删自己加的三行）。
set -u

RR=${RR:-lab-rr}
N1=${N1:-lab-cpe1}
N2=${N2:-lab-cpe2}
CTRL=${CTRL:-lab-ctrl}
MODULE=${MODULE:-/workspace/controller}
WORK=${WORK:-/tmp/sr-rr-mesh}

RR_IP=${RR_IP:-10.99.0.100}
TENANT=${TENANT:-tenant-a}
CTRL_API=${CTRL_API:-https://lab-ctrl:8444}   # 容器里看到的名字
POLL=${POLL:-3}                               # 轮询间隔（秒），配置下限就是 3
RT=${RT:-65000:1001}
PFX1=${PFX1:-10.177.1.0/24}                   # device 1 宣告
PFX2=${PFX2:-10.177.2.0/24}                   # device 2 宣告
NAME_PREFIX=${NAME_PREFIX:-meshtest-}

RR_A_PORT=18966; RR_A_ST=18967
RR_B_PORT=18976; RR_B_ST=18977
RR_C_PORT=18986; RR_C_ST=18987
N1_ST=18981
N2_ST=18982
RR_A_ADDR=$RR_IP:$RR_A_PORT
RR_B_ADDR=$RR_IP:$RR_B_PORT
RR_C_ADDR=$RR_IP:$RR_C_PORT

KEEP=0
for a in "$@"; do
    case "$a" in
      --keep) KEEP=1 ;;
      -h|--help) awk 'NR>1 { if ($0=="set -u") exit; sub(/^# ?/,""); print }' "$0"; exit 0 ;;
      *) echo "unknown option: $a" >&2; exit 2 ;;
    esac
done

PASS=0
FAIL=0
ok()   { PASS=$((PASS+1)); echo "  [PASS] $*"; }
bad()  { FAIL=$((FAIL+1)); echo "  [FAIL] $*"; }
info() { echo "== $*"; }
note() { echo "       $*"; }
die()  { echo "FATAL: $*" >&2; exit 2; }

dx()  { docker exec "$1" bash -lc "$2"; }
dxi() { docker exec -i "$1" bash -lc "$2"; }

CREATED_IDS=""      # 本轮创建/复用的 RR 行，cleanup 只删这些
HELPER_READY=0

# ---------------------------------------------------------------- 控制器账本

# call_ctrl <cmd> <extra-json> —— 口令只从环境变量进子进程、经 stdin 进容器。
# 子进程的 argv 里只有 cmd 和 extra（不含口令）。
call_ctrl() {
    local cmd=$1 extra=$2 json
    json=$(python3 -c '
import json, os, sys
print(json.dumps({"cmd": sys.argv[1],
                  "user": os.environ["TENANT_USER"],
                  "pass": os.environ["TENANT_PASS"],
                  **json.loads(sys.argv[2])}))
' "$cmd" "$extra") || die "组装请求失败"
    dxi "$RR" "python3 $WORK/ctrl.py" <<< "$json"
}

row_id() {   # <helper 输出> <name> -> id
    printf '%s\n' "$1" | awk -v n="$2" '$1=="ROW" && $3==n {print $2; exit}'
}
row_fetch() {  # <helper 输出> <id> -> last_fetch
    printf '%s\n' "$1" | awk -v i="$2" '$1=="ROW" && $2==i {print $5; exit}'
}

# ---------------------------------------------------------------- 状态口读法

get_url() { dx "$1" "curl -sf --max-time 2 '$2'" 2>/dev/null; }
rr_url() { printf 'http://127.0.0.1:%s/%s' "$1" "$2"; }  # <status port> <path>
n1_url() { printf 'http://127.0.0.1:%s/%s' "$N1_ST" "$1"; }
n2_url() { printf 'http://127.0.0.1:%s/%s' "$N2_ST" "$1"; }

# wait_json <container> <url> <python expr over d> <label> [timeout_s]
wait_json() {
    local c=$1 url=$2 expr=$3 label=$4 limit=${5:-25} i=0
    while [ "$i" -lt $((limit * 5)) ]; do
        if get_url "$c" "$url" |
            python3 -c "import json,sys; d=json.load(sys.stdin); sys.exit(0 if ($expr) else 1)" 2>/dev/null; then
            ok "$label"
            return 0
        fi
        sleep 0.2
        i=$((i + 1))
    done
    bad "$label（超时）"
    get_url "$c" "$url" | python3 -m json.tool 2>/dev/null | head -40 >&2
    return 1
}

field() {   # <container> <url> <python expr>
    local v
    v=$(get_url "$1" "$2" | python3 -c "import json,sys; d=json.load(sys.stdin); print($3)" 2>/dev/null)
    [ -n "$v" ] || v=-
    printf '%s\n' "$v"
}
need() { [ -n "$1" ] && [ "$1" != "-" ] || die "读不到 $2（$1）"; }

since_of() {  # <status port> <peer key>
    field "$RR" "$(rr_url "$1" status)" \
        "[s['since'] for s in d['sessions'] if s['key'] == 'rr/$2'][0]"
}
has_pfx() {   # <container> <status url> <prefix>
    get_url "$1" "$2" | python3 -c "
import json,sys
d=json.load(sys.stdin)
sys.exit(0 if '$3' in [r['prefix'] for v in d['rib'].values() for r in v] else 1)" >/dev/null 2>&1
}
stamp_of() {  # <container> <pid file>
    dx "$1" "p=\$(cat $2 2>/dev/null); ps -o pid=,lstart= -p \$p 2>/dev/null | tr -s ' '"
}

# ---------------------------------------------------------------- 起进程

start_rr() {   # <name> <rr_id> <listen port> <status port>
    local name=$1 id=$2 listen=$3 st=$4
    dxi "$RR" "cat > $WORK/$name.json" <<JSON
{
  "rr_id": "$id",
  "tenant": "$TENANT",
  "hold_sec": 12,
  "listen": "0.0.0.0:$listen",
  "status_addr": "0.0.0.0:$st",
  "tls": {
    "cert_file": "$WORK/id/$id/cert.pem",
    "key_file": "$WORK/id/$id/key.pem",
    "ca_file": "$WORK/id/$id/ca.pem"
  },
  "controller": {"addr": "$CTRL_API", "poll_sec": $POLL}
}
JSON
    # 外面套一层 sh 记退出码：删行之后这台 RR 必须自己以 1 退出，而不是留着当幽灵。
    dx "$RR" "cd $WORK && nohup bash -c './bin/sr-rr -config $WORK/$name.json >> $WORK/$name.log 2>&1; echo \$? > $WORK/$name.exit' >/dev/null 2>&1 & echo \$! > $WORK/$name.pid" >/dev/null ||
        die "起 $name 失败"
    wait_json "$RR" "$(rr_url "$st" status)" "d['rr_id'] == '$id'" \
        "sr-rr $id 起来了（听 $listen，状态口 $st）" 15 >/dev/null ||
        die "sr-rr $id 没起来：$(dx "$RR" "tail -5 $WORK/$name.log" 2>/dev/null | tr '\n' ' ')"
}

start_node() {   # <container> <device id> <serial> <status port> <rr id> <rr addr> <peer nh> <prefix>
    local c=$1 dev=$2 serial=$3 st=$4 rrid=$5 rraddr=$6 peer=$7 nh=$8 pfx=$9
    dxi "$c" "cat > $WORK/node$dev.json" <<JSON
{
  "device_id": $dev,
  "tenant": "$TENANT",
  "serial": "$serial",
  "identity_dir": "/etc/sdwan/identity",
  "hold_sec": 6,
  "status_addr": "0.0.0.0:$st",
  "rrs": [{"id": "$rrid", "addr": "$rraddr"}],
  "tunnels": [{"peer_device_id": $peer, "next_hop": "$nh"}],
  "vrfs": [{"id": 0, "export_rts": ["$RT"], "import_rts": ["$RT"]}]
}
JSON
    dx "$c" "cd $WORK && nohup ./bin/sr-route -config $WORK/node$dev.json > $WORK/node$dev.log 2>&1 & echo \$! > $WORK/node$dev.pid" >/dev/null ||
        die "起 device $dev 的 sr-route 失败"
    wait_json "$c" "http://127.0.0.1:$st/status" "d['device_id'] == $dev" \
        "device $dev 的 sr-route 起来了（状态口 $st）" 15 >/dev/null ||
        die "device $dev 的 sr-route 没起来：$(dx "$c" "tail -5 $WORK/node$dev.log" 2>/dev/null | tr '\n' ' ')"
    local i=0
    while [ "$i" -lt 50 ]; do
        if dx "$c" "curl -sf --max-time 2 -X PUT http://127.0.0.1:$st/local-prefixes \
            -H 'Content-Type: application/json' \
            -d '{\"prefixes\":[{\"vrf_id\":0,\"prefix\":\"$pfx\"}]}'" >/dev/null 2>&1; then
            ok "device $dev 宣告 $pfx（扮演 sr-frr）"
            return 0
        fi
        sleep 0.2
        i=$((i + 1))
    done
    die "device $dev 的前缀推不进去"
}

cleanup() {
    [ "$KEEP" = 1 ] && return 0
    local c f p id
    for c in "$RR" "$N1" "$N2"; do
        for f in mesh-a mesh-b mesh-c node1 node2; do
            p=$(dx "$c" "cat $WORK/$f.pid 2>/dev/null" 2>/dev/null | tr -d ' ')
            [ -n "$p" ] && dx "$c" "kill $p 2>/dev/null" >/dev/null 2>&1
            # pid 文件里是套的一层 sh：一定要按配置路径把真正的进程也收掉，否则端口留着
            dx "$c" "pkill -f '$WORK/$f.json' 2>/dev/null" >/dev/null 2>&1
        done
    done
    if [ "$HELPER_READY" = 1 ] && [ -n "$CREATED_IDS" ]; then
        local ids_json
        ids_json=$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1:]))' $CREATED_IDS 2>/dev/null)
        [ -n "$ids_json" ] && call_ctrl delete "{\"ids\": $ids_json}" >/dev/null 2>&1
    fi
    for c in "$RR" "$N1" "$N2"; do
        dx "$c" "rm -rf $WORK" >/dev/null 2>&1
    done
}
trap cleanup EXIT

# ---------------------------------------------------------------- 0 准备

[ -n "${TENANT_USER:-}" ] && [ -n "${TENANT_PASS:-}" ] ||
    die "要给租户账号：TENANT_USER=<账号> TENANT_PASS=<口令> $0（RR 库按租户读写，运营方令牌这里用不上）"
command -v docker >/dev/null 2>&1 || die "这条脚本要在**宿主机**上跑（需要 docker CLI）"
for c in "$RR" "$N1" "$N2" "$CTRL"; do
    docker inspect "$c" >/dev/null 2>&1 || die "容器 $c 不在：先起 lab"
done
for pair in "$N1:/etc/sdwan/identity/node_cert.pem" "$N2:/etc/sdwan/identity/node_cert.pem"; do
    c=${pair%%:*}; p=${pair#*:}
    dx "$c" "[ -e $p ]" >/dev/null 2>&1 || die "$c 里没有 $p：这两台盒子还没入网"
done

info "(0) 编译二进制、准备 $WORK、确认端口没人占"
dx "$CTRL" "cd $MODULE && go build -o bin/sr-rr ./cmd/sr-rr && go build -o bin/sr-route ./cmd/sr-route" >/dev/null ||
    die "编译失败"
for c in "$RR" "$N1" "$N2"; do
    dx "$c" "rm -rf $WORK && mkdir -p $WORK/id $WORK/bin && cp -f $MODULE/bin/sr-rr $MODULE/bin/sr-route $WORK/bin/" >/dev/null ||
        die "$c 里没拿到二进制"
done
for pair in "$RR:$RR_A_PORT $RR_A_ST $RR_B_PORT $RR_B_ST $RR_C_PORT $RR_C_ST" "$N1:$N1_ST" "$N2:$N2_ST"; do
    c=${pair%%:*}
    for p in ${pair#*:}; do
        if dx "$c" "ss -ltn 2>/dev/null | grep -q ':$p '" 2>/dev/null; then
            die "$c 上端口 $p 已被占用：先撤掉占用它的进程（或是上一次 --keep 留下来的）"
        fi
    done
done
ok "二进制就绪，八个端口全空"

dxi "$RR" "cat > $WORK/ctrl.py" <<'PY'
# 租户侧的 RR 库操作。请求从 stdin 进来（口令不经过 argv），输出只打印 id/名字/
# 地址/指纹/状态码：证书和私钥只写进 dir，私钥 0600，绝不回显。
import http.cookiejar
import json
import os
import sys
import urllib.error
import urllib.request

BASE = os.environ.get("CONSOLE_URL", "http://lab-ctrl:8080")


def die(msg):
    print("ERR " + msg)
    sys.exit(1)


req = json.load(sys.stdin)
jar = http.cookiejar.CookieJar()
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))


def call(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + path, data=data, method=method)
    if data is not None:
        r.add_header("Content-Type", "application/json")
    try:
        with opener.open(r, timeout=10) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


code, body = call("POST", "/api/login", {"username": req["user"], "password": req["pass"]})
if code != 200:
    die("login HTTP %d" % code)

cmd = req["cmd"]
if cmd == "create":
    code, body = call("GET", "/api/rrs")
    if code != 200:
        die("list HTTP %d" % code)
    have = {(r["name"], r["addr"]): r["id"] for r in json.loads(body)["rrs"]}
    for spec in req["rrs"]:
        key = (spec["name"], spec["addr"])
        if key in have:
            print("ROW %s %s %s reused" % (have[key], spec["name"], spec["addr"]))
            continue
        code, body = call("POST", "/api/rrs",
                          {"name": spec["name"], "addr": spec["addr"],
                           "location": spec.get("location", "")})
        if code != 200:
            die("create %s HTTP %d: %s" % (spec["name"], code, body.strip()[:200]))
        rr = json.loads(body)["rr"]
        print("ROW %s %s %s new" % (rr["id"], rr["name"], rr["addr"]))
elif cmd == "identity":
    out = req["dir"]
    os.makedirs(out, mode=0o700, exist_ok=True)
    code, body = call("POST", "/api/rrs/identity", {"id": req["id"]})
    if code != 200:
        die("identity %s HTTP %d: %s" % (req["id"], code, body.strip()[:200]))
    d = json.loads(body)
    for name, content, mode in (("cert.pem", d["cert_pem"], 0o644),
                                ("key.pem", d["key_pem"], 0o600),
                                ("ca.pem", d["ca_pem"], 0o644)):
        p = os.path.join(out, name)
        fd = os.open(p, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, mode)
        os.write(fd, content.encode())
        os.close(fd)
        os.chmod(p, mode)
    print("ID %s %s %s" % (d["rr_id"], d["fingerprint"], d["serial"]))
elif cmd == "delete":
    for rid in req["ids"]:
        code, body = call("POST", "/api/rrs/delete", {"id": rid})
        print("DEL %s %d%s" % (rid, code,
                               "" if code == 200 else " " + body.strip()[:160]))
elif cmd == "list":
    code, body = call("GET", "/api/rrs")
    if code != 200:
        die("list HTTP %d" % code)
    for r in json.loads(body)["rrs"]:
        print("ROW %s %s %s %s" % (r["id"], r["name"], r["addr"],
                                   r.get("last_fetch_at_unix", 0)))
else:
    die("unknown cmd %r" % cmd)
PY
HELPER_READY=1
ok "租户侧 RR 库助手就绪（口令只走 stdin）"

# 上一轮失败留下的、属于本脚本的行：先删干净，否则 id 算不准。只删自己的名字前缀。
list_out=$(call_ctrl list '{}') || die "读不到 RR 库"
stale=""
for id in $(printf '%s\n' "$list_out" | awk -v p="$NAME_PREFIX" '$1=="ROW" && index($3,p)==1 {print $2}'); do
    stale="$stale $id"
done
if [ -n "$stale" ]; then
    call_ctrl delete "{\"ids\": $(python3 -c 'import json,sys;print(json.dumps(sys.argv[1:]))' $stale)}" >/dev/null ||
        die "删不掉上一轮留下的行：$stale（可能被设备的 RR 选择占用）"
    note "清掉了上一轮留下的行：$stale"
fi

# ---------------------------------------------------------------- 1 两条行

info "(1) 建两条 RR 行并签发身份：网格从控制器长出来，每个对端只有一条会话"
create_out=$(call_ctrl create "{\"rrs\":[{\"name\":\"${NAME_PREFIX}a\",\"addr\":\"$RR_A_ADDR\"},{\"name\":\"${NAME_PREFIX}b\",\"addr\":\"$RR_B_ADDR\"}]}") ||
    die "建 RR 行失败"
I1=$(row_id "$create_out" "${NAME_PREFIX}a"); I2=$(row_id "$create_out" "${NAME_PREFIX}b")
[ -n "$I1" ] && [ -n "$I2" ] || die "拿不到 RR 行 id：$create_out"
CREATED_IDS="$I1 $I2"
[ "$I1" != "$I2" ] || die "两条行拿到同一个 id"
SORTED=$(printf '%s\n%s\n' "$I1" "$I2" | LC_ALL=C sort)
FIRST=$(printf '%s\n' "$SORTED" | head -1)
if [ "$FIRST" = "$I1" ]; then
    DIALER_ST=$RR_A_ST; DIALER_ID=$I1; LISTENER_ST=$RR_B_ST; LISTENER_ID=$I2
else
    DIALER_ST=$RR_B_ST; DIALER_ID=$I2; LISTENER_ST=$RR_A_ST; LISTENER_ID=$I1
fi
note "RR 行：$I1 → $RR_A_ADDR、$I2 → $RR_B_ADDR；按 id 次序，$DIALER_ID 拨 $LISTENER_ID"

for pair in "$I1:$RR_A_ADDR" "$I2:$RR_B_ADDR"; do
    id=${pair%%:*}
    id_out=$(call_ctrl identity "{\"id\":\"$id\",\"dir\":\"$WORK/id/$id\"}") ||
        die "签发 $id 失败"
    printf '%s\n' "$id_out" | awk '$1=="ID" {printf "       %s 指纹 %s serial %s\n", $2, $3, $4}'
done

start_rr mesh-a "$I1" "$RR_A_PORT" "$RR_A_ST"
start_rr mesh-b "$I2" "$RR_B_PORT" "$RR_B_ST"

wait_json "$RR" "$(rr_url "$DIALER_ST" status)" \
    "[(p['id'], p['dialing'], p['connected']) for p in d['peers']] == [('$LISTENER_ID', True, True)] and
     len([s for s in d['sessions'] if s['key'] == 'rr/$LISTENER_ID']) == 1 and
     [s for s in d['sessions'] if s['key'] == 'rr/$LISTENER_ID'][0]['synced']" \
    "$DIALER_ID 拨 $LISTENER_ID：peer 表里它 dialing=true、connected=true，会话只有一条且已 synced" 20
wait_json "$RR" "$(rr_url "$LISTENER_ST" status)" \
    "[(p['id'], p['dialing'], p['connected']) for p in d['peers']] == [('$DIALER_ID', False, True)] and
     len([s for s in d['sessions'] if s['key'] == 'rr/$DIALER_ID']) == 1 and
     [s for s in d['sessions'] if s['key'] == 'rr/$DIALER_ID'][0]['synced']" \
    "$LISTENER_ID 收到同一条会话：dialing=false，没有第二条对拨" 20

SINCE12_D=$(since_of "$DIALER_ST" "$LISTENER_ID"); need "$SINCE12_D" "网格会话 since"
SINCE12_L=$(since_of "$LISTENER_ST" "$DIALER_ID"); need "$SINCE12_L" "网格会话 since"
note "网格会话 since：$DIALER_ID 侧 $SINCE12_D，$LISTENER_ID 侧 $SINCE12_L"

# 控制器账本：两条行都必须真的被拉过，否则网格不是"从控制器长出来的"。
list_out=$(call_ctrl list '{}')
for id in "$I1" "$I2"; do
    f=$(row_fetch "$list_out" "$id")
    [ -n "$f" ] && [ "$f" != "0" ] &&
        ok "$id 的 last_fetch_at_unix = $f：这台 RR 真的来拉过邻居清单" ||
        bad "$id 没有拉取记录（last_fetch=$f）"
done

# ---------------------------------------------------------------- 2 跨网格反射

info "(2) 两台设备各只挂一台 RR：前缀必须跨过网格互相学过去"
start_node "$N1" 1 lab-cpe1 "$N1_ST" "$I1" "$RR_A_ADDR" 2 10.99.0.12 "$PFX1"
start_node "$N2" 2 lab-cpe2 "$N2_ST" "$I2" "$RR_B_ADDR" 1 10.99.0.11 "$PFX2"

wait_json "$N1" "$(n1_url status)" \
    "d['rib_ready'] and len(d['sessions']) == 1 and d['sessions'][0]['addr'] == '$RR_A_ADDR' and
     '$PFX2' in [r['prefix'] for v in d['rib'].values() for r in v]" \
    "node1（只挂 $I1）学到了 node2 的前缀 $PFX2：只能是 $I2 从网格里反射过来的" 25
wait_json "$N2" "$(n2_url status)" \
    "d['rib_ready'] and len(d['sessions']) == 1 and d['sessions'][0]['addr'] == '$RR_B_ADDR' and
     '$PFX1' in [r['prefix'] for v in d['rib'].values() for r in v]" \
    "node2（只挂 $I2）学到了 node1 的前缀 $PFX1：反方向同样靠网格" 25

wait_json "$RR" "$(rr_url "$RR_A_ST" routes)" \
    "'$PFX1' in [r['prefix'] for r in d.get('1', [])] and '$PFX2' in [r['prefix'] for r in d.get('2', [])]" \
    "RR-A 的表里两台设备都有（device 2 是经网格来的）" 20
wait_json "$RR" "$(rr_url "$RR_B_ST" routes)" \
    "'$PFX1' in [r['prefix'] for r in d.get('1', [])] and '$PFX2' in [r['prefix'] for r in d.get('2', [])]" \
    "RR-B 的表里两台设备也都有（device 1 是经网格来的）" 20
wait_json "$RR" "$(rr_url "$RR_A_ST" status)" \
    "not [s for s in d['sessions'] if s['device_id'] == 2]" \
    "反面：RR-A 上没有 device 2 的客户端会话 —— 那条路由真的是网格给的" 10
wait_json "$RR" "$(rr_url "$RR_B_ST" status)" \
    "not [s for s in d['sessions'] if s['device_id'] == 1]" \
    "反面：RR-B 上没有 device 1 的客户端会话" 10

stamp_n1=$(stamp_of "$N1" "$WORK/node1.pid"); need "$stamp_n1" "node1 pid"
stamp_n2=$(stamp_of "$N2" "$WORK/node2.pid"); need "$stamp_n2" "node2 pid"
w1_0=$(field "$N1" "$(n1_url status)" "d['withdrawn']"); need "$w1_0" "node1 withdrawn"
w2_0=$(field "$N2" "$(n2_url status)" "d['withdrawn']"); need "$w2_0" "node2 withdrawn"
note "node1 = $stamp_n1，node2 = $stamp_n2；withdrawn 起点 $w1_0 / $w2_0"

# ---------------------------------------------------------------- 3 加第三条

info "(3) 只增不改：加第三条 RR 行，旧会话不许重建"
create_out=$(call_ctrl create "{\"rrs\":[{\"name\":\"${NAME_PREFIX}c\",\"addr\":\"$RR_C_ADDR\"}]}") ||
    die "建第三条 RR 行失败"
I3=$(row_id "$create_out" "${NAME_PREFIX}c"); [ -n "$I3" ] || die "拿不到第三条行 id：$create_out"
CREATED_IDS="$I1 $I2 $I3"
ISORT=$(printf '%s\n%s\n%s\n' "$I1" "$I2" "$I3" | LC_ALL=C sort)
[ "$(printf '%s\n' "$ISORT" | tail -1)" = "$I3" ] ||
    die "第三条行的 id（$I3）不在字典序最后：$ISORT —— 这条脚本按 $I1<$I2<$I3 假设，别再跑了"
id_out=$(call_ctrl identity "{\"id\":\"$I3\",\"dir\":\"$WORK/id/$I3\"}") || die "签发 $I3 失败"
printf '%s\n' "$id_out" | awk '$1=="ID" {printf "       %s 指纹 %s serial %s\n", $2, $3, $4}'
start_rr mesh-c "$I3" "$RR_C_PORT" "$RR_C_ST"

wait_json "$RR" "$(rr_url "$RR_C_ST" status)" \
    "sorted((p['id'], p['dialing'], p['connected']) for p in d['peers']) ==
       [('$I1', False, True), ('$I2', False, True)] and
     sorted(s['key'] for s in d['sessions']) == ['rr/$I1', 'rr/$I2']" \
    "$I3 刚起来就收到 $I1 与 $I2 拨来的会话（它自己一条都不拨）" 25
wait_json "$RR" "$(rr_url "$RR_A_ST" status)" \
    "sorted((p['id'], p['dialing'], p['connected']) for p in d['peers']) ==
       [('$I2', True, True), ('$I3', True, True)] and
     len([s for s in d['sessions'] if s['key'] == 'rr/$I3']) == 1" \
    "RR-A 多出 $I3 一条（dialing=true），对 $I2 仍然只有原来那一条" 25
wait_json "$RR" "$(rr_url "$RR_B_ST" status)" \
    "sorted((p['id'], p['dialing'], p['connected']) for p in d['peers']) ==
       [('$I1', False, True), ('$I3', True, True)] and
     len([s for s in d['sessions'] if s['key'] == 'rr/$I3']) == 1" \
    "RR-B 同样只多 $I3 一条，对 $I1 仍然只有原来那一条" 25
wait_json "$RR" "$(rr_url "$RR_C_ST" routes)" \
    "'$PFX1' in [r['prefix'] for r in d.get('1', [])] and '$PFX2' in [r['prefix'] for r in d.get('2', [])]" \
    "$I3 通过网格快照拿到了两张设备表（新 RR 不用设备重新宣告）" 20

SINCE12_D_AFTER=$(since_of "$DIALER_ST" "$LISTENER_ID")
SINCE12_L_AFTER=$(since_of "$LISTENER_ST" "$DIALER_ID")
[ "$SINCE12_D_AFTER" = "$SINCE12_D" ] &&
    ok "$DIALER_ID 侧对 $LISTENER_ID 的会话 since 没变（$SINCE12_D）：加第三台没有重建它" ||
    bad "旧网格会话被重建了：$DIALER_ID 侧 $SINCE12_D → $SINCE12_D_AFTER"
[ "$SINCE12_L_AFTER" = "$SINCE12_L" ] &&
    ok "$LISTENER_ID 侧对 $DIALER_ID 的会话 since 没变（$SINCE12_L）" ||
    bad "旧网格会话被重建了：$LISTENER_ID 侧 $SINCE12_L → $SINCE12_L_AFTER"

w1_1=$(field "$N1" "$(n1_url status)" "d['withdrawn']")
w2_1=$(field "$N2" "$(n2_url status)" "d['withdrawn']")
[ "$w1_1" = "$w1_0" ] && ok "node1 withdrawn 没动（$w1_0）：加 RR 不动设备的路由表" ||
    bad "node1 withdrawn 从 $w1_0 变成 $w1_1"
[ "$w2_1" = "$w2_0" ] && ok "node2 withdrawn 没动（$w2_0）" ||
    bad "node2 withdrawn 从 $w2_0 变成 $w2_1"

# ---------------------------------------------------------------- 4 删第三条

info "(4) 删一行 = 这台 RR 不再存在：404 → 自己退出，网格把它摘掉"
del_out=$(call_ctrl delete "{\"ids\":[\"$I3\"]}") || die "删第三条行失败"
printf '%s\n' "$del_out" | awk '$1=="DEL" {printf "       删 %s → HTTP %s\n", $2, $3}'
CREATED_IDS="$I1 $I2"
[ "$(printf '%s\n' "$del_out" | awk '$1=="DEL" {print $3}')" = "200" ] &&
    ok "删行返回 200" || bad "删行没成功：$del_out"

i=0
while [ "$i" -lt 50 ] && ! dx "$RR" "[ -f $WORK/mesh-c.exit ]" >/dev/null 2>&1; do
    sleep 0.2; i=$((i + 1))
done
if dx "$RR" "[ -f $WORK/mesh-c.exit ]" >/dev/null 2>&1; then
    code=$(dx "$RR" "cat $WORK/mesh-c.exit" | tr -d ' ')
    [ "$code" = "1" ] && ok "$I3 以退出码 1 停下（不是留着当幽灵 RR）" ||
        bad "$I3 的退出码是 $code，想要 1"
    line=$(dx "$RR" "grep -m1 'stopping' $WORK/mesh-c.log" 2>/dev/null)
    [ -n "$line" ] && ok "$I3 的日志写明了原因：$line" || bad "$I3 的日志里没有 stopping 行"
else
    bad "$I3 在 $((50 / 5))s 内没有退出"
fi

wait_json "$RR" "$(rr_url "$RR_A_ST" status)" \
    "sorted(p['id'] for p in d['peers']) == ['$I2'] and
     not [s for s in d['sessions'] if s['key'] == 'rr/$I3'] and
     len([s for s in d['sessions'] if s['key'] == 'rr/$I2']) == 1" \
    "RR-A 把 $I3 摘掉了（peer 表与会话表都干净），对 $I2 的会话还在" 15
wait_json "$RR" "$(rr_url "$RR_B_ST" status)" \
    "sorted(p['id'] for p in d['peers']) == ['$I1'] and
     not [s for s in d['sessions'] if s['key'] == 'rr/$I3'] and
     len([s for s in d['sessions'] if s['key'] == 'rr/$I1']) == 1" \
    "RR-B 同样摘干净，对 $I1 的会话还在" 15

SINCE12_D_END=$(since_of "$DIALER_ST" "$LISTENER_ID")
SINCE12_L_END=$(since_of "$LISTENER_ST" "$DIALER_ID")
[ "$SINCE12_D_END" = "$SINCE12_D" ] &&
    ok "删掉 $I3 之后，$DIALER_ID 对 $LISTENER_ID 的会话 since 还是 $SINCE12_D" ||
    bad "删 $I3 把旧网格会话也拆了：$SINCE12_D → $SINCE12_D_END"
[ "$SINCE12_L_END" = "$SINCE12_L" ] &&
    ok "删掉 $I3 之后，$LISTENER_ID 侧的会话 since 也还是 $SINCE12_L" ||
    bad "删 $I3 把旧网格会话也拆了：$SINCE12_L → $SINCE12_L_END"

has_pfx "$N1" "$(n1_url status)" "$PFX2" && ok "node1 的表里 $PFX2 还在" ||
    bad "node1 丢了 $PFX2：删一台 RR 不该动设备的路由"
has_pfx "$N2" "$(n2_url status)" "$PFX1" && ok "node2 的表里 $PFX1 还在" ||
    bad "node2 丢了 $PFX1"
w1_end=$(field "$N1" "$(n1_url status)" "d['withdrawn']")
w2_end=$(field "$N2" "$(n2_url status)" "d['withdrawn']")
[ "$w1_end" = "$w1_0" ] && ok "node1 全程 withdrawn = $w1_0" || bad "node1 withdrawn 变成 $w1_end"
[ "$w2_end" = "$w2_0" ] && ok "node2 全程 withdrawn = $w2_0" || bad "node2 withdrawn 变成 $w2_end"

stamp_n1_end=$(stamp_of "$N1" "$WORK/node1.pid")
stamp_n2_end=$(stamp_of "$N2" "$WORK/node2.pid")
[ "$stamp_n1" = "$stamp_n1_end" ] && ok "node1 的 sr-route 全程同一个进程（$stamp_n1_end）" ||
    bad "node1 的 sr-route 换了：$stamp_n1 → $stamp_n1_end"
[ "$stamp_n2" = "$stamp_n2_end" ] && ok "node2 的 sr-route 全程同一个进程（$stamp_n2_end）" ||
    bad "node2 的 sr-route 换了：$stamp_n2 → $stamp_n2_end"

list_out=$(call_ctrl list '{}')
rows_left=$(printf '%s\n' "$list_out" | awk -v p="$NAME_PREFIX" '$1=="ROW" && index($3,p)==1' | wc -l | tr -d ' ')
[ "$rows_left" = "2" ] && ok "RR 库里只剩本脚本的两行（$I1、$I2）" ||
    bad "RR 库里的测试行数量是 $rows_left，想要 2"

echo
echo "== 结果：$PASS passed, $FAIL failed"
if [ "$FAIL" -ne 0 ]; then
    for pair in "$RR:mesh-a mesh-b mesh-c" "$N1:node1" "$N2:node2"; do
        c=${pair%%:*}
        for f in ${pair#*:}; do
            echo "---- $c/$f.log" >&2
            dx "$c" "tail -25 $WORK/$f.log" >&2 2>/dev/null || true
        done
    done
fi
if [ "$KEEP" = 1 ]; then
    trap - EXIT
    echo "== --keep：三台 RR 与两台设备进程、两条 RR 行都留着"
    echo "   撤的时候：TENANT_USER=... TENANT_PASS=... 手动删掉 RR 行 $I1 / $I2，再 kill 掉各容器 $WORK 里的进程"
fi
[ "$FAIL" -eq 0 ]
