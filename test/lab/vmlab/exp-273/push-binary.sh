#!/bin/bash
# 换件（不重启任何东西）：gzip 推到盒子 -> sha256 核对 -> 落 $WORK/../artifacts -> install -m 0755 到 /usr/local/bin/sr-frr。
# 用法：WORK=/tmp/p273 BIN=/path/to/sr-frr.gz SRC_SHA=9f28db6c268aa12c bash push-binary.sh [CPE1 CPE2 ...]
# 推完之后，运行中的 sr-frr 还是旧码；要让它生效就是 window-swap.sh 那一枪（SIGTERM + watch 补进程），
# 已经"锁死"的盒子换新二进制不会自己解开 —— 见 docs/vmlab-rebuild-plan.md §9.19 第九节第 1 条、§9.20 第五节。
WORK=${WORK:-/tmp/p273}
BIN=${BIN:?要 gzip 好的 linux/amd64 sr-frr}
BOXES=${*:-CPE1 CPE2 CPE3 CPE4}
mkdir -p $WORK/push
for B in $BOXES; do
  echo "== $B"
  scp -q -o BatchMode=yes $BIN $B:/root/vmlab/artifacts/linux/sr-frr.gz && \
  ssh -n -o BatchMode=yes $B 'cd /root/vmlab/artifacts/linux && gunzip -c sr-frr.gz > sr-frr.new \
    && sha256sum sr-frr.new | cut -c1-16 && install -m 0755 sr-frr.new sr-frr && rm -f sr-frr.new sr-frr.gz'
  echo "   rc=$?"
done
