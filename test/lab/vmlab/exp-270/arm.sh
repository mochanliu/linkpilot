#!/bin/bash
# 问题6（#270）摆臂：只改**运行时**的 VPP 主线程落核（可选再改 af-packet 的 rx-mode），
# 不重启任何东西、不写任何配置 —— 这是 1 号硬规则要求的形状，也是这次对照能连续换臂的原因。
# 每次摆完都把生效值读回来打印，不靠"我写了所以生效了"（exp-267 第一批六块就是栽在从当前掩码
# 反推参考核，摆臂失败还在照样取样）。
#
# 用法（在 Mac 上跑）：
#   CORE=0 bash arm.sh CPE1 CPE2 CPE3 CPE4           # 只挪核
#   CORE=1 RX=polling bash arm.sh CPE1 CPE4          # 同时把两口 rx-mode 设成 polling
#   RX=interrupt bash arm.sh CPE4                    # 只改 rx-mode，核不动
set -uo pipefail
CORE=${CORE:-}
RX=${RX:-}

for box in "$@"; do
  ssh "$box" "bash -s" <<EOS 2>/dev/null
pid=\$(pgrep -x vpp_main)
[ -n "$CORE" ] && taskset -pc $CORE \$pid >/dev/null 2>&1
if [ -n "$RX" ]; then
  for i in host-enp0s8 host-enp0s9; do vppctl "set interface rx-mode \$i $RX" >/dev/null 2>&1 || echo "  rx-mode \$i 设置失败"; done
fi
printf "%s pid=%s mask=%s rx=%s irq17=%s\n" "$box" "\$pid" \\
  "\$(taskset -cp \$pid | cut -d: -f2 | tr -d ' ')" \\
  "\$(vppctl 'show interface rx-placement' 2>/dev/null | tr -d '\r' | awk '/host-enp/{printf \"%s \", \$NF}')" \\
  "\$(cat /proc/irq/17/smp_affinity_list 2>/dev/null)"
EOS
done
