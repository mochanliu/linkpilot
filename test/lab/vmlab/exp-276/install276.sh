#!/bin/bash
# #276 换件第二步：把 push276.sh 推到 artifacts 里的产物**装盘**（原子改名），不换进程。
#
# 装完故意让「跑的那份」和「盘上那份」不一致，并且把这两行都打出来 ——
# 不一致才是对的：这一枪只改磁盘事实，客户流量还在老进程上跑（§1.1 不许重启兜底）。
# 真正换进程是下一步 swap276.sh，两步分开是因为「推完直接换」会让开机清单
# 从旧盘那份把人抬起来（agent-handover.md §5.2 第②步，2026-11-07 真犯过一次）。
#
# 用法：bash install276.sh <BOX> <name> [盘上的源文件]
#   name = sr-route | sr-fib | sr-frr | sr-agent（四个都在开机清单里）
#   源文件默认 $D/<name>.new，也就是 push276.sh 的落点。
set -eu
BOX=${1:?CPE1|CPE2|CPE3|CPE4}
NAME=${2:?sr-route|sr-fib|sr-frr|sr-agent}
D=/root/vmlab/artifacts/linux
SRC=${3:-$D/$NAME.new}

ssh -o BatchMode=yes "$BOX" bash -s -- "$NAME" "$SRC" "$BOX" <<'REMOTE'
set -eu
NAME=$1; SRC=$2; BX=$3
BIN=/usr/local/bin/$NAME
# 只看存在与非空：push276.sh 的 gunzip 落点本来就是 0644，可执行位由下面 chmod 给。
[ -s "$SRC" ] || { echo "盘上没有产物或它是空的：$SRC（先跑 push276.sh）" >&2; exit 1; }
NEW=$(sha256sum "$SRC" | cut -c1-16)
NEWB=$(stat -c%s "$SRC")
OLD=$(sha256sum "$BIN" 2>/dev/null | cut -c1-16 || echo 无)
PID=$(pgrep -x "$NAME" | head -1 || true)
RUN=$( [ -n "$PID" ] && sha256sum /proc/$PID/exe 2>/dev/null | cut -c1-16 || echo 没跑 )
# 同一文件系统内的 rename(2)：老进程抱着老 inode 继续跑，新进程一起来就是新的。
cp -a "$SRC" "$BIN.installing" && chmod 0755 "$BIN.installing" && mv -f "$BIN.installing" "$BIN"
printf '   %s 装盘完成\n' "$NAME"
printf '   新   盘上=%s（%s 字节）\n   老   盘上=%s\n   跑着的=%s (pid=%s)\n' "$NEW" "$NEWB" "$OLD" "${RUN:-没跑}" "${PID:-—}"
if [ "$NEW" = "$RUN" ]; then
  echo "   !! 跑着的就是这一份 —— 装盘这枪没有改变任何运行事实，不需要 swap"
else
  echo "   跑 ≠ 盘：这是预期状态。要生效就接着跑 swap276.sh $BX $NAME"
fi
REMOTE
