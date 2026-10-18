#!/bin/bash
# perf-bench.sh - 在 perf-rig.sh 搭好的「终端-CPE-中转-CPE-终端」台子上测隧道带宽
#
# 用法:
#   test/scripts/perf-bench.sh                 # 标准一轮（TCP + UDP 阶梯 + 周期采样）
#   test/scripts/perf-bench.sh --udp-only      # 只跑 UDP 阶梯
#   test/scripts/perf-bench.sh --tcp-only      # 只跑 TCP（抖动观测）
#   TEST_DURATION=8 TCP_ROUNDS=3 bash test/scripts/perf-bench.sh
#
# 输出两类数：
#   1) 端到端带宽/pps（受 Docker af-packet + 单 VPP 线程限制，只能当下限看）
#   2) 每个数据面节点的 周期/包（show runtime 的 Packet-Clocks 是采样窗口平均值，
#      所以必须在流量跑起来的中途取样，跑完再取就归零了）
#
# 判定口径：性能归因看 2)，绝对带宽看 1) 但必须标注测试台限制。
#
# 2026-10-18 基线（CPE 锁 2 vCPU / 2 GB，VPP 单线程 vpp_main，path MTU 1360）：
#   端到端  TCP 单流 169~175 Mbit/s；TCP 4 流 190~200 Mbit/s
#           UDP 1200B：100/150/200 Mbit/s 全 0 丢包，250M 起丢 7.7%，400M 丢 40%，
#                       即饱和点 ≈225 Mbit/s（≈23.5 kpps）
#           UDP 200B： 上限 ≈43 Mbit/s（≈27 kpps），50M 就已经丢 7.4%
#           4 流洪泛： 灌 1.99 Gbit/s 只过 196 Mbit/s（丢 90%）
#   周期/包（负载中途取样，两轮复现一致）：
#           sr-wg4-encap 623 / sr-wg4-transit 250 / sr-wg4-input(decap) 1150
#           wg4-output-tun 1.95e4 / wg4-input 1.86e4 / host-*-tx 6.6e4~7.8e4
#   结论：SR 三段合计 ≈2.0k 周期/包，只占出口节点单包成本（WG+af-packet TX ≈9.8e4）
#         的 2% 左右；这台台子的 pps 天花板是 af-packet 的 TX 开销决定的
#         （78k 周期/包 ≈ 2.6GHz 下 3 万多 pps），不是我们的插件。
#         nr_throttled=0 说明 2 vCPU 限额没有限流——VPP 只有一个线程在跑。
set -u

TERM_A=${TERM_A:-srwg-term-a}
TERM_B_IP=${TERM_B_IP:-10.20.2.3}
CPE_A=${CPE_A:-srwg-node1}
NODES=${NODES:-"srwg-node1 srwg-transit srwg-node2"}
WATCH=${WATCH:-"ip4-lookup ip4-midchain sr-wg4-encap sr-wg4-transit sr-wg4-input wg4-input wg4-output-tun host-eth0-tx"}
DURATION=${TEST_DURATION:-8}
UDP_ONLY=0; TCP_ONLY=0
for a in "$@"; do
    [ "$a" = "--udp-only" ] && UDP_ONLY=1
    [ "$a" = "--tcp-only" ] && TCP_ONLY=1
done

say() { printf '\n== %s\n' "$*"; }

iperf_run() { # $* = iperf3 参数
    # iperf3 的列是 "<数值> Mbits/sec"，速率在单位的前一列；尾部（重传/丢包率）
    # 列数不固定，所以取速率之后的所有字段。
    # 坑：TCP 的 receiver 行窗口会拖过 -t 设定的秒数（实测 5s 的测试接收侧报
    # 0.00-15.11，于是"实收"被摊薄成 52 Mbit/s 而发送侧 158）。所以 TCP 只看
    # 发送行，同时把窗口秒数一起打印出来，别拿摊薄值当结论。
    docker exec "${TERM_A}" bash -c "timeout $((DURATION + 45)) iperf3 -c ${TERM_B_IP} -t ${DURATION} $* 2>&1" |
        awk '/SUM|^\[  5\]/ && /sender|receiver/ {
                r=-1; for(i=1;i<=NF;i++) if($i ~ /bits\/sec$/) r=i;
                if(r<2) next;
                win="-"; for(i=1;i<=NF;i++) if($i ~ /^[0-9]+\.[0-9]+-[0-9]+\.[0-9]+$/) { win=$i; break }
                tail=""; for(i=r+1;i<=NF;i++) tail=tail" "$i;
                printf "  %-8s 窗口 %-12s 传输 %-12s 速率 %-14s%s\n",
                       (/sender/?"发送":"实收"), win, $(r-3)" "$(r-2), $(r-1)" "$r, tail
              }'
}

# 在负载中途抓一次 show runtime：Packet-Clocks 列就是该窗口内的 周期/包
sample_runtime() {
    for n in ${NODES}; do
        printf '  -- %s\n' "$n"
        docker exec "$n" bash -c "vppctl show runtime" 2>/dev/null |
            awk -v k="${WATCH}" 'BEGIN{n=split(k,a," ");for(i=1;i<=n;i++)w[a[i]]=1}
                                 $1 in w && $2=="active" && $3>500 {
                                     printf "     %-16s 包/窗口 %-10s 周期/包 %s\n", $1, $3, $6 }'
    done
}
sr_counters() {
    for n in ${NODES}; do
        printf '  -- %s : %s\n' "$n" \
            "$(docker exec "$n" bash -c "vppctl show sr-wg stats" 2>/dev/null |
               awk '/Encap:|Transit:|Decap:/{sec=$1} /ok:|bypass:|hmac_fail:|no_headroom:/{gsub(/:/,"",$1); if($2+0>0) printf "%s%s=%s ", sec, $1, $2}')"
    done
}

if [ "${UDP_ONLY}" != "1" ]; then
    # TCP 在这台测试台上抖动极大（实测单流 76~174 Mbit/s，4 流 169~186），
    # 单采样没有意义，所以每种跑 TCP_ROUNDS 轮，报的是区间而不是点。
    TCP_ROUNDS=${TCP_ROUNDS:-2}
    for p in "" "-P 4"; do
        for i in $(seq 1 ${TCP_ROUNDS}); do
            say "TCP ${p:-单流} 第 ${i}/${TCP_ROUNDS} 轮 ${DURATION}s（path MTU 由 perf-rig.sh 设定）"
            iperf_run "${p}"
        done
    done
fi

if [ "${TCP_ONLY}" = "1" ]; then
    say "CPE 线程与限流（VPP 只有 vpp_main 时，带宽是单核数）"
    docker exec "${CPE_A}" bash -c 'vppctl show threads | sed -n "1,6p"; echo "cpu.stat:"; grep -E "nr_throttled|throttled_usec" /sys/fs/cgroup/cpu.stat'
    exit 0
fi

say "UDP 阶梯：载荷 1200B，找无丢包上限与饱和点"
for r in 100M 150M 200M 250M 400M; do
    printf '  @%s\n' "$r"
    iperf_run "-u -b $r -l 1200"
done

say "UDP 阶梯：载荷 200B（看 pps 上限而不是 bps）"
for r in 50M 100M 200M; do
    printf '  @%s\n' "$r"
    iperf_run "-u -b $r -l 200"
done

say "满载中途采样 周期/包（起 40s 洪泛，第 25s 取样）"
docker exec "${TERM_A}" bash -c "timeout 90 iperf3 -c ${TERM_B_IP} -u -b 500M -l 1200 -t 40 -P 4 >/tmp/bench-load.log 2>&1" &
LOADPID=$!
sleep 25
sample_runtime
sr_counters
wait ${LOADPID}
say "洪泛 4 流结果"
docker exec "${TERM_A}" bash -c "grep -E 'SUM' /tmp/bench-load.log | tail -2"
say "CPE 线程与限流（VPP 只有 vpp_main 时，带宽是单核数）"
docker exec "${CPE_A}" bash -c 'vppctl show threads | sed -n "1,6p"; echo "cpu.stat:"; grep -E "nr_throttled|throttled_usec" /sys/fs/cgroup/cpu.stat'
