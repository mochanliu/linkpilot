#!/bin/bash
# vcpe.sh - 起一台「vCPE」：一个只做入网 + 心跳的最小 agent，跑在 lab 管理网里
#
# 它模拟客户买回来的 CPE 盒子刚上电的样子：身份目录是空的，手里只有一张
# 入网码（claim code）和控制器的 CA。入网之后设备出现在控制台里，等客户
# 在控制台配置它 —— 这一步没有任何数据面：入网不带端口、不带 overlay 地址、
# 不带密钥环。真盒子在节点计划下发后才去装 VPP，这里先不假装。
#
# 用法：
#   vcpe.sh up --claim <code>    # 首次入网（已有身份时 claim 可省略）
#   vcpe.sh up                  # 沿用已有身份，只重启
#   vcpe.sh status              # 从 agent 的 /status 看 tenant/node_id/心跳
#   vcpe.sh logs                # 跟进 agent 日志
#   vcpe.sh down                # 停掉容器（身份目录保留）
#   vcpe.sh wipe                # 删除本地身份（下次 up 必须重新入网）
#
# 选项：--name <容器名>（默认 sr-lab-vcpe）--ip <管理网地址>（默认 10.99.0.60）
#       --serial <序列号>（默认容器名）
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "${HERE}/../.." && pwd)
RUN="${HERE}/run"
IMAGE=srwg-lab:latest
NET=sdwan-lab_lab_mgmt
CTRL_HOST=lab-ctrl
CTRL_PORT=8444

NAME=sr-lab-vcpe
IP=10.99.0.60
SERIAL=""
CLAIM=""
CMD=${1:-up}
[ $# -gt 0 ] && shift

while [ $# -gt 0 ]; do
    case "$1" in
        --claim)  CLAIM=${2:-}; shift 2 ;;
        --name)   NAME=${2:-};  shift 2 ;;
        --ip)     IP=${2:-};    shift 2 ;;
        --serial) SERIAL=${2:-}; shift 2 ;;
        -h|--help) awk 'NR>1 { if ($0=="set -u") exit; sub(/^# ?/,""); print }' "$0"; exit 0 ;;
        *) echo "未知参数：$1" >&2; exit 2 ;;
    esac
done
[ -n "$SERIAL" ] || SERIAL=$NAME
IDDIR="${RUN}/vcpe-id"

die() { echo "[vcpe] $*" >&2; exit 1; }
say() { printf '\033[1;36m== %s\033[0m\n' "$*"; }
note() { printf '   %s\n' "$*"; }

running() { docker ps --format '{{.Names}}' | grep -qx "$1"; }
exists()  { docker ps -a --format '{{.Names}}' | grep -qx "$1"; }

need_lab() {
    running "$CTRL_HOST" || die "$CTRL_HOST 没在跑：先 test/lab/bringup.sh（或 --stage enroll）"
    docker exec "$CTRL_HOST" test -s /var/lib/sr-ctrl/ca.pem \
        || die "$CTRL_HOST 上没有控制器 CA：先 test/lab/bringup.sh --stage enroll"
    docker network inspect "$NET" >/dev/null 2>&1 || die "找不到网络 $NET（lab 容器没起？）"
}

cmd_up() {
    need_lab
    if exists "$NAME"; then
        die "$NAME 已存在：先 vcpe.sh down（保留身份）或 vcpe.sh wipe（连身份一起删）"
    fi
    if docker network inspect "$NET" --format '{{range .Containers}}{{.IPv4Address}} {{end}}' \
        | grep -q "${IP}/"; then
        die "$NET 里 $IP 已被占用：换 --ip"
    fi

    mkdir -p "$IDDIR"
    # 身份目录只给容器里的 root 用；0700 是 device_ed25519.key 的同级目录要求。
    chmod 700 "$IDDIR"
    # 固定 CA：设备第一次连接就先认 CA，不信任"第一次看到的那个控制器"。
    docker exec "$CTRL_HOST" cat /var/lib/sr-ctrl/ca.pem > "$IDDIR/pinned_ca.pem" \
        || die "取控制器 CA 失败"

    local fresh=1
    [ -s "$IDDIR/state.json" ] && fresh=0
    if [ "$fresh" = "1" ] && [ -z "$CLAIM" ]; then
        die "这台 vCPE 还没有身份：up --claim <入网码>（控制台 → 租户 → 复制入网码）"
    fi
    if [ "$fresh" = "0" ] && [ -n "$CLAIM" ]; then
        note "本地已有身份（tenant/node 见 vcpe.sh status），--claim 会被忽略"
    fi

    local claimflag=""
    [ -n "$CLAIM" ] && claimflag="--claim $CLAIM"

    say "启动 vCPE $NAME（$IP，serial=$SERIAL）"
    docker run -d --name "$NAME" \
        --network "$NET" --ip "$IP" \
        -v "${ROOT}/controller:/workspace/controller:ro" \
        -v "${IDDIR}:/var/lib/sdwan" \
        "$IMAGE" bash -lc "
            set -e
            cd /workspace/controller
            go build -o /tmp/sr-agent ./cmd/sr-agent
            echo '[vcpe] sr-agent 构建完成'
            exec /tmp/sr-agent \
                --ctrl ${CTRL_HOST}:${CTRL_PORT} \
                --ca /var/lib/sdwan/pinned_ca.pem \
                --id /var/lib/sdwan \
                --serial ${SERIAL} \
                --status 127.0.0.1:8089 ${claimflag}
        " >/dev/null || die "docker run 失败"

    local i
    for i in $(seq 1 60); do
        docker exec "$NAME" test -s /var/lib/sdwan/state.json 2>/dev/null && break
        running "$NAME" || { docker logs "$NAME" 2>&1 | tail -20 >&2; die "agent 退出了"; }
        sleep 0.5
    done
    docker exec "$NAME" test -s /var/lib/sdwan/state.json >/dev/null 2>&1 \
        || { docker logs "$NAME" 2>&1 | tail -20 >&2; die "60 秒内没拿到身份"; }

    wait_online || exit 1
    note "接下来：控制台 → 设备（这一台会显示 online）；隧道/端口要等节点计划，暂未实现"
}

wait_online() {
    local i st hb
    for i in $(seq 1 30); do
        st=$(status_json 2>/dev/null)
        # sr-agent 是 Go 写的，/status 是紧凑 JSON（"heartbeats":9），别按带空格
        # 的格式去 grep。
        hb=$(echo "$st" | python3 -c \
            'import json,sys;print(json.load(sys.stdin).get("heartbeats",0))' 2>/dev/null)
        if [ -n "$hb" ] && [ "$hb" -ge 1 ]; then
            say "已上线"
            echo "$st" | python3 -m json.tool | sed 's/^/   /'
            return 0
        fi
        sleep 1
    done
    echo "[vcpe] 还没有心跳成功，日志：" >&2
    docker logs "$NAME" 2>&1 | tail -20 >&2
    return 1
}

status_json() {
    docker exec "$NAME" python3 -c '
import urllib.request
print(urllib.request.urlopen("http://127.0.0.1:8089/status", timeout=3).read().decode())'
}

cmd_status() {
    exists "$NAME" || die "$NAME 不存在（vcpe.sh up --claim <code>）"
    running "$NAME" || { note "容器没在跑；身份目录里的最后状态："; ls -l "$IDDIR"; return 0; }
    status_json | python3 -c '
import json,sys
d=json.load(sys.stdin)
print("  序列号    %s" % d["serial"])
print("  租户      %s" % d["tenant_id"])
print("  node_id   %s" % d["node_id"])
print("  控制器    %s" % d["controller"])
print("  心跳      %d 次（每 %ds），最后成功 %s" % (d["heartbeats"], d["heartbeat_sec"], d.get("last_ok_unix") or "-"))
print("  epoch     %s   peers=%s" % (d["epoch"], d["peer_count"]))
print("  数据面    %s" % d["dataplane"])
if d.get("last_err"): print("  最近错误  %s" % d["last_err"])'
}

cmd_down() {
    if exists "$NAME"; then
        docker rm -f "$NAME" >/dev/null && echo "[vcpe] $NAME 已停止（身份保留在 $IDDIR）"
    else
        echo "[vcpe] $NAME 不存在"
    fi
}

cmd_wipe() {
    cmd_down
    if [ -d "$IDDIR" ]; then
        rm -rf "$IDDIR"
        echo "[vcpe] 已删除 $IDDIR —— 下次 up 必须重新入网（控制器那边会显示为失联）"
    fi
}

case "$CMD" in
    up)     cmd_up ;;
    status) cmd_status ;;
    logs)   docker logs -f --tail 50 "$NAME" ;;
    down)   cmd_down ;;
    wipe)   cmd_wipe ;;
    *)      echo "用法：vcpe.sh {up|status|logs|down|wipe} [--claim CODE]" >&2; exit 2 ;;
esac
