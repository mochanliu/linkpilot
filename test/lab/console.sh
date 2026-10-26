#!/bin/bash
# console.sh - 打开 lab 控制台的地址与登录令牌
#
# 控制台是 sr-ctrl 的第二个 listener：明文 HTTP，只发布到宿主机回环
# （127.0.0.1:8088 → lab-ctrl:8080）。设备面 /v1/* 仍然只在 lab 网络里走 mTLS，
# 不在这里。浏览器上不了 mTLS，所以人用的是这条通道，凭一张 token 换会话 cookie。
#
# 用法：
#   test/lab/console.sh           # 打印地址与 token，并检查在不在跑
#   test/lab/console.sh restart   # 重启 sr-ctrl 的整个进程（设备面 + 控制台）
#
# 注意：控制台和设备面在同一个进程里，restart 会短暂断开各 CPE 的入网面；
# lab 里 CPE 只是拿过证书，心跳由 sr-agent/vcpe 自己重试，不会丢身份。
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
RUN="${HERE}/run"
CTRL=lab-ctrl
CTRL_ADDR=10.99.0.200
CTRL_PORT=8444
URL=http://127.0.0.1:8088/

die() { echo "[console] $*" >&2; exit 1; }

running() { docker ps --format '{{.Names}}' | grep -qx "$1"; }

token() {
    if [ -s "$RUN/console-token" ]; then
        cat "$RUN/console-token"
    else
        docker exec "$CTRL" cat /var/lib/sr-ctrl/console-token 2>/dev/null
    fi
}

cmd_restart() {
    running "$CTRL" || die "$CTRL 没在跑：先 test/lab/bringup.sh"
    local tok
    tok=$(token)
    [ -n "$tok" ] || die "找不到控制台 token（$RUN/console-token 或容器内 /var/lib/sr-ctrl/console-token）"
    docker exec "$CTRL" pkill -f '/usr/local/bin/sr-ctrl' 2>/dev/null
    sleep 1
    docker exec -d "$CTRL" sh -c "mkdir -p /var/lib/sr-ctrl && /usr/local/bin/sr-ctrl \
        -data /var/lib/sr-ctrl -listen 0.0.0.0:${CTRL_PORT} \
        -tls-san ${CTRL_ADDR},10.88.1.200,lab-ctrl \
        -console 0.0.0.0:8080 -admin-token ${tok} \
        > /var/log/sr-ctrl.log 2>&1"
    sleep 2
    echo "[console] sr-ctrl 已重启"
}

cmd_show() {
    running "$CTRL" || die "$CTRL 没在跑：先 test/lab/bringup.sh"
    local tok code
    tok=$(token)
    [ -n "$tok" ] || die "找不到控制台 token（先跑 bringup.sh --stage enroll）"
    code=$(curl -s -o /dev/null -w '%{http_code}' "${URL}api/session" 2>/dev/null)
    echo "  地址   $URL"
    echo "  token  $tok"
    echo "  HTTP   ${code:-连接失败}（/api/session 未登录时也回 200，body 里 logged_in:false）"
    if ! docker exec "$CTRL" pgrep -f '/usr/local/bin/sr-ctrl' >/dev/null; then
        echo "  警告   sr-ctrl 进程不在（bringup.sh --stage enroll 或 console.sh restart）"
    fi
}

case "${1:-show}" in
    restart) cmd_restart; cmd_show ;;
    show|"") cmd_show ;;
    *) echo "用法：console.sh [restart]" >&2; exit 2 ;;
esac
