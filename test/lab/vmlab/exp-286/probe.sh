#!/bin/bash
# probe.sh —— 在被测盒本地跑的采样器（exp-286 全方位复测）
#
# 用法: probe.sh <PFX> <PEERS> <IF> <DUR_SEC> <OUT>
#   PFX    单前缀查询，例如 172.16.12.1/32
#   PEERS  BFD 对端 overlay 地址，逗号分隔（ECMP 时两条都要看）
#   IF     watch 的隧道口，例如 wg1009
#   DUR    采样时长（秒）
#   OUT    输出 CSV
#
# 每轮采三样，全部落在**本盒自己的时钟**上（盒间不对时，见 handover §7.1）：
#   1) 该前缀的 FIB 行：path 条数 / 各条 pref / resolved 条数 / buckets / 承载下一跳
#   2) 每个对端 BFD 会话状态
#   3) watch 的那个接口 admin 状态
#
# 输出一行：
#   ts_ms|len|prefs|resolved|buckets|carry|bfd:peer=state,...|if=state
#
# 注意（每一条都踩过）：
#   - vppctl 非 TTY 每行结尾带 CR，一律 tr -d '\r'
#   - 单前缀查询才有 path-list/path:/pref/oper-flags；整表 dump 没有
#   - vppctl 可能挂着不回话，一律 timeout
#   - 解析全部压进单次 awk：每多一个 sed/grep/cut 进程，分辨率就掉几十毫秒
set -u

PFX=${1:?pfx}
PEERS=${2:?peers}
IF=${3:?if}
DUR=${4:?dur}
OUT=${5:?out}

end=$(( $(date +%s%3N) + DUR * 1000 ))
: > "$OUT"

while :; do
    now=$(date +%s%3N)
    [ "$now" -ge "$end" ] && break

    fib=$(timeout 5 vppctl "show ip fib $PFX table 0" 2>/dev/null | tr -d '\r' |
        awk '
            /path-list:\[/  { for (i=1;i<=NF;i++) if ($i ~ /^len:/)    { split($i,a,":"); len=a[2] } }
            /^ *path:\[/    { for (i=1;i<=NF;i++) {
                                  if ($i ~ /^pref=/)          { split($i,a,"="); prefs = prefs (prefs?",":"") a[2] }
                                  if ($i ~ /^oper-flags:/)     { if ($i ~ /resolved/) res++ }
                              } }
            /buckets:/      { for (i=1;i<=NF;i++) if ($i ~ /^buckets:/) { split($i,a,":"); b=a[2] } }
            /mpls via/      { carry = carry (carry?",":"") $4 }
            END { printf "%s|%s|%d|%s|%s", (len==""?"?":len), (prefs==""?"?":prefs), res+0, (b==""?"?":b), (carry==""?"none":carry) }
        ')

    bfdout=$(timeout 5 vppctl "show bfd sessions" 2>/dev/null | tr -d '\r')
    bfd=$(printf '%s\n' "$bfdout" | awk -v want="$PEERS" '
        BEGIN { n=split(want,p,","); for (i=1;i<=n;i++) need[p[i]]=1 }
        /IPv4 address/ { f = ($5 in need); peer = $5 }
        f && /Session state/ { out = out (out?",":"") peer "=" $3; f=0; done++ }
        END { printf "%s", out }')

    ist=$(timeout 5 vppctl "show interface $IF" 2>/dev/null | tr -d '\r' |
        awk -v i="$IF" '$1 == i { print $3; exit }')

    echo "${now}|${fib}|${bfd}|if=${ist:-?}" >> "$OUT"

    # 目标 ~100ms 一轮
    sleep 0.04
done
