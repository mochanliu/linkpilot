#!/bin/bash
# exp-295 换件第一步（沿用 #291 的结论）：大产物分块并行推到盒子的
# artifacts 目录，落地后拼回、核对整包 sha256。
#
# 为什么分块：Mac↔盒子 ~13KB/s 且单连接 scp 会被中断截断（#291 §5 两次全栽），
# 分块并行 + sha 核验一次过。每块独立传输，哪块断了重推哪块。
#
# 用法：bash push-par-295.sh <BOX> <name> <src.gz>
#   name = sr-agent（产物名与 /usr/local/bin 下同名）
set -eu
BOX=${1:?CPE1..CPE4}
NAME=${2:?sr-agent}
SRC=${3:?gzip 好的 linux/amd64 产物}
D=/root/vmlab/artifacts/linux
CHUNK=524288          # 512KB
WORK=/tmp/push-295
# EXPECT 是解压后的整包 sha（盒子侧对 gunzip 的产物核对，不对 .gz 本身）。
EXPECT=$(gunzip -c "$SRC" | shasum -a 256 | awk '{print $1}')
T0=$(date +%s)

mkdir -p "$WORK/$BOX"
split -b $CHUNK "$SRC" "$WORK/$BOX/x_" 2>/dev/null || split -b $CHUNK "$SRC" "$WORK/$BOX/x_"

# 并行推每一块（每块一次独立 scp），失败的块收集起来重推一轮。
push_one() {
    local f=$1
    for try in 1 2 3; do
        if scp -q -o BatchMode=yes "$f" "$BOX:$D/$(basename "$f").part" 2>/dev/null; then
            return 0
        fi
        sleep 1
    done
    return 1
}
FAIL=()
for f in "$WORK/$BOX"/x_*; do
    push_one "$f" || FAIL+=("$f") &
done
wait
for f in "${FAIL[@]:-}"; do
    [ -z "$f" ] && continue
    echo "重推 $f"
    push_one "$f" || { echo "!! $f 三轮都失败" >&2; exit 1; }
done

# 盒子侧拼回 + 核对 sha + gz 校验。
ssh -o BatchMode=yes "$BOX" bash -s -- "$NAME" "$D" "$EXPECT" <<'REMOTE'
set -eu
NAME=$1; D=$2; EXPECT=$3
cd "$D"
cat x_*.part > "$NAME.gz"
rm -f x_*.part
gunzip -c "$NAME.gz" > "$NAME.new"
rm -f "$NAME.gz"
GOT=$(sha256sum "$NAME.new" | awk '{print $1}')
if [ "$GOT" != "$EXPECT" ]; then
    echo "!! sha 不符：got=$GOT expect=$EXPECT" >&2
    exit 1
fi
printf '%s %s 落地（sha 前 16 位吻合）\n' "${GOT:0:16}" "$NAME"
REMOTE
echo "$BOX 全程 $(( $(date +%s) - T0 ))s"
