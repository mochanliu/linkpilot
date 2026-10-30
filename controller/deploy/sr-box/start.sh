#!/bin/bash
# sr-box 开机清单：一台盒子从上电到"路由面自己在"要走的那几步，写在一处。
#
# 为什么是一份清单而不是六个 systemd unit：这台盒子的两种形态都需要同一个顺序，
# 而只有一种形态里有 systemd ——
#   * 真盒子（有 systemd）：sr-box.service 是个薄壳，ExecStart 就是本脚本的 up；
#     vpp/frr 由它们自己的 unit 管，这里的 vpp/frr 两步只做"等它起来"。
#   * 容器 / 没有 systemd 的最小镜像：vpp 那一步是**真启动**（见下面的顺序说明）。
#     lab 里走的就是这一条，而实测的形状要说准：srwg-node1/2 的 CMD 是 /bin/bash，
#     PID 1 不是本脚本，所以容器起来之后是**人（判据脚本）敲一条 up** 把它带回来。
#     想让容器自己带上，可以把 CMD 换成 start.sh up —— 那意味着清单跑完就退出、
#     整个 PID namespace 随之清空，配合 --restart unless-stopped 就是"整盒冷重启"，
#     而不是"缺哪步补哪步"；这一条**没实测**，进程级的持续看护在 systemd 形态才有。
# 顺序、门控、就绪判据只有一份，不会因为两种形态各写一遍而走样。
#
# 顺序及理由：
#   vpp    数据的落地对象。VPP 只把 table/MTU/接口状态/lcp 配对记在内存里，重启即
#          清零，后面每一步都往它上面写，所以它必须最先。
#   apply  sr-apply boot：把**装在磁盘上**的那版客户配置（/var/lib/sr-cpe/current）
#          重放给 VPP 和内核。不问控制器 —— 设备不能因为控制器不可达就恢复不了自己。
#   frr    盒内 FRR 读 /etc/frr/frr.conf 起 zebra+bgpd。必须在 apply 之后：lcp TAP
#          是 apply 建出来的，FRR 的邻居挂在 TAP 上。
#   agent  入网身份 + 心跳 +（-apply 时）拉取发布配置 +（-tunnels 时）建隧道。
#   route  sr-route：读 /etc/sdwan/route.json，连 RR，把 RIB 摆在自己的 /status 上。
#   fib    sr-fib：轮询 sr-route 的 /status 装 VPP 表。它吃的是 route 的口，所以在 route 之后。
#   frrbridge sr-frr：一头连 FRR 的 zserv，一头往 sr-route 的 /local-prefixes 送。
#          两边都要活着，所以放最后。
#
# 三条规矩，都是这条清单存在的理由：
#   1. **门控**：一步缺什么就明说缺什么并跳过。中转-only 盒子没有 route.json，未入网的
#      盒子没有身份 —— 它们应当"跳过"，不应当"失败"，也不应当被假装成已经就绪。
#   2. **幂等**：已经在跑的进程绝不重启。跑第二遍 up 是"补上没起来的那几步"，不是
#      "把整套再来一遍"。O5 之后配置变更走 PUT / 发布，进程不需要重启，这条清单
#      也不该成为重启进程的手段（那是人为操作，不是开机）。
#   3. **失败要说清在哪一步**：每步一行结论，退出码只要有任何一步 FAIL 就非 0。
#
# 用法（在盒子里以 root 执行）：
#   start.sh up      # 从上电状态把清单走一遍（幂等）
#   start.sh check   # 只读：每步的门控与在跑与否，不启动任何东西
#   start.sh stop    # 只停本清单自己启动的 sr-* 进程（reverse order），
#                    # 不碰 VPP 与 FRR —— 那两个有自己的 service/启动器
#
# 配置：/etc/sr-box/sr-box.conf（KEY=VALUE，全部可省略，见下面的默认值）。
# 一件事这里做不到，必须先说明：**隧道不是这份清单能恢复的**。VPP 重启后 wg0 没了，
# 而建 wg 口的是 sr-agent -tunnels，它的期望值每次都是从控制器取的（internal/agent
# tunnel.go 的 fetchAndApply），磁盘上没有那份隧道表。所以控制器不可达时，盒子开机会
# 得到"VPP + 客户侧配置 + 路由面进程都在，隧道 0 条"，check 会把这一步显式说出来。

set -u

# 一台盒子只需要一个配置文件：/etc/sr-box/sr-box.conf，KEY=VALUE，全部可省略。
# 它被 source，所以和 /etc/conf.d 一样只放常量；文件归 root（0640 更好）。
# 下面每一项的默认值就是"真盒子上的样子"，lab 里换路径只要覆盖对应的那一个。
CONF=${SR_BOX_CONF:-/etc/sr-box/sr-box.conf}
[ -r "$CONF" ] && . "$CONF"

RUN=${SR_BOX_RUN:-/run/sr-box}
LOG=${SR_BOX_LOG:-/var/log/sr-box}
SR_BIN=${SR_BOX_BIN:-/usr/local/bin}

APPLY_ROOT=${SR_BOX_APPLY_ROOT:-/var/lib/sr-cpe}
VPP_CONF=${SR_BOX_VPP_CONF:-/etc/vpp/startup.conf}
VPP_WAIT=${SR_BOX_VPP_WAIT:-60}
# auto：vppctl 等不到就用手里的启动器拉一次（容器/无 systemd 形态）。
# no ：本盒子不启动 VPP（真盒子上 vpp.service 自己负责，这里只等）。
VPP_START=${SR_BOX_VPP_START:-auto}
VPP_LAUNCHER=${SR_BOX_VPP_LAUNCHER:-/usr/local/bin/start-node.sh}
VPP_LOG=${SR_BOX_VPP_LOG:-/var/log/vpp/vpp-stdout.log}

FRR_CONF=${SR_BOX_FRR_CONF_FILE:-/etc/frr/frr.conf}
FRR_INIT=${SR_BOX_FRR_INIT:-/usr/lib/frr/frrinit.sh}
FRR_WAIT=${SR_BOX_FRR_WAIT:-30}

# agent 这一步要的参数只有这几个：控制器地址、身份目录、要不要装配置/建隧道。
# 入网之后的开机不需要 claim 与 pinned CA —— 两者都已经在身份目录里了。
AGENT_CTRL=${SR_BOX_AGENT_CTRL:-}
AGENT_ID=${SR_BOX_AGENT_ID:-/etc/sdwan/identity}
AGENT_STATUS=${SR_BOX_AGENT_STATUS:-http://127.0.0.1:8089/status}
AGENT_APPLY=${SR_BOX_AGENT_APPLY:-}
AGENT_STORE=${SR_BOX_AGENT_STORE:-$APPLY_ROOT}
AGENT_VPP_API=${SR_BOX_AGENT_VPP_API:-/run/vpp/api.sock}
AGENT_TUNNELS=${SR_BOX_AGENT_TUNNELS:-}

ROUTE_CONF=${SR_BOX_ROUTE_CONF:-/etc/sdwan/route.json}
ROUTE_STATUS=${SR_BOX_ROUTE_STATUS:-}          # 留空则从 route.json 的 status_addr 推
FIB_CONF=${SR_BOX_FIB_CONF:-/etc/sdwan/fib.json}
BRIDGE_CONF=${SR_BOX_BRIDGE_CONF:-/etc/sr-frr.json}
STATUS_WAIT=${SR_BOX_STATUS_WAIT:-30}

# ------------------------------------------------------------------ 输出

say()    { printf '%-11s %s\n' "$1" "$2"; }
report() { printf '%-11s %-4s %s\n' "$1" "$2" "$3"; }
fail()   { printf '%-11s FAIL: %s\n' "$1" "$2"; FAILED=1; }
FAILED=0

# ------------------------------------------------------------------ 工具

alive() {   # <step> —— pidfile 里的**同名**进程还活着，或同名进程在跑（别人起的也算）
    local pid name
    name=$(name_of "$1")
    if [ -f "$RUN/$1.pid" ]; then
        pid=$(cat "$RUN/$1.pid" 2>/dev/null)
        # 只看"这个 pid 存在"是不够的：盒子重启后 pidfile 可能留在磁盘上（/run 未必是
        # tmpfs），而那个号已被别的进程占用 —— 那样清单会以为这一步已经好了，什么都
        # 不起。所以必须核对 /proc/<pid>/comm 就是这一步该跑的那个二进制。
        if [ -n "$pid" ] && [ -r "/proc/$pid/comm" ] &&
           [ "$(cat "/proc/$pid/comm" 2>/dev/null)" = "$name" ]; then
            return 0
        fi
    fi
    pgrep -x "$name" >/dev/null 2>&1
}

pid_of() {   # <step> —— 只报清单认得的进程：pidfile 里的号若已被别的二进制占用就当没有
    local pid name
    name=$(name_of "$1")
    if [ -f "$RUN/$1.pid" ]; then
        pid=$(cat "$RUN/$1.pid" 2>/dev/null)
        if [ -n "$pid" ] && [ "$(cat "/proc/$pid/comm" 2>/dev/null)" = "$name" ]; then
            echo "$pid"
            return
        fi
    fi
    pgrep -x "$name" 2>/dev/null | head -1
}

name_of() {
    case "$1" in
        apply) echo sr-apply ;;      # 一次性命令，不常驻；用退出码判，不看进程
        vpp)   echo vpp_main ;;
        frr)   echo zebra ;;
        agent) echo sr-agent ;;
        route) echo sr-route ;;
        fib)   echo sr-fib ;;
        bridge) echo sr-frr ;;
    esac
}

launch() {   # <step> <name> <argv...> —— 后台起一个常驻进程，日志只追加
    local step=$1 name=$2
    shift 2
    mkdir -p "$RUN" "$LOG"
    nohup "$@" >>"$LOG/$step.log" 2>&1 &
    local pid=$!
    echo "$pid" >"$RUN/$step.pid"
    # 配置读错（DisallowUnknownFields、缺证书、口被占）会立刻退出，等一秒就能看见，
    # 比"起了但什么都没干"好。
    sleep 1
    if kill -0 "$pid" 2>/dev/null; then
        return 0
    fi
    fail "$step" "$name 起来又退了（看 $LOG/$step.log）"
    rm -f "$RUN/$step.pid"
    return 1
}

wait_vpp() {   # vppctl 回话为止。VPP 的 CLI socket 在进程起来之后才出现。
    local i
    for i in $(seq 1 "$VPP_WAIT"); do
        vppctl show version >/dev/null 2>&1 && return 0
        sleep 1
    done
    return 1
}

wait_url() {   # <url> —— 本地状态口回话为止
    local url=$1 i
    for i in $(seq 1 "$STATUS_WAIT"); do
        curl -sf --max-time 2 "$url" >/dev/null 2>&1 && return 0
        sleep 0.5
    done
    return 1
}

json_field() {   # <file> <python expr over d>
    python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print($2)" "$1" 2>/dev/null
}

# route/fib/bridge 的状态口：route.json 里有 status_addr，fib/bridge 只有
# status_url，各自读自己的配置文件，不在清单里再写一遍端口。
route_status_url() {
    if [ -n "$ROUTE_STATUS" ]; then
        echo "$ROUTE_STATUS"
        return
    fi
    local a host
    a=$(json_field "$ROUTE_CONF" "d.get('status_addr') or ''")
    [ -n "$a" ] || return 1
    case "$a" in
        http://*|https://*) echo "$a"; return ;;      # 已经写成了 URL，照用
    esac
    # 监听在 0.0.0.0（或 ::、或光写一个端口）上不等于能用那个地址去访问：curl 那边
    # 换成本地回环。写了具体地址的（127.0.0.1、管理口 IP）一概不动 —— 那是运维自己选的。
    host=${a%:*}
    case "$host" in
        ""|0.0.0.0|::|\[::\]|\[::1\]|::1) a="127.0.0.1${a#"$host"}" ;;
    esac
    echo "http://$a/status"
}

# ------------------------------------------------------------------ 各步门控

# 每步两个函数：gate 决定"这台盒子该不该有这一步"，state 报现状。
# up 阶段：先 gate（不过就 say skip），再 alive（在跑就 say running），再启动 + 就绪。

gate_vpp() { return 0; }
state_vpp() { vppctl show version >/dev/null 2>&1; }

gate_apply() { [ -e "$APPLY_ROOT/current" ]; }

gate_frr() {
    [ -s "$FRR_CONF" ] && { [ -x "$FRR_INIT" ] || command -v systemctl >/dev/null 2>&1; }
}
state_frr() { vtysh -c 'show version' >/dev/null 2>&1; }

gate_agent() { [ -n "$AGENT_CTRL" ] && [ -s "$AGENT_ID/state.json" ]; }
state_agent() { curl -sf --max-time 2 "$AGENT_STATUS" >/dev/null 2>&1; }
# sr-agent 的 -status 收监听地址，AGENT_STATUS 收可读的 URL，这里换算一次。
agent_listen() { local h=${AGENT_STATUS#*://}; echo "${h%%/*}"; }

# 磁盘上那版最后一次是被谁装的、成没成。status.json 由 sr-apply 自己写。
last_apply() {
    local cur v src ok
    cur=$(readlink "$APPLY_ROOT/current" 2>/dev/null)
    v=$(json_field "$APPLY_ROOT/status.json" "d.get('version')")
    src=$(json_field "$APPLY_ROOT/status.json" "d.get('source')")
    ok=$(json_field "$APPLY_ROOT/status.json" "'ok' if d.get('ok') else 'FAIL'")
    echo "current -> ${cur:-?}，最后一次 v${v:-?} / ${src:-?} / ${ok:-没有记录}"
}

gate_route() { [ -s "$ROUTE_CONF" ]; }
state_route() {
    local u
    u=$(route_status_url) || return 1
    curl -sf --max-time 2 "$u" >/dev/null 2>&1
}

gate_fib() { [ -s "$FIB_CONF" ]; }
state_fib() { alive fib; }

gate_bridge() { [ -s "$BRIDGE_CONF" ]; }
state_bridge() { alive bridge; }

# ------------------------------------------------------------------ up

do_vpp() {
    if state_vpp; then say vpp "已在（$(vppctl show version 2>/dev/null | head -1)）"; return; fi
    if [ "$VPP_START" != auto ] || [ ! -x "$VPP_LAUNCHER" ]; then
        # 真盒子交给 vpp.service；容器里没有启动器也起不来。等一会儿再判。
        if wait_vpp; then say vpp "已由别人拉起"; return; fi
        say vpp "SKIP: 没有 VPP，且 VPP_START=$VPP_START、启动器 $VPP_LAUNCHER 不可执行"
        say vpp "  → 这一步在 systemd 形态下由 vpp.service 负责；容器形态请挂 $VPP_LAUNCHER"
        return
    fi
    # VPP 必须以伪终端方式启动：无 PTY 的后台启动会在 exec 会话回收后静默退出
    # （start-node.sh 里记着实测）。所以是 exec 启动器而不是 nohup 二进制。
    say vpp "拉起 VPP（$VPP_LAUNCHER，conf=$VPP_CONF，日志 $VPP_LOG）"
    mkdir -p "$(dirname "$VPP_LOG")"
    setsid nohup "$VPP_LAUNCHER" sr-box >>"$VPP_LOG" 2>&1 &
    echo $! >"$RUN/vpp.pid"
    if wait_vpp; then
        say vpp "起来了（插件 $(vppctl show plugins 2>/dev/null | grep -c sr_wg_plugin.so) 份 sr_wg）"
    else
        fail vpp "$VPP_WAIT 秒内 vppctl 没回话（看 $VPP_LOG）"
    fi
}

do_apply() {
    if ! gate_apply; then
        say apply "SKIP: $APPLY_ROOT/current 不在（这台盒子没装过客户配置）"
        return
    fi
    say apply "重放磁盘上那版（sr-apply boot）"
    if "$SR_BIN/sr-apply" boot --root "$APPLY_ROOT" >>"$LOG/apply.log" 2>&1; then
        say apply "重放完成：$(json_field "$APPLY_ROOT/status.json" "'v%s %s %s' % (d.get('version'), d.get('source'), 'ok' if d.get('ok') else 'FAIL')")"
    else
        fail apply "sr-apply boot 返回非 0（看 $LOG/apply.log）"
    fi
}

do_frr() {
    if ! gate_frr; then
        say frr "SKIP: $FRR_CONF 是空的（这台盒子不跑 FRR）"
        return
    fi
    if state_frr; then say frr "已在跑"; return; fi
    if [ -x "$FRR_INIT" ]; then
        "$FRR_INIT" start >>"$LOG/frr.log" 2>&1 || { fail frr "$FRR_INIT start 失败（看 $LOG/frr.log）"; return; }
    else
        systemctl start frr >>"$LOG/frr.log" 2>&1 || { fail frr "systemctl start frr 失败"; return; }
    fi
    local i
    for i in $(seq 1 "$FRR_WAIT"); do
        state_frr && { say frr "起来了（zebra+bgpd）"; return; }
        sleep 1
    done
    fail frr "$FRR_WAIT 秒内 vtysh 没回话"
}

do_agent() {
    if [ -z "$AGENT_CTRL" ]; then
        say agent "SKIP: sr-box.conf 里没有 SR_BOX_AGENT_CTRL（这台盒子没登记要连哪个控制器）"
        return
    fi
    if [ ! -s "$AGENT_ID/state.json" ]; then
        say agent "SKIP: 身份目录 $AGENT_ID 里没有 state.json（这台盒子还没入网）"
        return
    fi
    if alive agent; then say agent "已在跑（pid $(pid_of agent)）"; return; fi
    local args=(-ctrl "$AGENT_CTRL" -id "$AGENT_ID" -status "$(agent_listen)")
    [ -n "$AGENT_APPLY" ] && args+=(-apply -store "$AGENT_STORE")
    [ -n "$AGENT_VPP_API" ] && args+=(-vpp-api "$AGENT_VPP_API")
    [ -n "$AGENT_TUNNELS" ] && args+=(-tunnels)
    launch agent sr-agent "$SR_BIN/sr-agent" "${args[@]}" || return
    if state_agent; then
        say agent "在跑，$AGENT_STATUS 已回话（心跳通不通看 /status 里的 last_ok）"
    else
        fail agent "进程在但 $AGENT_STATUS 没回话（看 $LOG/agent.log）"
    fi
}

do_route() {
    if ! gate_route; then
        say route "SKIP: 没有 $ROUTE_CONF（中转-only 盒子不跑路由面）"
        return
    fi
    if alive route; then
        say route "已在跑（pid $(pid_of route)）"
        return
    fi
    launch route sr-route "$SR_BIN/sr-route" -config "$ROUTE_CONF" || return
    local u
    u=$(route_status_url)
    if [ -n "$u" ] && wait_url "$u"; then
        say route "状态口 $u 已回话（RR 同步要几秒，check 里看 active_rr）"
    else
        fail route "进程在但状态口没回话：${u:-route.json 里没有 status_addr}（看 $LOG/route.log）"
    fi
}

do_fib() {
    if ! gate_fib; then
        say fib "SKIP: 没有 $FIB_CONF"
        return
    fi
    if alive fib; then say fib "已在跑"; return; fi
    launch fib sr-fib "$SR_BIN/sr-fib" -config "$FIB_CONF" || return
    local n
    n=$(vppctl show ip fib table "$(json_field "$FIB_CONF" "(d.get('vrfs') or [0])[0]")" 2>/dev/null |
        grep -c 'mpls via' || true)
    say fib "在跑，表 $(json_field "$FIB_CONF" "(d.get('vrfs') or [0])[0]") 里 $n 条带标签的路由（下一轮轮询还会补）"
}

do_bridge() {
    if ! gate_bridge; then
        say bridge "SKIP: 没有 $BRIDGE_CONF（这台盒子不做 FRR 双向导入）"
        return
    fi
    if alive bridge; then say bridge "已在跑"; return; fi
    launch bridge sr-frr "$SR_BIN/sr-frr" -config "$BRIDGE_CONF" || return
    say bridge "在跑"
}

run_up() {
    mkdir -p "$RUN" "$LOG"
    do_vpp
    do_apply
    do_frr
    do_agent
    do_route
    do_fib
    do_bridge
    tunnels_check
    [ "$FAILED" = 0 ] || return 1
    return 0
}

# 隧道不在清单的"能恢复"之列（文件头说明过）：这里只把它说出来，让 check/up 的
# 结论里看得见，不用一句"全部完成"盖掉。
tunnels_check() {
    state_vpp || return 0
    local n
    n=$(vppctl show wireguard interface 2>/dev/null | grep -cE '^\[[0-9]+\][[:space:]]+wg[0-9]' || true)
    if [ "$n" = 0 ]; then
        say tunnels "0 条：VPP 里的 wg 口是 sr-agent -tunnels 从**控制器**取期望值建的，"
        say tunnels "  磁盘上没有那份表。控制器不可达时开机就是这个形状。"
    else
        say tunnels "$n 条"
    fi
}

# ------------------------------------------------------------------ check

run_check() {
    mkdir -p "$RUN"
    report STEP GATE 现状

    if state_vpp; then
        report vpp 有 "VPP 活着（$(vppctl show version 2>/dev/null | head -1)）"
    else
        report vpp 有 "该有 VPP，但 vppctl 不回话"
    fi

    if gate_apply; then
        report apply 有 "$(last_apply)"
    else
        report apply 无 "$APPLY_ROOT/current 不在（这台盒子没装过客户配置）"
    fi

    if gate_frr; then
        if state_frr; then report frr 有 "zebra+bgpd 在跑"; else report frr 有 "配置在，进程没跑"; fi
    else
        report frr 无 "$FRR_CONF 是空的（这台盒子不跑 FRR）"
    fi

    if [ -z "$AGENT_CTRL" ]; then
        report agent 无 "sr-box.conf 里没有 SR_BOX_AGENT_CTRL"
    elif [ ! -s "$AGENT_ID/state.json" ]; then
        report agent 无 "身份目录里没 state.json（未入网）"
    elif state_agent; then
        report agent 有 "$AGENT_STATUS 在回话"
    else
        report agent 有 "身份在，进程或状态口没有"
    fi

    if gate_route; then
        if state_route; then
            report route 有 "device_id $(json_field "$ROUTE_CONF" "d.get('device_id')") 的状态口在回话"
        else
            report route 有 "配置在，进程没跑"
        fi
    else
        report route 无 "$ROUTE_CONF 不在（中转-only 盒子不跑路由面）"
    fi

    if gate_fib; then
        if alive fib; then report fib 有 "在跑（pid $(pid_of fib)）"; else report fib 有 "配置在，进程没跑"; fi
    else
        report fib 无 "$FIB_CONF 不在"
    fi

    if gate_bridge; then
        if alive bridge; then report bridge 有 "在跑（pid $(pid_of bridge)）"; else report bridge 有 "配置在，进程没跑"; fi
    else
        report bridge 无 "$BRIDGE_CONF 不在"
    fi

    tunnels_check
    return 0
}

# ------------------------------------------------------------------ stop

run_stop() {
    local s pid name
    # 容器形态下 $RUN/vpp.pid 也是这份清单写的（vpp 那一步真的把 VPP 拉起来了），
    # 但这里仍然不碰它：停 VPP = 现网断流，那不该是"停一份清单"的副作用。
    # 要停数据面请明着停（vpp.service / 那个启动器自己的办法）。
    for s in bridge fib route agent; do
        [ -f "$RUN/$s.pid" ] || continue
        name=$(name_of "$s")
        pid=$(cat "$RUN/$s.pid")
        # 只杀 pidfile 里那个**确实还是本步二进制**的进程：盒子重启后 pidfile 可能是旧的，
        # 号已经被别人用走，照着念 SIGTERM 就是杀无辜。
        if [ -n "$pid" ] && [ "$(cat "/proc/$pid/comm" 2>/dev/null)" = "$name" ]; then
            kill "$pid" 2>/dev/null
            say stop "SIGTERM $s (pid $pid)"
        else
            say stop "$s 的 pidfile 是陈的（$pid 不是 $name），只删文件"
        fi
        rm -f "$RUN/$s.pid"
    done
    say stop "VPP 与 FRR 没动（它们不归这份清单管）"
}

case "${1:-up}" in
    up)    run_up ;;
    check) run_check ;;
    stop)  run_stop ;;
    -h|--help|help)
        awk 'NR>1 { if ($0=="set -u") exit; sub(/^# ?/,""); print }' "$0"
        ;;
    *) echo "用法: start.sh {up|check|stop}" >&2; exit 2 ;;
esac
