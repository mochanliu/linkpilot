#!/bin/bash
# perf-bench.sh - 在 perf-rig.sh 搭好的「终端-CPE-中转-CPE-终端」台子上测隧道带宽
#
# 用法:
#   test/scripts/perf-bench.sh                 # 标准一轮（TCP + UDP 阶梯 + 周期采样）
#   test/scripts/perf-bench.sh --udp-only      # 只跑 UDP 阶梯
#   TEST_DURATION=8 bash test/scripts/perf-bench.sh
#
# 输出两类数：
#   1) 端到端带宽/pps（受 Docker af-packet + 单 VPP 线程限制，只能当下限看）
#   2) 每个数据面节点的 周期/包（show runtime 的 Packet-Clocks 是采样窗口平均值，
#      所以必须在流量跑起来的中途取样，跑完再取就归零了）
#
# 判定口径：性能归因看 2)，绝对带宽看 1) 但必须标注测试台限制。
set -u

TERM_A=${TERM_A:-srwg-term-a}
TERM_B_IP=${TERM_B_IP:-10.20.2.3}
CPE_A=${CPE_A:-srwg-node1}
NODES=${NODES:-"srwg-node1 srwg-transit srwg-node2"}
WATCH=${WATCH:-"ip4-lookup ip4-midchain sr-wg4-encap sr-wg4-transit sr-wg4-input wg4-input wg4-output-tun host-eth0-tx"}
DURATION=${TEST_DURATION:-8}
UDP_ONLY=0
for a in "$@"; do [ "$a" = "--udp-only" ] && UDP_ONLY=1; done

say() { printf '\n== %s\n' "$*"; }

iperf_run() { # $* = iperf3 参数
    docker exec "${TERM_A}" bash -c "timeout $((DURATION + 45)) iperf3 -c ${TERM_B_IP} -t ${DURATION} $* 2>&1" |
        awk '/SUM|^\[  5\]/ && /sender|receiver/ {
                bit=""; for(i=1;i<=NF;i++) if($i ~ /bits\/sec/) bit=$i" "$(i+1);
                printf "  %-8s %-9s %s %s\n", (/sender/?"发送":"实收"), $5, bit, $NF
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
    say "TCP 单流 ${DURATION}s（path MTU 由 perf-rig.sh 设定）"
    iperf_run ""
    say "TCP 4 流 ${DURATION}s"
    iperf_run "-P 4"
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
