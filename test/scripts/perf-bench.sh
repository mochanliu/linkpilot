#!/bin/bash
# perf-bench.sh - 在 perf-rig.sh 搭好的「终端-CPE-中转-CPE-终端」台子上测隧道带宽
#
# 用法:
#   test/scripts/perf-bench.sh                 # 标准一轮（TCP + UDP 阶梯 + 周期采样）
#   test/scripts/perf-bench.sh --udp-only      # 只跑 UDP 阶梯
#   test/scripts/perf-bench.sh --tcp-only      # 只跑 TCP（抖动观测）
#   test/scripts/perf-bench.sh --no-control    # 跳过控制组（默认会先跑）
#   TEST_DURATION=8 TCP_ROUNDS=3 bash test/scripts/perf-bench.sh
#
# 每轮顺序：控制组（不经 VPP，证明终端不是瓶颈）→ TCP → UDP 阶梯 → 周期/包取样 →
#           逐跳归因（attr-cpu.sh）。控制组丢了就先别看隧道数。
#
# 输出两类数：
#   1) 端到端带宽/pps（受 Docker af-packet + 单 VPP 线程限制，只能当下限看）
#   2) 每个数据面节点的 周期/包（show runtime 的 Packet-Clocks 是采样窗口平均值，
#      所以必须在流量跑起来的中途取样，跑完再取就归零了）
#
# 判定口径：先看控制组确认终端不是瓶颈，再看端到端，最后用 attr-cpu.sh 的逐跳差分定位。
#
# 2026-10-18 基线（CPE 锁 2 vCPU / 2 GB，VPP 单线程 vpp_main，path MTU 1360）：
#   控制组    同终端同 iperf3 但不经 VPP（srwg_direct 直连桥）：
#             UDP 400M/1200B 实收 399 Mbit/s、丢 0.011%（41.6k pps 全过）
#             UDP 800M/1200B 双流 861 Mbit/s、丢 0.015%（71k pps）
#             UDP 1.6G/1200B 四流 958 Mbit/s 发送 / 954 实收、丢 0.023%（84k pps）
#             TCP 单流 15.7 Gbit/s
#             终端配额 cpu.max=memory.max=不限，洪泛时 term-a 只占 0.80 核、term-b 0.08
#           => 终端和 iperf3 相对隧道数字有 4~10 倍余量，端到端上限不是它们造成的。
#   端到端  TCP 单流 169~175 Mbit/s；TCP 4 流 190~200 Mbit/s
#           UDP 1200B：100/150/200 Mbit/s 全 0 丢包，250M 起丢 7.7%，400M 丢 41%，
#                       即饱和点 ≈233 Mbit/s（≈24 kpps）
#           UDP 200B： 上限 ≈43 Mbit/s（≈27 kpps），50M 就已经丢 7.4%
#           4 流洪泛： 灌 1.99 Gbit/s 只过 196 Mbit/s（丢 90%）
#   周期/包（负载中途取样，注意这是 TSC 墙钟不是 CPU 功）：
#           sr-wg4-encap 623 / sr-wg4-transit 250 / sr-wg4-input(decap) 1150
#           wg4-output-tun 1.95e4 / wg4-input 1.86e4 / host-*-tx 6.6e4~7.8e4
#   结论（2026-10-18 用 attr-cpu.sh 的 cgroup 差分修正，早先那版把 af-packet TX 的
#         78k 周期/包当成天花板是错的——Packet-Clocks 把在 socket 上阻塞等待的墙钟
#         也算进去了）：
#           SR 三段合计 ≈2.0k 周期/包可信，这三个节点不阻塞。
#           出口 CPE 实测 0.28 核 @ 29.3k pps = 3.0e4 周期/包，比单给 host-eth0-tx
#           的 7.8e4 还小，所以 Packet-Clocks 的数不能当成本引用。
#           这台台子的上限形态是「每个 CPE 大约只进得来 3 万 pps」：灌 4 流 1.6G 时
#           term-a 网卡真的发出 16.3 万 pps，node1 内核在 eth2 上也确实收到 16.3 万
#           （sysfs rx_packets 差分，rx_dropped=0），但 VPP 的 host-eth2 rx 只计到
#           2.81 万——即 83% 的量卡在「内核交给 af-packet 环 / VPP 取包」这一步，
#           中转->node2 一跳同理（内核 2.87 万 vs VPP 2.00 万），而两台 CPE 都只用
#           0.27~0.28/2.0 核、nr_throttled=0。所以：
#             * 端到端丢包不是终端、不是 iperf3、不是 CPE 的 CPU 配额造成的；
#             * 是 VPP 单线程 vpp_main + af-packet 取包节奏在 Docker 上的上限
#               （show runtime 实测 vectors/call≈42，远小于 256 的每轮预算，
#                主循环只有 ~2.4k loops/s，被 host-*-tx 的阻塞 sendto 拖住）；
#             * CPE 之间那段隧道本身零丢包：中转 rx == Transit ok == tx，误差 0.0%。
#           所以 208 Mbit/s 只能当**下限**：按实测 3.1e4 周期/包，2 核配额的理论上限
#           约 2x3.2e9/3.1e4 ≈ 2.0e5 pps ≈ 2 Gbit/s@1200B，是实测的 10 倍。
#           attr-cpu.sh 的「内核侧」两行就是专门用来钉死这件事的。
#
# 2026-10-18 追加：到底是 WG 本身的开销，还是我们加的 SR 代码？（同容器同配额，
#   固定无损负载 200M/1200B ≈20.8k pps，各 2 轮，attr-cpu.sh 的 COST 行；口径是
#   占核差分 ÷ VPP 实际收到的 pps，对拓扑不敏感）
#     node1(出口)  纯转发 3.19e4 → +WG 3.63e4 → +SR+WG 3.99e4 周期/包
#     node2(入口)  纯转发 3.83e4 → +WG 4.23e4 → +SR+WG 4.21e4 周期/包
#     中转         普通 UDP 转发 2.52e4 → +SR transit 2.54e4 周期/包
#   拆出来的边际成本：原版 WireGuard ≈ +0.4e4~0.44e4（+12~14%），我们的 SR 头
#   ≈ +0.36e4（出口 +10%）、入口与中转 ≈ 0（噪声内，中转与 show runtime 的 250
#   周期/包一致）。而**什么都不加的纯 VPP 转发就已经要 3.2e4 周期/包**，占整条
#   出口的 80% —— 那部分是 af-packet + ip4/arp/midchain 的台子开销，不是加密、
#   也不是我们的代码。摘掉 SR 绑定跑纯 WG 的洪泛对照（4x400M）：node1 进 VPP
#   34.2k pps vs 带 SR 的 28.3k，端到端 190 vs 196 Mbit/s（SR 包更大所以比特持平），
#   即原版 WG 在这台台子上也快不了多少，同一个上限形态。
#   复现：`vppctl set sr-wg tunnel 2 del` → 测 → `set sr-wg tunnel 2 path 1 endpoint
#   10.10.2.2` 绑回；无 WG 那一档要把 node1/node2 的 10.20.x/24 路由先 del 再指到
#   underlay 下一跳（VPP 的 ip route add 是**追加路径**，不 del 会变成 ECMP 分半）。
set -u

TERM_A=${TERM_A:-srwg-term-a}
TERM_B_IP=${TERM_B_IP:-10.20.2.3}
DIRECT_IP=${DIRECT_IP:-10.30.0.2}      # term-b 在 srwg_direct 直连桥上的地址，不经 VPP
CPE_A=${CPE_A:-srwg-node1}
NODES=${NODES:-"srwg-node1 srwg-transit srwg-node2"}
WATCH=${WATCH:-"ip4-lookup ip4-midchain sr-wg4-encap sr-wg4-transit sr-wg4-input wg4-input wg4-output-tun host-eth0-tx"}
DURATION=${TEST_DURATION:-8}
UDP_ONLY=0; TCP_ONLY=0; NO_CTRL=0
for a in "$@"; do
    [ "$a" = "--udp-only" ] && UDP_ONLY=1
    [ "$a" = "--tcp-only" ] && TCP_ONLY=1
    [ "$a" = "--no-control" ] && NO_CTRL=1
done

say() { printf '\n== %s\n' "$*"; }

iperf_run() { # $* = iperf3 参数；目标取 ${DST:-TERM_B_IP}
    # iperf3 的列是 "<数值> Mbits/sec"，速率在单位的前一列；尾部（重传/丢包率）
    # 列数不固定，所以取速率之后的所有字段。
    # 坑：TCP 的 receiver 行窗口会拖过 -t 设定的秒数（实测 5s 的测试接收侧报
    # 0.00-15.11，于是"实收"被摊薄成 52 Mbit/s 而发送侧 158）。所以 TCP 只看
    # 发送行，同时把窗口秒数一起打印出来，别拿摊薄值当结论。
    docker exec "${TERM_A}" bash -c "timeout $((DURATION + 45)) iperf3 -c ${DST:-${TERM_B_IP}} -t ${DURATION} $* 2>&1" |
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

# 控制组：先证明「终端 + iperf3」不是瓶颈，否则后面所有隧道数字都不能归因到 CPE。
# 走 srwg_direct 直连桥（term-a 10.30.0.11 <-> term-b 10.30.0.2），路径上没有 VPP。
# 建桥见 perf-rig.sh；--no-control 可跳过。
if [ "${NO_CTRL}" != "1" ]; then
    docker exec srwg-term-b iperf3 -s -p 5201 --daemon >/dev/null 2>&1 || true
    sleep 1   # --daemon 是 fork 后才 bind，立刻发起客户端会 Connection refused
    say "控制组：同终端、同载荷、不经 VPP（终端/iperf3 的余量在这里）"
    printf '  UDP 400M/1200B\n'
    DST=${DIRECT_IP} iperf_run "-u -b 400M -l 1200"
    printf '  UDP 800M/1200B 双流\n'
    DST=${DIRECT_IP} iperf_run "-u -b 800M -l 1200 -P 2"
    printf '  UDP 1.6G/1200B 四流（隧道实测才 208 Mbit/s，这一行就是余量）\n'
    DST=${DIRECT_IP} iperf_run "-u -b 400M -l 1200 -P 4"
    printf '  终端配额（应当是不限——若端到端受限，先怀疑台子再怀疑终端）\n'
    for c in "${TERM_A}" srwg-term-b; do
        printf '  %-13s cpu.max=%s memory.max=%s\n' "$c" \
            "$(docker exec "$c" sh -c 'tr "\n" " " < /sys/fs/cgroup/cpu.max')" \
            "$(docker exec "$c" cat /sys/fs/cgroup/memory.max)"
    done
fi

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
# 与控制组最后一行同口径（4x400M）：控制组能过 84k pps，隧道口只过 ~29k pps，
# 差出来的就是 af-packet 取包的量，不是终端的锅。
printf '  四流 4x400M 洪泛（与控制组最后一行同一 offer，用来暴露隧道口上限）\n'
iperf_run "-u -b 400M -l 1200 -P 4"

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

# 逐跳归因：端到端只告诉丢了多少，attr-cpu.sh 用 cgroup 占核 + sr-wg 计数器差分
# 说清丢在哪一跳、CPE 到底用了几个核。它自己起流自己掐表，所以放在最后单独跑。
say "逐跳归因（口径与坑见 attr-cpu.sh 文件头）"
WIN=${ATT_WIN:-20} RATE=${ATT_RATE:-400M} LEN=${ATT_LEN:-1200} \
    bash "$(dirname "$0")/attr-cpu.sh"
