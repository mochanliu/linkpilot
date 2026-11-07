#!/usr/bin/env bash
# p277：把「正在跑的那一份」补装到盘上，消除 run≠disk 的静默回退。
#
# 为什么不打断客户流量：
#   * 全程不 kill、不 restart、不动 watch timer —— 只换 /usr/local/bin 里的文件。
#   * 二进制不走网络：在盒内直接复制 /proc/<pid>/exe（内核打开的那个 inode），
#     复制出来的字节按定义就是在跑的那一份；再用期望哈希当闸门，不符就不装。
#     （Mac↔盒子这条链路实测只有 7KB/s 量级，18MB 传输要两小时，且传输不是必要的。）
#   * 换法是同目录临时件 + mv（rename），不是覆写目标名：覆写正在执行的二进制吃 ETXTBSY；
#     VPP 那边插件是 mmap（maps 里已是 (deleted) inode），必须靠 rename 让在跑的继续持有旧 inode。
#   * sr-box-watch 每 60s 跑一轮 start.sh up：清单里没有任何二进制安装动作（grep 过 install/cp/mv），
#     幂等前提「在跑的进程绝不重启」实测过，所以它既不会覆盖我装的，也不会因为换件重启谁。
#   * 插件盘上那份只有 247KB，走盒子主动从 Mac HTTP 拉（192.168.1.4:8799），拉完校哈希才装。
set -euo pipefail

AGENT=d2ad8519648c031f   # = Mac artifacts/linux/sr-agent（四台在跑的那代，含缺陷④ + 遥测）
FIB=bab523ff3809f57b     # = Mac artifacts/linux/sr-fib.20261106-bab523ff（缺陷⑦ 修复件，四台在跑）
PLUG=9267d28d8b76fb54    # = Mac artifacts/plugins/sr_wg_plugin.so（CPE1 盘上本来就是它）
MACURL=http://192.168.1.4:8799

for box in "$@"; do
  echo "=== $box"
  ssh "$box" "set -e
    cd /root/vmlab/artifacts/linux

    # 1) 插件：盘上不是 9267d28d 才去拉（247KB，一次 HTTP）
    if [ \"\$(sha256sum /usr/lib/x86_64-linux-gnu/vpp_plugins/sr_wg_plugin.so | cut -c1-16)\" != $PLUG ]; then
      curl -sf --max-time 300 -o sr_wg_plugin.new $MACURL/a-sr_wg_plugin.so
      p=\$(sha256sum sr_wg_plugin.new | cut -c1-16)
      [ \"\$p\" = $PLUG ] || { echo \"  中断：插件收到的是 \$p\"; exit 1; }
      cp -a /usr/lib/x86_64-linux-gnu/vpp_plugins/sr_wg_plugin.so /usr/lib/x86_64-linux-gnu/vpp_plugins/sr_wg_plugin.so.prerun-20261107
      install -m 0755 sr_wg_plugin.new /usr/lib/x86_64-linux-gnu/vpp_plugins/sr_wg_plugin.so.swap
      mv -f /usr/lib/x86_64-linux-gnu/vpp_plugins/sr_wg_plugin.so.swap /usr/lib/x86_64-linux-gnu/vpp_plugins/sr_wg_plugin.so
      echo '  插件：盘上那份已对齐（VPP 不重启，本次不生效，只是下次重启 VPP 的落点）'
    else
      echo '  插件：盘上本来就是 $PLUG，不动'
    fi

    # 2) 两个 Go 进程：从 /proc/<pid>/exe 盒内复制，期望哈希不符就整台中断
    for x in sr-agent sr-fib; do
      case \$x in sr-agent) want=$AGENT ;; sr-fib) want=$FIB ;; esac
      pid=\$(pgrep -x \$x | head -1)
      [ -n \"\$pid\" ] || { echo \"  中断：\$x 没在跑\"; exit 1; }
      run=\$(sha256sum /proc/\$pid/exe | cut -c1-16)
      [ \"\$run\" = \"\$want\" ] || { echo \"  中断：\$x 在跑的是 \$run，不是期望的 \$want\"; exit 1; }
      cur=\$(sha256sum /usr/local/bin/\$x | cut -c1-16)
      if [ \"\$cur\" = \"\$want\" ]; then echo \"  \$x：盘上已是 \$want，不动\"; continue; fi
      cp /proc/\$pid/exe /usr/local/bin/\$x.swap
      got=\$(sha256sum /usr/local/bin/\$x.swap | cut -c1-16)
      [ \"\$got\" = \"\$want\" ] || { rm -f /usr/local/bin/\$x.swap; echo \"  中断：\$x 复制出来是 \$got\"; exit 1; }
      chmod 0755 /usr/local/bin/\$x.swap
      cp -a /usr/local/bin/\$x /usr/local/bin/\$x.prerun-20261107
      mv -f /usr/local/bin/\$x.swap /usr/local/bin/\$x
      echo \"  \$x：\$cur → \$want（旧底 /usr/local/bin/\$x.prerun-20261107）\"
    done"

  echo "--- 复核：run vs disk，以及没被打扰的进程"
  ssh "$box" 'for x in sr-route sr-frr sr-fib sr-agent; do
      pid=$(pgrep -x $x | head -1)
      r=$(sha256sum /proc/$pid/exe | cut -c1-16); d=$(sha256sum /usr/local/bin/$x | cut -c1-16)
      printf "  %-9s pid=%-7s run=%s disk=%s %s\n" "$x" "$pid" "$r" "$d" \
        "$([ "$r" = "$d" ] && echo 一致 || echo 仍不一致)"
    done
    printf "  %-9s disk=%s\n" plugin "$(sha256sum /usr/lib/x86_64-linux-gnu/vpp_plugins/sr_wg_plugin.so | cut -c1-16)"
    printf "  vpp_main=%s zebra=%s bgpd=%s wg口=%s条\n" "$(pgrep vpp_main | head -1)" "$(pgrep -x zebra | head -1)" \
      "$(pgrep -x bgpd | head -1)" "$(vppctl show interface 2>/dev/null | grep -cE "^wg")"'
done
