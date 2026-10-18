#!/bin/bash
# attr-cpu.sh - 隧道跑分时的瓶颈归因：谁在流油、谁还很空、包在哪一跳丢掉
#
# 三个独立口径（缺一个就会得出错结论）：
#   1) cgroup cpu.stat 的 usage_usec 差分 -> 每个容器实际占几个核，和它的配额对比
#   2) sr-wg 计数器 + show interface 包数差分 -> 逐跳 pps，定位丢包发生在哪一跳
#   3) 由 1)+2) 反推出口 CPE 的「单包实际 CPU 功」
#
# 为什么必须有第 3 口径（本轮实测撞出来的，重要）：
#   `show runtime` 的 Packet-Clocks 用 TSC 计 **墙钟**，节点在 socket 上阻塞等待的
#   时间也算进去。af-packet 的 TX 会在 socket 上等，所以它报的 7.8e4 周期/包大部分
#   是"等"不是 CPU 功。并行回读修正后实测：出口 CPE 0.28 核 @ 30k pps，全链单包实际
#   CPU 功 ≈2.9e4 周期（入口 CPE ≈3.6e4），都小于 Packet-Clocks 单给 host-eth0-tx 的
#   7.8e4 —— 那个数不能当 CPU 成本引用。sr-wg 的三个节点不阻塞，它们的 Packet-Clocks 可信。
#
# 读数坑（都撞过）：
#   - vppctl 输出经 PTY，行尾带 \r，awk 的等值匹配必须先 tr -d '\r'。
#   - VPP 把主线程改名 vpp_main，pgrep -x vpp 抓不到。
#   - `show interface` 里 "rx packets" 打在接口名同一行（$7），"tx packets" 单独一行
#     （$3），"drops" 也单独一行（$2）——取法不同，统一走 vparse 一次取全三列。
#   - iperf3 客户端 -t 跑完就退出；跨命令的往返间隔常常超过 -t，采样窗口会落在流量
#     之外（表现是"占核 0.28、pps 0"）。所以本脚本自己起流，并且等它跑完再读汇总。
#   - 差分必须「一个容器一次 docker exec」：拆成十几次并行 exec，Docker 仍会把它们
#     排到不同瞬间，相邻跳之间冒出 10%+ 的假丢包（实测 node2 自己的 host-eth2 tx 比
#     它的 Decap ok 高 17%，物理上不可能）。改成一容器一次回读后，同一容器内的
#     rx/tx/sr 计数完全自洽。每个 key 还带自己的读表时刻，速率除以实测跨度而不是 WIN。
#   - 满载时同一条 sh -c 里的第二条 vppctl 会退到 CLI socket，首行输出变成
#     `vpp# SR-WG Statistics:`。按行首匹配的话整张计数器会**静默丢掉**（表现是
#     "读数缺失"，而不是报错）。vparse 先 sub 掉 `^vpp[#$] +` 再说。
#   - 这台 Mac 的 /bin/bash 是 3.2：没有关联数组；函数体里单引号串后必须 `;` 才收 `}`。
#
# 用法:
#   RATE=400M LEN=1200 WIN=15 test/scripts/attr-cpu.sh
#   RATE=400M STREAMS=4 test/scripts/attr-cpu.sh   # 4 流共 1.6G，压到隧道上限以上
#   TRAFFIC=0 test/scripts/attr-cpu.sh        # 零流量对照，所有差分必须≈0
set -u
NODES="srwg-node1 srwg-transit srwg-node2"
TERMS="srwg-term-a srwg-term-b"
WIN=${WIN:-15}; RATE=${RATE:-400M}; LEN=${LEN:-1200}; STREAMS=${STREAMS:-1}
D=${TEST_DURATION:-$((WIN + 15))}      # 流要盖住采样窗，还要自己能跑完出汇总行
MHZ=${MHZ:-3200}                      # Docker VM 报 3191.989 MHz
TERM_A=${TERM_A:-srwg-term-a}; TERM_B=${TERM_B:-srwg-term-b}
TERM_B_IP=${TERM_B_IP:-10.20.2.3}
CPE_A=${CPE_A:-srwg-node1}; CPE_B=${CPE_B:-srwg-node2}
TRANSIT=${TRANSIT:-srwg-transit}
LOG=${LOG:-/tmp/attr-one.log}

# 一个容器一次 docker exec 读全，且两张表在容器内并发取、各自带首尾时间戳。
# 两条硬要求（都是本轮实测撞出来的）：
#   - 不能拆成十几次 exec：Docker 会把它们排到不同瞬间，相邻跳之间冒出 10%+ 假丢包。
#   - 满载时一次 vppctl 要 4~7s（主线程忙），所以串行两张表会让两张表差开好几秒，
#     必须并发；每个 key 的时刻取该次读数首尾时间的中点，速率再除以实测跨度。
vread() { local c=$1 i out=""
    # 拿不到 sr-wg 那张表就重试（最多三次，每次间隔 1s）；重试只影响该容器的差分跨度，
    # 而速率一律除以实测跨度，所以不会被拉长/缩短带偏。
    for i in 1 2 3; do
        out=$(docker exec "$c" sh -c '
            A=$(date +%s.%N)
            V=$(sed -n "s/^usage_usec \([0-9]*\).*/\1/p" /sys/fs/cgroup/cpu.stat | head -1)
            B=$(date +%s.%N); echo "CORE;$A;$B;$V"
            # 内核在 af-packet 之前收到的包量：和下面 VPP 的 rx 计数一比，就知道丢包
            # 发生在「内核交给 VPP」之前还是之后（实测：内核 155.6k pps、rx_dropped 0，
            # VPP 只取到 28.6k —— 所以是 af-packet 取包的节奏，不是台子丢的）。
            for d in eth0 eth1 eth2; do
                [ -r /sys/class/net/$d/statistics/rx_packets ] || continue
                echo "KS;$d;$A;$B;$(cat /sys/class/net/$d/statistics/rx_packets)"
            done
            rm -f /tmp/.srwg1 /tmp/.srwg2 /tmp/.srwg1.out /tmp/.srwg2.out
            ( A=$(date +%s.%N); vppctl show interface > /tmp/.srwg1 2>/dev/null
              B=$(date +%s.%N); { echo "TS;$A;$B"; cat /tmp/.srwg1; } > /tmp/.srwg1.out ) &
            ( A=$(date +%s.%N); vppctl show sr-wg stats > /tmp/.srwg2 2>/dev/null
              B=$(date +%s.%N); { echo "TS;$A;$B"; cat /tmp/.srwg2; } > /tmp/.srwg2.out ) &
            wait
            cat /tmp/.srwg1.out /tmp/.srwg2.out
            rm -f /tmp/.srwg1 /tmp/.srwg2 /tmp/.srwg1.out /tmp/.srwg2.out' 2>/dev/null | tr -d '\r')
        case "$out" in *"SR-WG Statistics"*) printf '%s\n' "$out" | vparse "$c"; return ;; esac
        sleep 1
    done
    echo "警告：${c} 三次都没读到 sr-wg 计数器，这一轮它的 sr 行会显示读数缺失" >&2
    printf '%s\n' "$out" | vparse "$c"; }
pread() { local c=$1
    docker exec "$c" sh -c 'A=$(date +%s.%N)
        V=$(sed -n "s/^usage_usec \([0-9]*\).*/\1/p" /sys/fs/cgroup/cpu.stat | head -1)
        B=$(date +%s.%N); echo "CORE;$A;$B;$V"' 2>/dev/null | vparse "$c"; }
# 拍平成 "key<TAB>累计值<TAB>读表中点"；key 用 ; 分层，跨度由 join 后的两个中点相减
# 注意 sub(/^vpp[#$] +/)：满载时第二条 vppctl 会退到 CLI socket，第一行输出前面带
# "vpp# " 提示符（实测过），不去掉就匹配不到表头，整张 sr-wg 计数器会被静默丢掉。
vparse() { awk -v c="$1" '
{ sub(/^vpp[#$] +/, "") }
/^CORE;/ { split($0, p, ";"); printf "核;%s\t%s\t%.3f\n", c, p[4], (p[2]+p[3])/2; next }
/^KS;/    { split($0, p, ";"); printf "ks;%s;%s;rx\t%s\t%.3f\n", c, p[2], p[5], (p[3]+p[4])/2; next }
/^TS;/   { split($0, p, ";"); ts=(p[2]+p[3])/2; mode=""; next }
/^SR-WG Statistics/ { mode="sr"; next }
mode!="sr" {
    if (NF>=4 && $2 ~ /^[0-9]+$/ && ($3=="up" || $3=="down")) { cur=$1; rx=$7; tx=""; dr="" }
    else if ($1=="tx" && $2=="packets") { tx=$3 }
    else if ($1=="drops" && cur!="") { dr=$2
        printf "if;%s;%s;rx\t%s\t%.3f\n", c, cur, rx, ts
        printf "if;%s;%s;tx\t%s\t%.3f\n", c, cur, tx, ts
        printf "if;%s;%s;drops\t%s\t%.3f\n", c, cur, dr, ts }
    next }
{ if (NF==1 && $1 ~ /:$/) { sec=$1; sub(/:$/, "", sec); next }
  if ($1=="ok:") printf "sr;%s;%s\t%s\t%.3f\n", c, sec, $2, ts }'; }
quota() { docker exec "$1" awk '{if($1=="max"){printf "不限"}else{printf "%.1f 核", $1/$2}}' /sys/fs/cgroup/cpu.max; }

rm -f "$LOG"
docker exec "$TERM_A" pkill -9 -x iperf3 2>/dev/null || true
if [ "$TERM_A" != "$TERM_B" ]; then
    docker exec "$TERM_B" pkill -9 -x iperf3 2>/dev/null || true
    docker exec "$TERM_B" iperf3 -s -p 5201 --daemon >/dev/null 2>&1 || true
    # 必须等监听真的起来：pkill 之后 --daemon 只有几毫秒的空档，客户端在这段时间发起
    # 就是 Connection refused、进程立刻退出（perf-bench 全流程里撞到过一次，整表 0）。
    # 探活用 /proc/net/tcp 的 0A(LISTEN)，别真连上去——半开连接会让 iperf3 服务端起个
    # 子进程占住端口，反而把真正的测试挤掉。5201 = 0x1459。
    for i in 1 2 3 4 5 6 7 8 9 10; do
        docker exec "$TERM_B" awk '$2 ~ /:1459$/ && $4 == "0A" { f = 1 } END { exit !f }' /proc/net/tcp && break
        sleep 1
    done
fi
sleep 1
LEFT=$(docker exec "$TERM_A" pgrep -c -x iperf3)
[ "${LEFT}" = "0" ] || echo "警告：${TERM_A} 上还有 ${LEFT} 个 iperf3，速率会翻倍"
if [ "${TRAFFIC:-1}" = "1" ]; then
    docker exec -d "$TERM_A" bash -c "iperf3 -c ${TERM_B_IP} -u -b ${RATE} -l ${LEN} -P ${STREAMS} -t ${D} > ${LOG} 2>&1"
    sleep 8
    if [ "$(docker exec "$TERM_A" pgrep -c -x iperf3)" = "0" ]; then
        echo "客户端第一次没起来（$(docker exec "$TERM_A" head -2 "$LOG" 2>/dev/null | tr '\n' ' ')），重试一次" >&2
        docker exec "$TERM_B" iperf3 -s -p 5201 --daemon >/dev/null 2>&1 || true
        sleep 2
        docker exec -d "$TERM_A" bash -c "iperf3 -c ${TERM_B_IP} -u -b ${RATE} -l ${LEN} -P ${STREAMS} -t ${D} > ${LOG} 2>&1"
        sleep 8
    fi
    echo "负载中 ${TERM_A} iperf3 进程数 = $(docker exec "$TERM_A" pgrep -c -x iperf3)"
fi

# 五个容器并行回读，每个容器内部一次取全
T=$(mktemp -d) || exit 1
snap() { local out=$1 c
    for c in $TERMS; do ( pread "$c" > "$T/p.$c" ) & done
    for c in $NODES; do ( vread "$c" > "$T/v.$c" ) & done
    wait
    cat "$T"/* >> "$out"; }
F1=$(mktemp); F2=$(mktemp)
snap "$F1"; sleep "$WIN"; snap "$F2"
# join 后每行：key  前值  前时刻  后值  后时刻；速率一律除以各自的实测跨度
J=$(join -t $'\t' <(sort "$F1") <(sort "$F2"))

echo "--- 逐跳 pps（目标 ${STREAMS} x ${RATE} 载荷 ${LEN}B ≈ $(awk -v r="$RATE" -v l="$LEN" -v p="$STREAMS" 'BEGIN{gsub(/[^0-9.]/,"",r); printf "%.0f", r*1e6/8/l*p}') pps）---"
echo "$J" | awk -F'\t' -v A="$CPE_A" -v TR="$TRANSIT" -v B="$CPE_B" '
function g(k) { return (k in R) ? R[k] : -1 }
function row(l, k,   s, q) {
    q = g(k)
    if (q < 0) { printf "  %-24s 读数缺失\n", l; return }
    s = sprintf("  %-24s %9.0f pps", l, q)
    if (prev > 1000) s = s sprintf("   相对上一跳 %+6.2f%%", (q-prev)/prev*100)
    print s; prev = q
}
function krow(l, k,   q) { q = g(k)
    if (q < 0) { printf "  %-24s 读数缺失\n", l; return }
    printf "  %-24s %9.0f pps  （内核侧，交给 VPP 之前）\n", l, q }
$5-$3 > 0.05 { R[$1] = ($4-$2)/($5-$3) }
/^核;/       { split($1,k,";"); sp[k[2]] = $5-$3 }
END{
    prev = 0
    # 先看两台 CPE 的内核侧收包速率：offer 打满时，这一行与下面 VPP 的 rx 行之差
    # 就是「af-packet 没来得及交给 VPP」的量，跟隧道本身无关。
    krow("node1 内核 eth2 rx", "ks;"A";eth2;rx")
    krow("node2 内核 eth0 rx", "ks;"B";eth0;rx")
    row("node1  host-eth2 rx",   "if;"A";host-eth2;rx")
    row("node1  sr-wg Encap ok", "sr;"A";Encap")
    row("node1  host-eth0 tx",   "if;"A";host-eth0;tx")
    row("transit host-eth0 rx",  "if;"TR";host-eth0;rx")
    row("transit sr-wg Transit", "sr;"TR";Transit")
    row("transit host-eth1 tx",  "if;"TR";host-eth1;tx")
    row("node2  host-eth0 rx",   "if;"B";host-eth0;rx")
    row("node2  sr-wg Decap ok", "sr;"B";Decap")
    row("node2  host-eth2 tx",   "if;"B";host-eth2;tx")
    # 相邻跳之间少了包，先看这两条 af-packet 的 drops 有没有对上：
    # 对不上就说明丢在 VPP 之前/之后的内核 veth-网桥里，不是 VPP 丢的。
    drop1 = "if;"A";host-eth2;drops"; drop2 = "if;"B";host-eth0;drops"
    if (g(drop1) >= 0) printf "  %s 边缘口 drops %.0f/s\n", A, g(drop1)
    if (g(drop2) >= 0) printf "  %s 下开口 drops %.0f/s\n", B, g(drop2)
    printf "  差分跨度 %s=%.2fs %s=%.2fs %s=%.2fs（差得多于 0.3s 时相邻跳的百分比只能看趋势）\n", A, sp[A], TR, sp[TR], B, sp[B]
}'
echo "--- 每容器实际占核 / 配额 ---"
echo "$J" | awk -F'\t' '/^核;/ && $5-$3>0.05 {split($1,k,";"); printf "%s\t%.2f\n", k[2], ($4-$2)/1e6/($5-$3)}' > /tmp/attr-cores
while read -r c v; do
    printf '  %-13s 占核 %-6s 配额 %s\n' "$c" "$v" "$(quota "$c")"
done < /tmp/attr-cores
echo "--- 单包实际 CPU 功 = 占核 x ${MHZ}MHz / pps（不是 show runtime 的周期/包，见文件头）---"
echo "$J" | awk -F'\t' -v m="$MHZ" -v A="$CPE_A" -v TR="$TRANSIT" -v B="$CPE_B" '
$5-$3 > 0.05 { R[$1] = ($4-$2)/($5-$3) }
/^核;/       { split($1,k,";"); c[k[2]] = ($4-$2)/1e6/($5-$3) }
END{
    who[1]="出口 CPE 全程"; cn[1]=A; k[1]="if;"A";host-eth0;tx"
    who[2]="中转 全程";     cn[2]=TR; k[2]="sr;"TR";Transit"
    who[3]="入口 CPE 全程"; cn[3]=B; k[3]="if;"B";host-eth0;rx"
    for(i=1;i<=3;i++){ q=R[k[i]]
        if(q>1000) printf "  %-14s %-12s %.2f 核 / %8.0f pps = %.3e 周期/包\n", who[i], cn[i], c[cn[i]], q, c[cn[i]]*m*1e6/q
        else       printf "  %-14s %-12s %.2f 核 / %8.0f pps  零流量对照，差分≈0 即方法可信\n", who[i], cn[i], c[cn[i]], q
    }
}'
echo "--- 按「VPP 收到的包」归一化的成本（对拓扑不敏感，用来跨配置比：无 WG / 纯 WG / SR+WG）---"
echo "$J" | awk -F'\t' -v m="$MHZ" -v A="$CPE_A" -v TR="$TRANSIT" -v B="$CPE_B" -v L="${LABEL:-}" '
$5-$3 > 0.05 { R[$1] = ($4-$2)/($5-$3) }
/^核;/       { split($1,k,";"); c[k[2]] = ($4-$2)/1e6/($5-$3) }
END{
    printf "  标签 %s\n", L
    n[1]="node1 收包"; cn[1]=A; k[1]="if;"A";host-eth2;rx"
    n[2]="中转 收包";  cn[2]=TR; k[2]="if;"TR";host-eth0;rx"
    n[3]="node2 收包"; cn[3]=B; k[3]="if;"B";host-eth0;rx"
    for(i=1;i<=3;i++){ q=R[k[i]]
        if(q>1000) printf "  COST %-10s %-12s %8.0f pps  %.2f 核  %.3e 周期/包\n", n[i], cn[i], q, c[cn[i]], c[cn[i]]*m*1e6/q
        else       printf "  COST %-10s %-12s 收包差分≈0，这一轮没有流量\n", n[i], cn[i]
    }
}'
echo "--- 实收（客户端自报，覆盖整条流而非只覆盖采样窗）---"
if [ "${TRAFFIC:-1}" = "1" ]; then
    for i in $(seq 1 $((D + 20))); do
        [ "$(docker exec "$TERM_A" pgrep -c -x iperf3)" = "0" ] && break
        sleep 1
    done
fi
docker exec "$TERM_A" grep -E 'sender|receiver|error' "$LOG" 2>/dev/null | tail -2
rm -r "$T"; rm -f "$F1" "$F2" /tmp/attr-cores "$LOG"
docker exec "$TERM_A" pkill -9 -x iperf3 2>/dev/null || true
