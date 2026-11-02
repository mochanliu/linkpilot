#!/bin/bash
# test-rr-switch.sh - O6-c C-c 判据：换 RR 的时候，会话不能断、表不能空
#
# E1 拍的是"运营方逐台更换 RR 可以不断业务"。这条脚本在 lab 里起**三个真的 sr-rr
# 进程**和**两台真的设备**，把这件事拆成两个必须分开的问题，因为 lab 证明它们不是
# 一回事：
#
#   问题一（C-c 的机制）：**这台设备自己**换 RR 的瞬间，它的表会不会空？
#   问题二（协议的事实）：换过去的那台 RR **有没有它需要的路由**？
#
# C-c 管的是问题一：新会话没成立之前，旧的会话一条都不摘（make-before-break）。
# 问题二它管不着 —— 反射器的表是从各设备实时宣告建起来的，一台谁都没挂过的新 RR
# 接管一台设备，那台设备就会看不到还没在这台 RR 上出现过的对端。所以脚本必须把两
# 件事分别量出来，不能糊在一起报"通过"：
#
#   (1) 稳态：两台设备都只挂 RR-A（= 租户改之前的那份配置），互相看得见。
#   (2) **第一步（先加新，全租户同时）**：node1、node2 都 PUT /rrs [A, B]。
#       判据：两台的表全程不缺路由、每台设备的前缀全程至少在一个 RR 上、两台会话的
#       旧连接都没被拆过重连（RR 记的 since 一动不动）。
#   (3) **运营方该看的那一眼**：RR-B 的 /routes 里现在**两台设备都在** ⇒ 这时才允许撤旧。
#   (4) **第二步（撤旧）**：node1 先 PUT [B]，隔 1 秒再 node2 PUT [B]。
#       node1 是"被换的那台"：它的表**一条样本都不该缺**（B 上什么都有）—— 这就是 E1。
#       node2 此刻还是旧配置 [A, B]，两条会话都在往同一张 Loc-RIB 里送：A 撤走它自己
#       那份 device 1 的拷贝，B 的那份顶上，**前缀不离开表、样本一条都不缺**。
#       旧模型（从"挑中的那一条"会话读表）下这里会缺满一次扇出间隔；O7 把读路径改成
#       合并所有会话再选优之后，这道缺口在产品上就不存在了 —— 所以它是硬指标，不是
#       允许出现的数字（对齐 BGP：撤一条 adj-RIB-in 不等于撤一条路由）。
#       换 RR 的正确走法仍是 (2)(3)(4)：先加、等新 RR 收全、再撤。
#   (5) 收尾状态：两台都只挂 B，RR-A 上干净。
#   (6) **冷 RR 的反例**：node1 PUT [C]，而 C 这个进程还没启动 ⇒ 这是一段**想停多久
#       停多久**的确定状态：rr_wait 挂着 pending=[C]、retiring=[B]，`rib_ready` 正由
#       那条 retiring 的会话供着，表还是满的（机制在这里最硬，且不依赖任何采样时刻）。
#       然后才启动 C：node1 不 PUT 第二次，靠自己的重连退避接上 C，B 随即被摘 ——
#       会话没断，但表里 device 2 那条**真的没了**，因为 C 上从来就没有它。
#       接着 node2 也 PUT [C]：node1 那条路由**在没有第三次 PUT 的情况下自己回来**。
#       这两步把问题一和问题二分得干干净净：机制一次都没断，缺的是新 RR 的内容。
#   (7) 负向：空集合 / 缺 addr / 重复 addr / 未知字段都必须 400，且被拒之后 GET /rrs
#       回来的还是原来那份 —— 一次失败的 PUT 不许把盒子留在没有 RR 的状态。
#   (8) 全程 sr-route 的 pid + 启动时刻不变（只比 pid 会被复用骗）；PUT 只改内存，
#       磁盘那份 route.json 不动（配置以控制器发布为准）。
#
# "断没断"用的三样证据，前两条不依赖采样时刻：
#   1. 每台的 `withdrawn` 计数器：它是事件形状的，本机每处理一次撤路由才加一。
#   2. 两台 RR 各自的会话 since：接管用的是既有会话还是新连接的证据。
#   3. 采样器每 ~20ms 一条（两台设备的表 + 三台 RR 对两台设备的可见性），用来给出
#      缺口时长和重叠窗口这两个**数字**，而不是只给一个"没断"。
#
# 前置：lab-o6b1（device 1）与 lab-o6b2（device 2）里有入网签好的证书（租户
# o6b-lab），lab-rr 里有 CA 与 rr.crt（SAN 覆盖 10.99.0.100，换端口不用重签）。
# 那对盒子不在了也不要紧：主线 lab 的 lab-cpe1/2（租户 tenant-a）同样够用 ——
#   N1=lab-cpe1 N2=lab-cpe2 N2_IP=10.99.0.12 TENANT=tenant-a test/scripts/test-rr-switch.sh
# 这两台盒子里正在跑的 sr-route / sr-agent 本脚本**一概不动**：另起自己的进程、
# 自己的端口、自己的配置。lab-rr 上给 tenant-a 跑着的那个 sr-rr 同样不碰。
#
# 必须在**宿主机**上跑（需要 docker CLI）：
#   test/scripts/test-rr-switch.sh [--keep]
# --keep 保留三台 RR 与两台设备进程、以及各容器 $WORK 里的配置、日志与采样。
set -u

RR=${RR:-lab-rr}
N1=${N1:-lab-o6b1}                # device 1：被换的那台
N2=${N2:-lab-o6b2}                # device 2：另一台（并集之后再撤旧的那台：它一条样本都不许缺）
CTRL=${CTRL:-lab-ctrl}            # 只用来编译
MODULE=${MODULE:-/workspace/controller}
WORK=${WORK:-/tmp/sr-rr-switch}

RR_IP=${RR_IP:-10.99.0.100}
N2_IP=${N2_IP:-10.99.0.4}                  # 采样器在 node1 的容器里，读 node2 要用它的容器地址
TENANT=${TENANT:-o6b-lab}         # 必须等于两台盒子证书里的租户
RT=${RT:-65000:1001}
PFX1=${PFX1:-10.77.77.0/24}       # device 1 宣告
PFX2=${PFX2:-10.88.88.0/24}       # device 2 宣告

RR_A_PORT=18866; RR_A_ST=18867
RR_B_PORT=18876; RR_B_ST=18877
RR_C_PORT=18886; RR_C_ST=18887
N1_ST=18881
N2_ST=18882
RR_A_ADDR=$RR_IP:$RR_A_PORT
RR_B_ADDR=$RR_IP:$RR_B_PORT
RR_C_ADDR=$RR_IP:$RR_C_PORT
FANOUT_SLEEP=${FANOUT_SLEEP:-1}   # (4) 里两次 PUT 之间的间隔：以前 node2 的缺口等于它，现在是 0

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

# push_local <container> <status port> <prefix> <label> —— 扮演 sr-frr：sr-route 只从
# PUT /local-prefixes 收本端前缀。进程刚起时端口还没 listen，curl 直接失败不是被测
# 行为，所以要重试。
push_local() {
    local c=$1 st=$2 pfx=$3 label=$4 i=0
    while [ "$i" -lt 50 ]; do
        if dx "$c" "curl -sf --max-time 2 -X PUT http://127.0.0.1:$st/local-prefixes \
            -H 'Content-Type: application/json' \
            -d '{\"prefixes\":[{\"vrf_id\":0,\"prefix\":\"$pfx\"}]}'" >/dev/null 2>&1; then
            ok "$label"
            return 0
        fi
        sleep 0.2
        i=$((i+1))
    done
    bad "$label（超时）"
    return 1
}

# ---------------------------------------------------------------- 状态口读法

get_url() {   # <container> <url> —— 读不到就空，让调用方按"还没好"处理
    dx "$1" "curl -sf --max-time 2 '$2'" 2>/dev/null
}
n1_url() { printf 'http://127.0.0.1:%s/%s' "$N1_ST" "$1"; }
n2_url() { printf 'http://127.0.0.1:%s/%s' "$N2_ST" "$1"; }
rr_url() { printf 'http://127.0.0.1:%s/%s' "$1" "$2"; }   # <status port> <path>

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
    bad "$label (超时)"
    get_url "$c" "$url" | python3 -m json.tool 2>/dev/null | head -40 >&2
    return 1
}

# field <container> <url> <python expr> —— 取一个标量留着后面比（会话 since 跨阶段比）
field() {
    local v
    v=$(get_url "$1" "$2" | python3 -c "import json,sys; d=json.load(sys.stdin); print($3)" 2>/dev/null)
    [ -n "$v" ] || v=-
    printf '%s\n' "$v"
}
# 读不到的标量一律记成 -，后面每次比较都先确认它不是 -，免得把"没读到"当成"变了"。
need() { [ -n "$1" ] && [ "$1" != "-" ] || die "读不到 $2（$1）"; }

rrs_put() {   # <container> <url> <json body> —— PUT /rrs，回显 http code；正文在 $WORK/resp.body
    local c=$1 url=$2
    dxi "$c" "curl -s -o $WORK/resp.body -w '%{http_code}' -X PUT --data-binary @- '$url'" <<< "$3"
}
resp_body() { dx "$1" "head -c 200 $WORK/resp.body" 2>/dev/null | tr -d '\n'; }

stamp_of() {   # <container> <pid file> —— pid + 启动时刻，只比 pid 会被复用骗
    dx "$1" "p=\$(cat $2 2>/dev/null); ps -o pid=,lstart= -p \$p 2>/dev/null | tr -s ' '"
}

# 会话 since：<rr status port> <device id>
since_of() {
    field "$RR" "$(rr_url "$1" status)" \
        "[s['since'] for s in d['sessions'] if s['device_id'] == $2][0]"
}
# 本机表里有没有这条前缀：<container> <status url> <prefix>
has_pfx() { get_url "$1" "$2" | python3 -c "
import json,sys
d=json.load(sys.stdin)
sys.exit(0 if '$3' in [r['prefix'] for v in d['rib'].values() for r in v] else 1)" >/dev/null 2>&1; }

# ---------------------------------------------------------------- 起进程

start_rr() {   # <name> <listen port> <status port>
    local name=$1 listen=$2 st=$3
    dxi "$RR" "cat > $WORK/$name.json" <<JSON
{
  "rr_id": "$name",
  "tenant": "$TENANT",
  "hold_sec": 12,
  "listen": "0.0.0.0:$listen",
  "status_addr": "0.0.0.0:$st",
  "tls": {"cert_file": "/etc/sr/rr.crt", "key_file": "/etc/sr/rr.key", "ca_file": "/etc/sr/ca.pem"}
}
JSON
    dx "$RR" "cd $WORK && nohup ./bin/sr-rr -config $WORK/$name.json >> $WORK/$name.log 2>&1 & echo \$! > $WORK/$name.pid" >/dev/null ||
        die "起 $name 失败"
    wait_json "$RR" "$(rr_url "$st" status)" "d['rr_id'] == '$name'" \
        "sr-rr $name 起来了（听 $listen，状态口 $st）" 15 >/dev/null
}

start_node() {   # <container> <device id> <serial> <status port> <rrs json>
    local c=$1 dev=$2 serial=$3 st=$4 rrs=$5 peer nh pfx
    if [ "$dev" = 1 ]; then peer=2; nh=10.99.0.4; pfx=$PFX1; else peer=1; nh=10.99.0.2; pfx=$PFX2; fi
    dxi "$c" "cat > $WORK/node$dev.json" <<JSON
{
  "device_id": $dev,
  "tenant": "$TENANT",
  "serial": "$serial",
  "identity_dir": "/etc/sdwan/identity",
  "hold_sec": 6,
  "status_addr": "0.0.0.0:$st",
  "rrs": $rrs,
  "tunnels": [{"peer_device_id": $peer, "next_hop": "$nh"}],
  "vrfs": [{"id": 0, "export_rts": ["$RT"], "import_rts": ["$RT"]}]
}
JSON
    dx "$c" "cd $WORK && nohup ./bin/sr-route -config $WORK/node$dev.json > $WORK/node$dev.log 2>&1 & echo \$! > $WORK/node$dev.pid" >/dev/null ||
        die "起 device $dev 的 sr-route 失败"
    wait_json "$c" "http://127.0.0.1:$st/status" "d['device_id'] == $dev" \
        "device $dev 的 sr-route 起来了（状态口 $st）" 15 >/dev/null
    push_local "$c" "$st" "$pfx" "device $dev 宣告 $pfx（扮演 sr-frr）" ||
        die "device $dev 的前缀推不进去"
}

cleanup() {
    [ "$KEEP" = 1 ] && return 0
    local pair c f p
    for pair in "$RR:rr-a rr-b rr-c" "$N1:node1 sampler" "$N2:node2"; do
        c=${pair%%:*}
        for f in ${pair#*:}; do
            # nohup 起来的是一层 sh，pid 文件里是它的 pid：连子进程一起收，否则端口留着
            p=$(dx "$c" "cat $WORK/$f.pid 2>/dev/null" 2>/dev/null | tr -d ' ')
            [ -n "$p" ] && dx "$c" "pkill -P $p 2>/dev/null; kill $p 2>/dev/null" >/dev/null 2>&1
            dx "$c" "pkill -f '$WORK/$f.json' 2>/dev/null; pkill -f '$WORK/$f.py' 2>/dev/null" >/dev/null 2>&1
        done
    done
}
trap cleanup EXIT

# ---------------------------------------------------------------- 0 准备

command -v docker >/dev/null 2>&1 || die "这条脚本要在**宿主机**上跑（需要 docker CLI）"
for c in "$RR" "$N1" "$N2" "$CTRL"; do
    docker inspect "$c" >/dev/null 2>&1 || die "容器 $c 不在：先起 lab（test/lab/bringup.sh）"
done
for pair in "$N1:/etc/sdwan/identity" "$N2:/etc/sdwan/identity" "$RR:/etc/sr/rr.crt"; do
    c=${pair%%:*}; p=${pair#*:}
    dx "$c" "[ -e $p ]" >/dev/null 2>&1 || die "$c 里没有 $p：这两台盒子还没入网，或 RR 缺证书"
done

info "(0) 编译二进制、准备 $WORK、确认端口没人占"
dx "$CTRL" "cd $MODULE && go build -o bin/sr-rr ./cmd/sr-rr && go build -o bin/sr-route ./cmd/sr-route" >/dev/null ||
    die "编译失败"
for c in "$RR" "$N1" "$N2"; do
    dx "$c" "rm -rf $WORK && mkdir -p $WORK/bin && cp -f $MODULE/bin/sr-rr $MODULE/bin/sr-route $WORK/bin/" >/dev/null ||
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
ok "二进制就绪，六个端口全空"

start_rr rr-a "$RR_A_PORT" "$RR_A_ST"
start_rr rr-b "$RR_B_PORT" "$RR_B_ST"
note "A=$RR_A_ADDR、B=$RR_B_ADDR 在听；RR-C（$RR_C_ADDR）到 (6) 才起"

start_node "$N1" 1 O6B-1 "$N1_ST" "[{\"id\":\"rr-a\",\"addr\":\"$RR_A_ADDR\"}]"
start_node "$N2" 2 O6B-2 "$N2_ST" "[{\"id\":\"rr-a\",\"addr\":\"$RR_A_ADDR\"}]"

info "(1) 稳态：两台都只挂 A，并且互相看得见"
wait_json "$N1" "$(n1_url status)" \
    "d['rib_ready'] and len(d['sessions']) == 1 and d['sessions'][0]['addr'] == '$RR_A_ADDR' and
     '$PFX2' in [r['prefix'] for v in d['rib'].values() for r in v]" \
    "device 1 从 A 学到了 device 2 的 $PFX2" 25
wait_json "$N2" "$(n2_url status)" \
    "d['rib_ready'] and '$PFX1' in [r['prefix'] for v in d['rib'].values() for r in v]" \
    "device 2 从 A 学到了 device 1 的 $PFX1 —— 业务在跑的就是这两条" 25
wait_json "$RR" "$(rr_url "$RR_B_ST" routes)" "not d" "RR-B 现在还是空的：谁都没挂它" 10

stamp_n1=$(stamp_of "$N1" "$WORK/node1.pid")
stamp_n2=$(stamp_of "$N2" "$WORK/node2.pid")
w1_0=$(field "$N1" "$(n1_url status)" "d['withdrawn']"); need "$w1_0" "device 1 的 withdrawn 起点"
w2_0=$(field "$N2" "$(n2_url status)" "d['withdrawn']"); need "$w2_0" "device 2 的 withdrawn 起点"
note "device 1 = $stamp_n1，device 2 = $stamp_n2；withdrawn 起点 $w1_0 / $w2_0"

# ---------------------------------------------------------------- 采样器

dxi "$N1" "cat > $WORK/sampler.py" <<'PY'
# 每 20ms 采一条：两台设备的 /status + 三台 RR 的 /routes（键是设备号，值是它宣告的路由）。
import json, os, sys, time, urllib.request

csv, stop, pfx1, pfx2 = sys.argv[1:5]
urls = sys.argv[5].split(",")
COLS = ["ts_ms", "n1_ok", "n2_ok", "n1_ready", "n2_ready", "n1_has", "n2_has",
        "n1_wd", "n2_wd", "a_d1", "a_d2", "b_d1", "b_d2", "c_d1", "c_d2", "any_d1", "any_d2"]


def get(u):
    try:
        with urllib.request.urlopen(u, timeout=0.5) as r:
            return json.loads(r.read().decode())
    except Exception:
        return None


def has(routes, pfx):
    return 1 if pfx in [r.get("prefix") for r in (routes or [])] else 0


def row_of(st, rib_pfx):
    rib = (st or {}).get("rib") or {}
    return {
        "ok": 1 if st else 0,
        "ready": 1 if (st or {}).get("rib_ready") else 0,
        "has": 1 if rib_pfx in [r.get("prefix") for v in rib.values() for r in v] else 0,
        "wd": (st or {}).get("withdrawn", -1),
    }


out = open(csv, "w", buffering=1)
out.write("#" + ",".join(COLS) + "\n")
while not os.path.exists(stop):
    s1, s2 = get(urls[0]), get(urls[1])
    rrs = [get(u) for u in urls[2:]]
    x1, x2 = row_of(s1, pfx2), row_of(s2, pfx1)
    a, b, c = [(has((r or {}).get("1"), pfx1), has((r or {}).get("2"), pfx2)) for r in rrs]
    out.write(",".join(str(v) for v in [
        "%.0f" % (time.time() * 1000), x1["ok"], x2["ok"], x1["ready"], x2["ready"],
        x1["has"], x2["has"], x1["wd"], x2["wd"],
        a[0], a[1], b[0], b[1], c[0], c[1],
        1 if (a[0] or b[0] or c[0]) else 0,
        1 if (a[1] or b[1] or c[1]) else 0,
    ]) + "\n")
    time.sleep(0.02)
out.close()
PY

dxi "$N1" "cat > $WORK/window.py" <<'PY'
# 在 [from_ms, to_ms] 这段采样里算四个数字：缺样本条数、最长缺口时长、
# 两台 RR 同时持有的条数、withdrawn 是否一动不。判定交给 bash，这里只给数。
import sys

lines = [l.strip() for l in open(sys.argv[1]) if l.strip()]
head = lines[0].lstrip("#").split(",")
rows = [dict(zip(head, l.split(","))) for l in lines[1:]]
lo, hi = float(sys.argv[2]), float(sys.argv[3])
w = [r for r in rows if lo <= float(r["ts_ms"]) <= hi]
if not w:
    print("samples=0"); sys.exit(0)


def longest(col):
    """该列为 0 的最长连续区间时长（ms），以及条数。"""
    best, run_start, n = 0, None, 0
    for r in w + [{"ts_ms": str(float(w[-1]["ts_ms"]) + 1), col: "1"}]:
        if r[col] == "1":
            if run_start is not None:
                best = max(best, int(float(r["ts_ms"]) - run_start))
                run_start = None
        elif run_start is None:
            run_start = float(r["ts_ms"])
            n += 1
        else:
            n += 1
    return best, n


def flat(col):
    v = sorted(set(r[col] for r in w))
    return "%s..%s" % (v[0], v[-1]) if len(v) > 1 else v[0]


d1_gap, d1_n = longest("any_d1")
d2_gap, d2_n = longest("any_d2")
print("samples=%d span_ms=%d" % (len(w), float(w[-1]["ts_ms"]) - float(w[0]["ts_ms"])))
print("n1_ok=%s n2_ok=%s" % (flat("n1_ok"), flat("n2_ok")))
print("n1_has=%s n1_gap=%d n1_missing=%d" % (flat("n1_has"), longest("n1_has")[0], longest("n1_has")[1]))
print("n2_has=%s n2_gap=%d n2_missing=%d" % (flat("n2_has"), longest("n2_has")[0], longest("n2_has")[1]))
print("n1_ready=%s n2_ready=%s" % (flat("n1_ready"), flat("n2_ready")))
print("any_d1=%s any_d1_gap=%d any_d2=%s any_d2_gap=%d" % (flat("any_d1"), d1_gap, flat("any_d2"), d2_gap))
print("both_ab_d1=%d both_bc_d1=%d" % (
    sum(1 for r in w if r["a_d1"] == "1" and r["b_d1"] == "1"),
    sum(1 for r in w if r["b_d1"] == "1" and r["c_d1"] == "1")))
print("n1_wd=%s n2_wd=%s" % (flat("n1_wd"), flat("n2_wd")))
PY

dxi "$RR" "cat > $WORK/overlap.py" <<'PY'
# 重叠时长 = (旧 RR 上这条会话的 since + 它日志里的存续时长) - 新 RR 上该会话的 since。
# 三个输入都是两个进程自己记的账，不靠采样时刻，所以它比 CSV 里的重叠条数更硬。
import datetime, re, sys

since_old, since_new, line = sys.argv[1], sys.argv[2], sys.argv[3]
m = re.search(r'session ended after ([0-9.]+(?:ns|us|ms|s|m|h)+)', line)
if not m or len(since_old) < 12 or len(since_new) < 12:
    print('SKIP 会话 since 或旧 RR 的结束行没拿到'); sys.exit(0)

UNIT = {'ns': 1e-6, 'us': 1e-3, 'ms': 1.0, 's': 1e3, 'm': 6e4, 'h': 3.6e6}
held = sum(float(n) * UNIT[u] for n, u in re.findall(r'([0-9.]+)(ns|us|ms|s|m|h)', m.group(1)))


def ms(t):   # Go 的 RFC3339 可能带 9 位小数，3.10 的 fromisoformat 只吃 6 位
    head, sep, rest = t.partition('.')
    if sep:
        digits = re.match(r'[0-9]+', rest).group(0)
        zone = rest[len(digits):].replace('Z', '+00:00') or '+00:00'
        t = head + '.' + (digits + '000000')[:6] + zone
    else:
        t = t.replace('Z', '+00:00')
    return datetime.datetime.fromisoformat(t).timestamp() * 1000


print('OK %d %d %d' % (ms(since_old) + held - ms(since_new), ms(since_new), ms(since_old) + held))
PY

dx "$N1" "cd $WORK && nohup python3 sampler.py $WORK/samples.csv $WORK/stop $PFX1 $PFX2 \
'http://127.0.0.1:$N1_ST/status,http://$N2_IP:$N2_ST/status,http://$RR_IP:$RR_A_ST/routes,http://$RR_IP:$RR_B_ST/routes,http://$RR_IP:$RR_C_ST/routes' \
> $WORK/sampler.log 2>&1 & echo \$! > $WORK/sampler.pid" >/dev/null || die "采样器起不来"
sleep 0.6
[ "$(dx "$N1" "wc -l < $WORK/samples.csv" | tr -d ' ')" -ge 2 ] 2>/dev/null ||
    die "采样器只写了表头就停了：$(dx "$N1" "head -5 $WORK/sampler.log" | tr '\n' ' ')"
[ "$(dx "$N1" "test -s $WORK/sampler.log && echo Y || echo N")" = "N" ] ||
    die "采样器报了错：$(dx "$N1" "head -5 $WORK/sampler.log" | tr '\n' ' ')"
# 两个读表口都必须在通：读不到就把那台记成"表是空的"，会把读不到误判成缺口
[ "$(dx "$N1" "tail -1 $WORK/samples.csv | cut -d, -f2,3" | tr -d ' ')" = "1,1" ] ||
    die "采样器读不到某台设备的状态口（n1_ok/n2_ok 不是 1,1）：先修可达性，否则量到的缺口是真的还是读不到分不开"
note "采样器已开跑（每 20ms：两台设备的表 + 三台 RR 对两台设备的可见性）"

# ---------------------------------------------------------------- 工具

T0=$(dx "$N1" "python3 -c 'import time;print(int(time.time()*1000))'")
mark() { T0=$1; }          # 窗口起点：由调用方传当前毫秒
now_ms() { dx "$N1" "python3 -c 'import time;print(int(time.time()*1000))'"; }

# window <label> <from_ms> <to_ms> —— 把窗口里的数字打印出来并逐项判
window() {
    local label=$1 from=$2 to=$3 out
    out=$(dx "$N1" "python3 $WORK/window.py $WORK/samples.csv $from $to" 2>&1)
    note "$label 采样窗口：$(printf '%s' "$out" | tr '\n' ' ')"
    WIN_OUT=$out
    # 采样器中途死了 / 有一台的表读不到，这段窗口里每条判据都不可信，直接停
    [ "$(win_val samples)" != "0" ] || die "$label：这段窗口一条样本都没采到（采样器死了？看 $WORK/sampler.log）"
    [ "$(win_val n1_ok)" = "1" ] && [ "$(win_val n2_ok)" = "1" ] ||
        die "$label：窗口里有一台的表口读通过又读不通，量到的缺口分不清是没路由还是没读到"
}
# 一行里可能有几个键，先按空格拆开再取
win_val() { printf '%s\n' "$WIN_OUT" | tr ' ' '\n' | sed -n "s/^$1=//p" | head -1; }

need_n1_clean() {   # <label> —— 被换的那台必须一个缺口都没有
    local label=$1
    [ "$(win_val n1_has)" = "1" ] &&
        ok "$label：device 1 的表全程有对端前缀（缺口 0 条样本）" ||
        bad "$label：device 1 缺了 $(win_val n1_missing) 条样本、最长缺口 $(win_val n1_gap)ms"
    [ "$(win_val any_d1)" = "1" ] &&
        ok "$label：device 1 的前缀全程至少在一台 RR 上（全网没看不见它）" ||
        bad "$label：device 1 的前缀有 $(win_val any_d1_gap)ms 三台 RR 上都没有"
    [ "$(win_val n1_wd)" = "$w1_0" ] &&
        ok "$label：device 1 的 withdrawn 没动（$w1_0）：这张表一次都没被撤" ||
        bad "$label：device 1 的 withdrawn 变成 $(win_val n1_wd)：换 RR 期间它撤过路由"
}

# ---------------------------------------------------------------- 2 先加新

info "(2) 第一步：全租户 PUT /rrs [A, B]（先加新，谁都不撤）"
FROM=$(now_ms)
since_a1=$(since_of "$RR_A_ST" 1); since_a2=$(since_of "$RR_A_ST" 2)
body_ab="{\"rrs\":[{\"id\":\"rr-a\",\"addr\":\"$RR_A_ADDR\"},{\"id\":\"rr-b\",\"addr\":\"$RR_B_ADDR\"}]}"
code=$(rrs_put "$N1" "$(n1_url rrs)" "$body_ab"); [ "$code" = 202 ] && ok "node1 响应 202" || bad "node1 响应 $code $(resp_body "$N1")"
code=$(rrs_put "$N2" "$(n2_url rrs)" "$body_ab"); [ "$code" = 202 ] && ok "node2 响应 202" || bad "node2 响应 $code $(resp_body "$N2")"
wait_json "$N1" "$(n1_url status)" \
    "len(d['sessions']) == 2 and all(not s.get('retiring') for s in d['sessions']) and d.get('rr_wait') is None and
     all(s['connected'] and s['synced'] for s in d['sessions']) and d['rib_ready']" \
    "node1 两条会话都是 wanted、都 up+synced、无更换在途，两条都往同一张 Loc-RIB 里送" 20
wait_json "$N2" "$(n2_url status)" \
    "len(d['sessions']) == 2 and d['rib_ready'] and d.get('rr_wait') is None" \
    "node2 同样挂上两台（等价的两条活动 RR，不分主备）" 20
sleep 1
window "(2)" "$FROM" "$(now_ms)"
need_n1_clean "(2)"
[ "$(win_val any_d2)" = "1" ] && ok "(2)：device 2 的前缀也全程在 RR 上" || bad "(2)：device 2 的前缀有 $(win_val any_d2_gap)ms 谁都没有"
[ "$(win_val n2_has)" = "1" ] && ok "(2)：node2 的表全程有 $PFX1" || bad "(2)：node2 缺了 $(win_val n2_missing) 条样本（$(win_val n2_gap)ms）"
since_a1b=$(since_of "$RR_A_ST" 1); since_a2b=$(since_of "$RR_A_ST" 2)
[ "$since_a1" = "$since_a1b" ] && [ "$since_a1" != "-" ] &&
    ok "RR-A 上 device 1 的会话 since 没变（$since_a1）：加 standby 没拆旧连接" ||
    bad "RR-A 上 device 1 的会话被重建：$since_a1 → $since_a1b"
[ "$since_a2" = "$since_a2b" ] && [ "$since_a2" != "-" ] &&
    ok "RR-A 上 device 2 的会话 since 也没变（$since_a2）" ||
    bad "RR-A 上 device 2 的会话被重建：$since_a2 → $since_a2b"
[ "$(win_val both_ab_d1)" -ge 5 ] &&
    ok "(2) 采到 $(win_val both_ab_d1) 条 A、B 同时持有 device 1：重叠窗口是真的，不是事后编的" ||
    bad "(2) 只采到 $(win_val both_ab_d1) 条 A、B 同时持有 device 1"

# ---------------------------------------------------------------- 3 该看的看一眼

info "(3) 撤旧之前该看的那一眼：新 RR 收齐全租户路由了吗"
wait_json "$RR" "$(rr_url "$RR_B_ST" routes)" \
    "'$PFX1' in [r['prefix'] for r in d.get('1', [])] and '$PFX2' in [r['prefix'] for r in d.get('2', [])]" \
    "RR-B 已经同时有 device 1 的 $PFX1 与 device 2 的 $PFX2：这时候撤 A 才是无损的" 20

# ---------------------------------------------------------------- 4 再撤旧

info "(4) 第二步：撤旧。node1 先 PUT [B]，隔 ${FANOUT_SLEEP}s 再 node2 PUT [B]"
FROM=$(now_ms)
since_b1=$(since_of "$RR_B_ST" 1)
code=$(rrs_put "$N1" "$(n1_url rrs)" "{\"rrs\":[{\"id\":\"rr-b\",\"addr\":\"$RR_B_ADDR\"}]}")
[ "$code" = 202 ] && ok "node1 响应 202" || bad "node1 响应 $code $(resp_body "$N1")"
wait_json "$N1" "$(n1_url status)" \
    "d['rib_ready'] and len(d['sessions']) == 1 and d['sessions'][0]['addr'] == '$RR_B_ADDR' and
     d.get('rr_wait') is None" \
    "node1 只剩 B 一条会话且它已经同步：A 已经摘掉" 20
wait_json "$RR" "$(rr_url "$RR_A_ST" status)" "not [s for s in d['sessions'] if s['device_id'] == 1]" \
    "RR-A 上 device 1 的会话确实关了（不是留着记账）" 20
sleep "$FANOUT_SLEEP"
MID=$(now_ms)
code=$(rrs_put "$N2" "$(n2_url rrs)" "{\"rrs\":[{\"id\":\"rr-b\",\"addr\":\"$RR_B_ADDR\"}]}")
[ "$code" = 202 ] && ok "node2 响应 202（这一步隔了 ${FANOUT_SLEEP}s 才发：模拟发布扇出）" || bad "node2 响应 $code $(resp_body "$N2")"
wait_json "$N2" "$(n2_url status)" \
    "d['rib_ready'] and len(d['sessions']) == 1 and d['sessions'][0]['addr'] == '$RR_B_ADDR' and
     '$PFX1' in [r['prefix'] for v in d['rib'].values() for r in v]" \
    "node2 也只剩 B，且它表里 $PFX1 又回来了" 20
sleep 0.5
window "(4)" "$FROM" "$(now_ms)"
need_n1_clean "(4) 被换的那台（E1 的正面判据）"
since_b1b=$(since_of "$RR_B_ST" 1)
[ "$since_b1" = "$since_b1b" ] && [ "$since_b1" != "-" ] &&
    ok "RR-B 上 device 1 的会话 since 没变（$since_b1）：接管用的是 (2) 里那条既有会话" ||
    bad "RR-B 上 device 1 的会话被重建：$since_b1 → $since_b1b"
log_a=$(dx "$RR" "grep -m1 'node/1' $WORK/rr-a.log" 2>/dev/null)
log_a=${log_a//\"/}; log_a=${log_a//\'/}
[ -n "$log_a" ] && note "RR-A 的关闭记录：$log_a" || bad "RR-A 日志里没有 device 1 的会话结束行"

# O7 的靶心：node2 是"还没换的那台"，这一步它同样一条样本都不许缺。
# A 撤走自己那份 device 1 的拷贝之后，B 的同一份必须顶上；under 旧模型这里会缺满整个
# 扇出间隔（缺口长度恰好等于两次 PUT 之间），现在缺口必须为 0。
n2_gap=$(win_val n2_gap)
[ "${n2_gap:-x}" = "0" ] &&
    ok "(4)：node2 全程没缺（并集覆盖：A 撤了它那份，B 的同一份顶上）" ||
    bad "(4)：node2 缺了 $(win_val n2_missing) 条样本、最长 $(win_val n2_gap)ms —— 并集没兜住，第二条 RR 等于白挂"
[ "$(win_val n2_wd)" = "$w2_0" ] &&
    ok "(4)：node2 的 withdrawn 没动（$w2_0）：少了一份拷贝，前缀没离开表" ||
    bad "(4)：node2 的 withdrawn 从 $w2_0 变成 $(win_val n2_wd)：A 的撤回不该让这个前缀离开表"

# ---------------------------------------------------------------- 5 收尾状态

info "(5) 撤完之后：两台都只挂 B，RR-A 干净"
wait_json "$RR" "$(rr_url "$RR_A_ST" routes)" "not d" "RR-A 上一台设备都没有了：它不再服务这个租户" 20
wait_json "$RR" "$(rr_url "$RR_B_ST" routes)" "d.get('1') and d.get('2')" "RR-B 上一台设备都不少" 10

# ---------------------------------------------------------------- 6 冷 RR

info "(6) 反例：node1 单独换成一台谁都没挂过的 RR-C，而且 C 还没启动"
code=$(rrs_put "$N1" "$(n1_url rrs)" "{\"rrs\":[{\"id\":\"rr-c\",\"addr\":\"$RR_C_ADDR\"}]}")
[ "$code" = 202 ] && ok "node1 响应 202（立刻回话，不等换完）" || bad "node1 响应 $code $(resp_body "$N1")"
wait_json "$N1" "$(n1_url status)" \
    "d['rr_wait'] and d['rr_wait']['pending_addrs'] == ['$RR_C_ADDR'] and d['rr_wait']['retiring_addrs'] == ['$RR_B_ADDR'] and
     d['rib_ready'] and d['sessions'][1]['retiring'] and d['sessions'][1]['synced'] and
     '$PFX2' in [r['prefix'] for v in d['rib'].values() for r in v]" \
    "新 RR 上不去这一路：rr_wait 挂着 pending=[C]/retiring=[B]，rib_ready 正是由那条 retiring 的会话供着，表还是满的" 15
note "这段状态是确定的（不靠抢采样），停 3 秒"
sleep 3
wait_json "$N1" "$(n1_url status)" "d['rib_ready'] and d['rr_wait'] and d['sessions'][1]['retiring']" \
    "3 秒后仍是同一份状态：B 没被摘、表没空过" 10

FROM6=$(now_ms)
start_rr rr-c "$RR_C_PORT" "$RR_C_ST"
wait_json "$N1" "$(n1_url status)" \
    "d['rib_ready'] and len(d['sessions']) == 1 and d['sessions'][0]['addr'] == '$RR_C_ADDR' and
     d.get('rr_wait') is None" \
    "C 一监听，node1 靠自己的重连退避就接上了（全程没有第二次 PUT）；B 随即被摘" 30

# 会话没断，但 C 上从来没有 device 2 —— 这就是问题二。量它缺多久，再让 node2 也挂过来。
i=0
while has_pfx "$N1" "$(n1_url status)" "$PFX2" && [ $i -lt 10 ]; do sleep 0.2; i=$((i + 1)); done
if has_pfx "$N1" "$(n1_url status)" "$PFX2"; then
    bad "冷 RR 反例没做出来：device 2 还没挂 C，node1 的表里却已经有 $PFX2 了"
else
    ok "冷 RR 反例成立：node1 接上了 C，但表里 $PFX2 没了 —— C 上从来就没有 device 2 的宣告"
fi
wait_json "$RR" "$(rr_url "$RR_C_ST" routes)" "d.get('1') and not d.get('2')" \
    "RR-C 的 /routes 里只有 device 1：缺的不是会话，是这台 RR 的内容" 15
code=$(rrs_put "$N2" "$(n2_url rrs)" "{\"rrs\":[{\"id\":\"rr-c\",\"addr\":\"$RR_C_ADDR\"}]}")
[ "$code" = 202 ] && ok "现在把 node2 也 PUT [C]（对端进来了）" || bad "node2 响应 $code $(resp_body "$N2")"
wait_json "$N1" "$(n1_url status)" \
    "'$PFX2' in [r['prefix'] for v in d['rib'].values() for r in v] and len(d['sessions']) == 1 and
     d['sessions'][0]['addr'] == '$RR_C_ADDR'" \
    "node1 一次都没再动，$PFX2 自己回来了：RR 收到宣告就反射，缺内容 ≠ 缺机制" 20
window "(6)" "$FROM6" "$(now_ms)"
note "(6) 里 node1 缺样本 $(win_val n1_missing) 条、最长缺口 $(win_val n1_gap)ms（全是冷 RR 的内容空窗）"

# ---------------------------------------------------------------- 7 负向

info "(7) 负向：非法集合必须 400，且被拒之后集合一点没动"
rrs_of() { get_url "$N1" "$(n1_url rrs)" | python3 -c "import json,sys; print(json.dumps(json.load(sys.stdin)['rrs'], sort_keys=True))" 2>/dev/null; }
before=$(rrs_of)
for body in '{"rrs":[]}' '{}' '{"rrs":[{"id":"x","addr":"a"},{"id":"y","addr":"a"}]}' '{"rrs":[{"id":"x"}]}' '{"rrs":[{"addr":"a:1"},"nope"]}'; do
    code=$(rrs_put "$N1" "$(n1_url rrs)" "$body")
    [ "$code" = 400 ] && ok "拒绝 $body → 400 $(resp_body "$N1")" || bad "$body 应当 400，实际 $code"
done
after=$(rrs_of)
[ -n "$before" ] && [ "$before" = "$after" ] && ok "五次被拒之后集合还是 $after" || bad "非法 PUT 改了集合：$before → $after"
wait_json "$N1" "$(n1_url status)" \
    "d['rib_ready'] and '$PFX2' in [r['prefix'] for v in d['rib'].values() for r in v]" \
    "连着五次被拒之后，node1 仍在 C 上正常服务" 10
code=$(dx "$N1" "curl -s -o /dev/null -w '%{http_code}' -X DELETE $(n1_url rrs)")
[ "$code" = 405 ] && ok "DELETE /rrs 是 405：这个入口只有读和整体替换" || bad "DELETE /rrs 应当 405，实际 $code"

dx "$N1" "touch $WORK/stop" >/dev/null
i=0
while [ $i -lt 25 ] && dx "$N1" "kill -0 \$(cat $WORK/sampler.pid) 2>/dev/null" >/dev/null 2>&1; do sleep 0.2; i=$((i + 1)); done

# ---------------------------------------------------------------- 8 全程硬账

info "(8) 三笔不依赖采样时刻的硬账"
if [ -n "$log_a" ]; then
    OVERLAP=$(dx "$RR" "python3 $WORK/overlap.py '$since_a1b' '$since_b1' '$log_a'" 2>&1)
    read -r _tag ov_ms b_ms a_close <<< "$OVERLAP"
    case "$_tag" in
      OK) [ "$ov_ms" -gt 0 ] &&
            ok "重叠时长 = ${ov_ms}ms：RR-B 从 ${b_ms} 起服务 device 1，RR-A 到 ${a_close} 才关 —— 先立后破" ||
            bad "重叠时长 = ${ov_ms}ms：RR-A 关得比 RR-B 起得还早，这是 break-before-make" ;;
      *)  bad "重叠时长算不出来：$OVERLAP（since_a=$since_a1b since_b=$since_b1）" ;;
    esac
fi
w1_end=$(field "$N1" "$(n1_url status)" "d['withdrawn']")
w2_end=$(field "$N2" "$(n2_url status)" "d['withdrawn']")
[ "$w1_end" = "$w1_0" ] && ok "device 1 全程 withdrawn = $w1_0：被换的那台一次撤路由都没处理" ||
    bad "device 1 的 withdrawn 从 $w1_0 变成 $w1_end"
[ "$w2_end" != "$w2_0" ] && ok "device 2 的 withdrawn 从 $w2_0 涨到 $w2_end：涨在 (6) —— 那时 B 上 device 1 的拷贝**全没了**，是真撤路由；(4) 里只少一份拷贝，没记事件" ||
    note "device 2 的 withdrawn 没涨（$w2_0）：它换过去时 A 上已经没东西可撤了"
stamp_n1_end=$(stamp_of "$N1" "$WORK/node1.pid")
stamp_n2_end=$(stamp_of "$N2" "$WORK/node2.pid")
[ "$stamp_n1" = "$stamp_n1_end" ] && ok "device 1 的 sr-route 全程同一个进程（$stamp_n1_end）" ||
    bad "device 1 的 sr-route 换了：$stamp_n1 → $stamp_n1_end"
[ "$stamp_n2" = "$stamp_n2_end" ] && ok "device 2 的 sr-route 全程同一个进程（$stamp_n2_end）" ||
    bad "device 2 的 sr-route 换了：$stamp_n2 → $stamp_n2_end"
disk=$(dx "$N1" "python3 -c \"import json;print(sorted(r['addr'] for r in json.load(open('$WORK/node1.json'))['rrs']))\"")
live=$(field "$N1" "$(n1_url rrs)" "sorted(r['addr'] for r in d['rrs'])")
[ "$disk" = "['$RR_A_ADDR']" ] && ok "磁盘那份仍是起步时的 $disk：PUT /rrs 只改内存，配置以控制器发布为准" ||
    bad "磁盘那份被改了？现在是 $disk"
note "内存里现在是 $live：这个进程重启就回到磁盘那份"

echo
echo "== 结果：$PASS passed, $FAIL failed"
if [ "$FAIL" -ne 0 ]; then
    for pair in "$RR:rr-a rr-b rr-c" "$N1:node1 sampler" "$N2:node2"; do
        c=${pair%%:*}
        for f in ${pair#*:}; do
            echo "---- $c/$f.log" >&2
            dx "$c" "tail -25 $WORK/$f.log" >&2 2>/dev/null || true
        done
    done
fi
if [ "$KEEP" = 1 ]; then
    trap - EXIT
    echo "== --keep：三台 RR 与两台设备进程还在跑，配置/日志/采样留在各容器的 $WORK"
fi
[ "$FAIL" -eq 0 ]
