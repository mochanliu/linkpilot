#!/bin/bash
# #276 换件第一步：把 gzip 好的产物推到盒子的 artifacts 目录，落地后核对哈希。
# **只落盘，不 install、不换进程** —— 那两步由 install276.sh / swap276.sh 分开做，
# 因为「推完直接换进程」会重启起旧盘上那一份（§5.2 第②步，2026-11-07 真犯过一次）。
#
# 用法：bash push276.sh <BOX> <name> <src.gz>
#   name = sr-route | sr-fib，产物名与 /usr/local/bin 下同名。
# 串行、不并发：Mac↔盒子这条链路只有 12KB/s 量级（2026-11-08 实测 512KB/39.6s），
# 而并发 scp 会静默截断（§7.3）。
set -eu
BOX=${1:?CPE1}
NAME=${2:?sr-route|sr-fib}
SRC=${3:?gzip 好的 linux/amd64 产物}
D=/root/vmlab/artifacts/linux
T0=$(date +%s)
scp -q -o BatchMode=yes "$SRC" "$BOX:$D/$NAME.gz"
ssh -n -o BatchMode=yes "$BOX" "cd $D && gunzip -c $NAME.gz > $NAME.new && rm -f $NAME.gz && \
  printf '%s %s 落地 %ss\n' \"\$(sha256sum $NAME.new | cut -c1-16)\" $NAME \"$(( $(date +%s) - T0 ))\""
ssh -n -o BatchMode=yes "$BOX" "rm -f $D/probe.bin"
