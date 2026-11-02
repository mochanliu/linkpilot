#!/bin/bash
# test-overlay-bundle.sh - O6-b 判据：配置包第 4 段 → sr-apply 叠加写盘 + 推活表 + 开机重放
#
# O6-b 的判据一句话：**控制器发布的 `tunnels`/`vrfs`（O6-c 起还有 `rrs`）落进盒子的
# route.json，正在跑的 sr-route 当场换表，全程不重启任何进程；控制器不可达时盘上那份
# 就是答案，盒子重启后不会再向任何人要一次。** 这条脚本把这句话拆成可以重跑的步骤：
#
#   (1) 基线：盘上是运维手写的那份 route.json（全部设备键 + 一张旧表），sr-route 读它起来；
#   (2) apply v2：第 4 段盖掉 tunnels/vrfs；活表同一次 apply 就变；sr-route 的 pid 与
#       启动时刻一动不动；设备键（tenant/device_id/identity_dir/serial/hold_sec/
#       tls_server_name/rrs/status_addr）逐字不动；
#   (3) 第 4 段多带一个键（将来的 `leak`）：明写"不认识"并忽略，盘上不写它，活表不动；
#   (4) 导通组删掉（tunnels/vrfs 都是空表）：活表清空，残留 RT 当场摘掉；
#   (5) sr-route 不在：apply 仍然 rc=0，明写"已落盘、未热更"，status.json 带着那句注记
#       —— 一次瞬时的连不上不许把整台盒子的下发卡进错误里；
#   (6) 开机重放：sr-apply boot 打那句"开机重放不热更"，随后 sr-route 起来 /status 就是
#       盘上那份 —— 全程没有一次 PUT 到达它（rrs 指向没人听的地址）；
#   (7) rrs 热更接缝（C-c）：换 RR 时 sr-route 答 202 + rr_wait，sr-apply 如实报
#       "已开始、没换完"，不判失败；
#   (8) 盘上那份没有 status_addr：能叠、不能推，明写"盘上那份没有 status_addr"；
#   (9) 盘上根本没有 route.json：整次 apply rc=1，而版本已经装进 versions/（叠加是最后一步）；
#  (10) 收尾：私有进程与 work 目录清掉；盒子上自己的 sr-route pid 与 /etc/sdwan/route.json
#       的哈希前后一致（这条脚本不许碰 lab 的正式配置）。
#
# 三条"为什么这么写"（排障时先看这里）：
#   - VPP/FRR 两段用空片段 + `--frr-reload true`：本脚本判的是第 4 段与 applier 的叠加热更，
#     那两段的判据在 test-boot.sh。applier 本来就带"盒子没有 FRR"的形态（FRRReload 字段）。
#   - 盒子上正在跑的 sr-route / sr-agent 一概不动：另起自己的 sr-route（自己的端口、
#     自己的配置文件），身份目录只读复用（sr-route 要求入网证书，且配置里的 tenant/
#     device_id 必须与证书一致，所以这两个值从 state.json 读出来生成）。rrs 指向没人听的
#     地址 ⇒ 运行中那张表唯一可能的来源就是 sr-apply 推的 PUT。
#   - 本端前缀不在这份文件里：sr-route 启动时一条都不宣告，运行中由 sr-frr 经
#     PUT /local-prefixes 推进来。本脚本不碰那一路，所以每次重启都是零宣告起来。
#
# 前置：
#   - lab 在跑（默认 BOX=lab-cpe1，容器里有 /etc/sdwan/identity 与 /workspace 挂载）；
#   - 宿主机有 docker CLI，且 DOCKER_HOST 指向 lab 所在的那台 daemon（2026-11-01 起是 colima：
#     export DOCKER_HOST=unix:///Users/louis/.colima/default/docker.sock）。
#
# 用法（宿主机执行）：
#   test/scripts/test-overlay-bundle.sh [--keep]
#   BOX=lab-cpe2 test/scripts/test-overlay-bundle.sh      # 换盒子，必须已入网
# --keep：跑完保留 $WORK（bin、配置、日志、每个版本的 bundle）与私有 sr-route。
set -u

BOX=${BOX:-lab-cpe1}
PORT=${PORT:-18721}                      # 私有 sr-route 的状态口（盒子自己的在别的口）
WORK=${WORK:-/tmp/sr-overlay-bundle}     # 容器内的工作目录
IDENT=${IDENT:-/etc/sdwan/identity}      # 入网身份（只读复用）
REPO_IN_BOX=${REPO_IN_BOX:-/workspace/controller}

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
die()  { echo "FATAL: $*" >&2; exit 2; }

dx()  { docker exec "$BOX" bash -lc "$1"; }
dxi() { docker exec -i "$BOX" bash -lc "$1"; }

BIN=$WORK/bin
ROUTE_CONF=$WORK/route.json
SEED=$WORK/seed.json
ROOT=$WORK/root
FRR=$WORK/frr.conf
SR_LOG=$WORK/sr-route.log
VERIFY=$WORK/verify.py

# ---------------------------------------------------------------- 私有 sr-route

our_pid()   { dx "pgrep -f 'sr-route -config $ROUTE_CONF' | head -1" 2>/dev/null; }
pid_start() { dx "ps -o lstart= -p $1 2>/dev/null" | sed 's/^ *//;s/ *$//'; }
route_up()  { dx "curl -sf --max-time 2 http://127.0.0.1:$PORT/status >/dev/null 2>&1"; }

wait_route() {   # <label> [timeout_s]
    local label=$1 limit=${2:-15} i=0
    while [ "$i" -lt $((limit * 5)) ]; do
        if route_up; then ok "$label"; return 0; fi
        sleep 0.2
        i=$((i + 1))
    done
    bad "$label（/status 起不来）"
    dx "cat $SR_LOG 2>/dev/null" | sed 's/^/    /' >&2
    return 1
}

start_route() {
    docker exec -d "$BOX" bash -lc "exec $BIN/sr-route -config $ROUTE_CONF >> $SR_LOG 2>&1"
}

stop_route() {
    local p i=0
    p=$(our_pid)
    [ -n "$p" ] || return 0
    dx "kill $p" >/dev/null 2>&1
    while [ "$i" -lt 50 ]; do
        dx "kill -0 $p" >/dev/null 2>&1 || return 0
        sleep 0.1
        i=$((i + 1))
    done
    dx "kill -9 $p" >/dev/null 2>&1 || true
}

live_to() { dx "curl -sf --max-time 2 http://127.0.0.1:$PORT/status > $1" || return 1; }

# ---------------------------------------------------------------- 断言工具

APPLY_N=0
LASTLOG=""

# apply_bundle <bundle> —— 跟盒子上一样的 apply；rc 就是 sr-apply 的 rc
apply_bundle() {
    APPLY_N=$((APPLY_N + 1))
    LASTLOG=$WORK/apply-$APPLY_N.log
    dx "$BIN/sr-apply apply --bundle $1 --root $ROOT --frr-conf $FRR --frr-reload true --route-conf $ROUTE_CONF >> $LASTLOG 2>&1; echo rc=\$? >> $LASTLOG" >/dev/null
    dx "cat $LASTLOG"
    dx "grep -q '^rc=0$' $LASTLOG" >/dev/null 2>&1
}

# apply_raw <bundle> <root> <route-conf> —— 换 root / 换 route.json 的负向用
apply_raw() {
    APPLY_N=$((APPLY_N + 1))
    LASTLOG=$WORK/apply-$APPLY_N.log
    dx "$BIN/sr-apply apply --bundle $1 --root $2 --frr-conf $FRR --frr-reload true --route-conf $3 >> $LASTLOG 2>&1; echo rc=\$? >> $LASTLOG" >/dev/null
    dx "cat $LASTLOG"
    dx "grep -q '^rc=0$' $LASTLOG" >/dev/null 2>&1
}

log_has() {
    if dx "grep -qF '$2' $1" >/dev/null 2>&1; then ok "$3"; else bad "$3（日志里没有：$2）"; dx "cat $1" | sed 's/^/    /' >&2; fi
}
log_lacks() {
    if dx "grep -qF '$2' $1" >/dev/null 2>&1; then bad "$3（日志里出现了不该有的：$2）"; else ok "$3"; fi
}

# run_v <label> <verify.py 子命令与参数...> —— 通过就 ok，不然把解释打出来
run_v() {
    local label=$1 out
    shift
    out=$(dx "python3 $VERIFY $*" 2>&1)
    if [ $? -eq 0 ]; then ok "$label"; else bad "$label"; printf '%s\n' "$out" | sed 's/^/    /' >&2; fi
}

KEEP_PID=""
KEEP_START=""
remember_route() {
    KEEP_PID=$(our_pid)
    KEEP_START=$(pid_start "$KEEP_PID")
}
same_route() {
    local p s
    p=$(our_pid); s=$(pid_start "$p")
    if [ -n "$KEEP_PID" ] && [ "$p" = "$KEEP_PID" ] && [ "$s" = "$KEEP_START" ]; then
        ok "$1（pid $p，启动时刻未变）"
    else
        bad "$1（基线 pid=$KEEP_PID @ $KEEP_START，现在 pid=$p @ $s）"
    fi
}

CLEANED=0
cleanup() {
    [ "$CLEANED" = "1" ] && return 0
    CLEANED=1
    stop_route
    if [ "$KEEP" = "1" ]; then
        info "--keep：保留 $WORK（$BOX:$WORK），私有日志 $SR_LOG"
    else
        dx "rm -rf $WORK" >/dev/null 2>&1
    fi
}
trap cleanup EXIT

# ---------------------------------------------------------------- 准备

[ "$(docker ps -q -f name=^${BOX}$)" ] || die "$BOX 没在跑（lab 起没起？DOCKER_HOST 指对了吗）"
dx "test -s $IDENT/state.json && test -s $IDENT/node_cert.pem" || die "$BOX:$IDENT 里没有入网身份"
dx "test -d $REPO_IN_BOX" || die "$BOX:$REPO_IN_BOX 不在（容器没挂仓库？）"

# 基线：盒子自己的 sr-route 与正式 route.json，收尾要对上。
# 锚在行首：不锚会把 start.sh 那层 `sh -c /usr/local/bin/sr-route …` 的包装壳也算进来。
BOX_PGREP='^/usr/local/bin/sr-route '
BOX_PID=$(dx "pgrep -f '$BOX_PGREP' | head -1")
BOX_ROUTE_SHA=$(dx "sha256sum /etc/sdwan/route.json 2>/dev/null | cut -d' ' -f1")

info "0. 编译与准备"
stop_route
dx "rm -rf $WORK && mkdir -p $BIN $ROOT" || die "建不了 $WORK"
if dx "cd $REPO_IN_BOX && go build -o $BIN/sr-route ./cmd/sr-route && go build -o $BIN/sr-apply ./cmd/sr-apply" >/dev/null 2>&1; then
    ok "sr-route / sr-apply 已编译（容器里的 Go）"
else
    bad "编译失败"
    dx "cd $REPO_IN_BOX && go build -o $BIN/sr-route ./cmd/sr-route && go build -o $BIN/sr-apply ./cmd/sr-apply" 2>&1 | sed 's/^/    /' >&2
    exit 2
fi

# ---------------------------------------------------------------- verify.py

dxi "cat > $VERIFY" <<'PY'
import json, sys

KEYS = ["tenant", "device_id", "identity_dir", "serial", "hold_sec", "tls_server_name",
        "rrs", "tunnels", "vrfs", "status_addr"]

A = {"tunnels": [{"peer_device_id": 2, "next_hop": "10.108.0.2"}],
     "vrfs": [{"id": 0, "export_rts": ["65000:1000"], "import_rts": ["65000:1000"]}]}
B = {"tunnels": [{"peer_device_id": 3, "next_hop": "10.108.0.3"}],
     "vrfs": [{"id": 0, "export_rts": ["65000:1002"], "import_rts": ["65000:1002"]}]}
D = {"tunnels": [{"peer_device_id": 5, "next_hop": "10.108.0.5"}],
     "vrfs": [{"id": 0, "export_rts": ["65000:1004"], "import_rts": ["65000:1004"]}]}
E = {"tunnels": [{"peer_device_id": 6, "next_hop": "10.108.0.6"}],
     "vrfs": [{"id": 0, "export_rts": ["65000:1005"], "import_rts": ["65000:1005"]}]}
EMPTY = {"tunnels": [], "vrfs": []}
TABLES = {"A": A, "B": B, "D": D, "E": E, "EMPTY": EMPTY}


def gen(_):
    # argv: gen <work> <state.json> <identity_dir> <port>
    work, state_path, identity, port = sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5]
    st = json.load(open(state_path))
    seed = {
        "tenant": st["tenant_id"], "device_id": st["node_id"],
        "identity_dir": identity, "serial": st["serial"],
        "hold_sec": 12, "tls_server_name": "10.99.0.100",
        "rrs": [{"id": "rr-none", "addr": "10.99.0.250:18446"}],
        # 运维手写的那张旧表：peer 99 / RT 65000:999，等第 4 段来盖
        "tunnels": [{"peer_device_id": 99, "next_hop": "10.99.99.99"}],
        "vrfs": [{"id": 0, "export_rts": ["65000:999"], "import_rts": ["65000:999"]}],
        "status_addr": "127.0.0.1:" + port,
    }
    assert set(seed) == set(KEYS), "seed 的键集合变了"
    open(work + "/seed.json", "w").write(json.dumps(seed, indent=2) + "\n")
    open(work + "/expect-SEED.json", "w").write(
        json.dumps({"tunnels": seed["tunnels"], "vrfs": seed["vrfs"]}, indent=2) + "\n")

    def bundle(ver, route):
        b = {"node_id": st["node_id"], "version": ver, "checks": [],
             "artifacts": {"frr": "", "vpp": "", "boot": "#!/bin/sh\nset -e\n",
                           "route": json.dumps(route)}}
        open(work + "/b%d.json" % ver, "w").write(json.dumps(b, indent=2) + "\n")

    bundle(2, A)
    leak = dict(A)
    leak["leak"] = {"in": "static", "out": "bgp"}
    bundle(3, leak)
    bundle(4, EMPTY)
    bundle(5, B)
    # 换 RR 的那一版：表是 D，rrs 指向第二台没人听的地址（旧的是 rr-none）
    d_rrs = dict(D)
    d_rrs["rrs"] = [{"id": "rr-none2", "addr": "10.99.0.251:18446"}]
    bundle(6, d_rrs)
    bundle(7, D)
    bundle(8, E)

    for name, t in TABLES.items():
        open(work + "/expect-%s.json" % name, "w").write(json.dumps(t, indent=2) + "\n")
    # 第 (8) 步：同一份设备键，但没有 status_addr，表也是旧的 —— 能叠、不能推。
    # 留一份 base 给断言用（apply 会就地改写 route-nostatus.json）。
    nost = dict(seed)
    nost.pop("status_addr")
    open(work + "/route-nostatus.base.json", "w").write(json.dumps(nost, indent=2) + "\n")
    print("generated seed + 8 bundles in " + work)


def merged(_):
    """merged <merged.json> <seed.json> <owned,...> —— 设备键逐字相等、拥有的键被换掉"""
    m = json.load(open(sys.argv[2]))
    s = json.load(open(sys.argv[3]))
    owned = set(sys.argv[4].split(","))
    errs = []
    if set(m) != set(s):
        errs.append("键集合变了：多了 %s，少了 %s" % (sorted(set(m) - set(s)), sorted(set(s) - set(m))))
    for k in s:
        if k in owned:
            continue
        if m.get(k) != s[k]:
            errs.append("设备键 %s 被改了：%r -> %r" % (k, s[k], m.get(k)))
    for k in owned:
        if m.get(k) == s.get(k):
            errs.append("控制器键 %s 没被第 4 段盖掉（还是旧值 %r）" % (k, s.get(k)))
    if errs:
        print("\n".join(errs))
        sys.exit(1)
    print("ok：%d 个设备键逐字未动，%s 已覆盖" % (len(set(s) - owned), ",".join(sorted(owned))))


def live(_):
    """live <status.json> <expect-*.json> —— 活表的 tunnels/vrfs 与期望逐项相等"""
    d = json.load(open(sys.argv[2]))
    e = json.load(open(sys.argv[3]))
    got = {"tunnels": d.get("tunnels", []), "vrfs": d.get("vrfs", [])}
    if got != e:
        print("活表不符：\n got=%s\n want=%s" % (json.dumps(got, sort_keys=True), json.dumps(e, sort_keys=True)))
        sys.exit(1)
    print("ok：活表 = %s" % json.dumps(got, sort_keys=True))


def nostatus(_):
    """nostatus <merged.json> <base.json> —— 表换成了 E，其它设备键逐字未动，status_addr 没被造出来"""
    m = json.load(open(sys.argv[2]))
    s = json.load(open(sys.argv[3]))
    errs = []
    if m.get("tunnels") != E["tunnels"] or m.get("vrfs") != E["vrfs"]:
        errs.append("表没落盘：tunnels=%r vrfs=%r" % (m.get("tunnels"), m.get("vrfs")))
    if "status_addr" in m or "status_addr" in s:
        errs.append("status_addr 被凭空造出来了")
    if set(m) != set(s):
        errs.append("键集合变了：多了 %s，少了 %s" % (sorted(set(m) - set(s)), sorted(set(s) - set(m))))
    for k in set(s) - {"tunnels", "vrfs"}:
        if m.get(k) != s[k]:
            errs.append("设备键 %s 被改了：%r -> %r" % (k, s[k], m.get(k)))
    if errs:
        print("\n".join(errs))
        sys.exit(1)
    print("ok：表已落盘，%d 个设备键未动，status_addr 仍是缺的" % (len(set(s) - {"tunnels", "vrfs"})))


def stat(_):
    """stat <status.json> <version> <apply|boot> <ok|err> [备注子串]"""
    d = json.load(open(sys.argv[2]))
    ver, src, want = int(sys.argv[3]), sys.argv[4], sys.argv[5]
    errs = []
    if d.get("version") != ver:
        errs.append("version=%r，期望 %d" % (d.get("version"), ver))
    if d.get("source") != src:
        errs.append("source=%r，期望 %s" % (d.get("source"), src))
    if want == "ok" and not d.get("ok"):
        errs.append("ok=false：%s" % d.get("error"))
    if want == "err" and d.get("ok"):
        errs.append("ok=true，期望失败")
    if len(sys.argv) > 6:
        notes = " | ".join(d.get("notes") or [])
        if sys.argv[6] not in notes:
            errs.append("备注里没有 %r（实际 %r）" % (sys.argv[6], notes))
    if errs:
        print("\n".join(errs))
        sys.exit(1)
    print("ok：v%d %s %s" % (ver, src, want))


def sessions(_):
    """sessions <status.json> <addr,...> —— 会话集合恰好是这些地址"""
    d = json.load(open(sys.argv[2]))
    want = sorted(sys.argv[3].split(","))
    got = sorted(s["addr"] for s in d.get("sessions") or [])
    if got != want:
        print("会话集合 %s，期望 %s" % (got, want))
        sys.exit(1)
    print("ok：会话 %s" % got)


globals()[sys.argv[1]]("x")
PY

# ---------------------------------------------------------------- 1. 基线

info "1. 基线：手写的 route.json + 私有 sr-route（rrs 指向没人听的地址）"
if dx "ss -ltn 2>/dev/null | grep -q ':$PORT '" >/dev/null 2>&1; then
    die "$BOX 上 $PORT 已被占用"
fi
dx "python3 $VERIFY gen $WORK $IDENT/state.json $IDENT $PORT" || die "生成种子与 bundle 失败"
dx "cp $SEED $ROUTE_CONF" || die "写不了 $ROUTE_CONF"
start_route
wait_route "私有 sr-route 起来了" || exit 2
live_to $WORK/live.json || die "读不到 /status"
run_v "基线活表 = 盘上那张旧表（peer 99 / 65000:999）" live $WORK/live.json $WORK/expect-SEED.json
remember_route
ok "基线私有进程 pid=$KEEP_PID"

# ---------------------------------------------------------------- 2. v2

info "2. apply v2：第 4 段盖掉 tunnels/vrfs，活表同一次 apply 就变"
if apply_bundle $WORK/b2.json; then ok "apply v2 rc=0"; else bad "apply v2 失败"; fi
log_has "$LASTLOG" "installed version 2" "版本 2 已装进 versions/"
log_has "$LASTLOG" "已热更 http://127.0.0.1:$PORT/tunnels" "推了 PUT /tunnels"
log_has "$LASTLOG" "已热更 http://127.0.0.1:$PORT/vrfs" "推了 PUT /vrfs"
same_route "sr-route 没有重启（pid + 启动时刻）"
live_to $WORK/live.json
run_v "活表 = 第 4 段那张表" live $WORK/live.json $WORK/expect-A.json
run_v "盘上合并：设备键逐字未动，tunnels/vrfs 被盖掉" merged $ROUTE_CONF $SEED tunnels,vrfs
run_v "status.json：v2 / apply / ok" stat $ROOT/status.json 2 apply ok

# ---------------------------------------------------------------- 3. 未知键

info "3. 第 4 段多带一个键（将来的 leak）：说出来、忽略、不写盘"
if apply_bundle $WORK/b3.json; then ok "apply v3 rc=0"; else bad "apply v3 失败"; fi
log_has "$LASTLOG" "不认识" "日志明写这个键不认识"
log_has "$LASTLOG" "没被改写" "日志说盘上那份没被它改写"
run_v "盘上没有多出 leak 键" merged $ROUTE_CONF $SEED tunnels,vrfs
live_to $WORK/live.json
run_v "活表没被未知键影响" live $WORK/live.json $WORK/expect-A.json
same_route "sr-route 仍然没有重启"

# ---------------------------------------------------------------- 4. 空表

info "4. 导通组删掉（空表）：活表清空，残留 RT 当场摘掉"
if apply_bundle $WORK/b4.json; then ok "apply v4 rc=0"; else bad "apply v4 失败"; fi
live_to $WORK/live.json
run_v "活表 = tunnels [] / vrfs []（65000:1000 已经不在）" live $WORK/live.json $WORK/expect-EMPTY.json
run_v "盘上同样是空表" merged $ROUTE_CONF $SEED tunnels,vrfs
same_route "sr-route 仍然没有重启"

# ---------------------------------------------------------------- 5. 连不上

info "5. sr-route 不在：已落盘、未热更，不是失败"
stop_route
if apply_bundle $WORK/b5.json; then ok "apply v5 rc=0（sr-route 不在也算成功）"; else bad "apply v5 被判失败"; fi
log_has "$LASTLOG" "未热更" "日志明写未热更（而不是完成）"
log_has "$LASTLOG" "连不上" "日志带上连不上的原因"
run_v "status.json：v5 / apply / ok，备注带'未热更'" stat $ROOT/status.json 5 apply ok "未热更"
run_v "盘上是 v5 那张表（B）" merged $ROUTE_CONF $SEED tunnels,vrfs
dx "python3 -c \"import json;d=json.load(open('$ROUTE_CONF'));print(json.dumps({'tunnels':d['tunnels'],'vrfs':d['vrfs']}))\" > $WORK/disk.json"
run_v "盘上那份 = 期望表 B" live $WORK/disk.json $WORK/expect-B.json

# ---------------------------------------------------------------- 6. 开机重放

info "6. 开机重放：只落盘不推，sr-route 起来就是盘上那份"
dx "$BIN/sr-apply boot --root $ROOT --frr-conf $FRR --frr-reload true --route-conf $ROUTE_CONF > $WORK/boot.log 2>&1; echo rc=\$? >> $WORK/boot.log" >/dev/null
dx "cat $WORK/boot.log"
log_has "$WORK/boot.log" "rc=0" "boot rc=0"
log_has "$WORK/boot.log" "开机重放不热更" "日志那句「开机重放不热更，sr-route 起来就是这张表」"
log_lacks "$WORK/boot.log" "已热更" "开机重放没有推任何表"
run_v "status.json：v5 / boot / ok" stat $ROOT/status.json 5 boot ok
start_route
wait_route "sr-route 重新起来（读盘上那份）"
live_to $WORK/live.json
run_v "活表就是盘上那份 B —— 唯一来源是磁盘" live $WORK/live.json $WORK/expect-B.json
remember_route
ok "重放后的私有进程 pid=$KEEP_PID（新进程，后面按它判不重启）"

# ---------------------------------------------------------------- 7. rrs

info "7. rrs 热更接缝（C-c）：换 RR 只答「已开始」，不判失败"
if apply_bundle $WORK/b6.json; then ok "apply v6 rc=0"; else bad "apply v6 失败"; fi
log_has "$LASTLOG" "换 RR 已开始" "日志如实说换 RR 已开始、还没换完"
run_v "status.json 备注带 rrs 的进度（不是完成）" stat $ROOT/status.json 6 apply ok "rrs 已开始热更"
run_v "盘上 rrs 已换成 rr-none2（设备键里的 rrs 也是控制器管的）" merged $ROUTE_CONF $SEED tunnels,vrfs,rrs
live_to $WORK/live.json
run_v "活表 = D 那张表（换 RR 不影响导通组）" live $WORK/live.json $WORK/expect-D.json
run_v "两台 RR 都在会话集合里（新的 pending，旧的先留着）" sessions $WORK/live.json 10.99.0.250:18446,10.99.0.251:18446
same_route "换 RR 期间 sr-route 没有重启"

# ---------------------------------------------------------------- 8. 没有 status_addr

info "8. 盘上那份没有 status_addr（设备键）：能叠、不能推，明写原因"
dx "cp $WORK/route-nostatus.base.json $WORK/route-nostatus.json" || die "写不了 route-nostatus.json"
if apply_raw $WORK/b8.json $WORK/root-ns $WORK/route-nostatus.json; then
    ok "apply rc=0（叠成功就是成功）"
else
    bad "apply 判失败"
fi
log_has "$LASTLOG" "没有 status_addr" "日志指出缺的是哪个设备键"
log_lacks "$LASTLOG" "已热更" "没有推出任何 PUT（连不上与推不了都不是完成）"
run_v "status.json：v8 / apply / ok，备注说盘上那份没有 status_addr" stat $WORK/root-ns/status.json 8 apply ok "没有 status_addr"
run_v "表落盘了、其它设备键未动、status_addr 没有凭空造出来" nostatus $WORK/route-nostatus.json $WORK/route-nostatus.base.json

# ---------------------------------------------------------------- 9. 没有 route.json

info "9. 盘上根本没有 route.json：整次 apply rc=1，但版本已经装进去"
if apply_raw $WORK/b7.json $WORK/root-missing $WORK/nope.json; then
    bad "没以错误收场"
else
    ok "rc=1（硬错误，不猜 identity_dir）"
fi
log_has "$LASTLOG" "盘上没有" "错误里点名缺的是哪份文件"
if dx "test -f $WORK/root-missing/versions/7/vpp.conf" >/dev/null 2>&1; then
    ok "版本 7 已经在 versions/ 里（叠加排在 VPP/FRR 之后）"
else
    bad "版本 7 没装进去：报错的顺序不对"
fi
run_v "status.json：v7 / apply / 失败，带错误原文" stat $WORK/root-missing/status.json 7 apply err

# ---------------------------------------------------------------- 10. 收尾

info "10. 收尾：lab 的正式配置与进程一个字节都没动"
if [ -n "$BOX_PID" ]; then
    now=$(dx "pgrep -f '$BOX_PGREP' | head -1")
    if [ "$now" = "$BOX_PID" ]; then ok "盒子上自己的 sr-route pid 未变（$BOX_PID）"
    else bad "盒子上自己的 sr-route pid 变了：$BOX_PID -> $now"; fi
else
    ok "盒子本来就没跑 sr-route（无从对比，跳过）"
fi
if [ -n "$BOX_ROUTE_SHA" ]; then
    now=$(dx "sha256sum /etc/sdwan/route.json 2>/dev/null | cut -d' ' -f1")
    if [ "$now" = "$BOX_ROUTE_SHA" ]; then ok "盒子的 /etc/sdwan/route.json 哈希未变（$BOX_ROUTE_SHA）"
    else bad "盒子的 /etc/sdwan/route.json 被改了"; fi
fi

cleanup

echo
echo "=================================="
echo "总计: $PASS 通过, $FAIL 失败"
[ "$FAIL" = "0" ] || exit 1
exit 0
