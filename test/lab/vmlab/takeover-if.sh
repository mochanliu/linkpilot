#!/bin/bash
# 把一个物理口接管进 VPP：四步增量 CLI，出现配对的 TAP 才算成。
# 由 underlay.sh 在 vpp.service 起来之后调用；不放在 VPP 的 unix{exec} 里 ——
# 2026-11-04 实测：放在 exec 里时一次 systemctl restart vpp 就稳定丢掉第一条接管的创建，
# 掉的那个口没人再管（客户侧断到 374.9 秒）。见 docs/vmlab-rebuild-plan.md §5.6(3)。
#
# 用法：takeover-if.sh <物理口> <TAP 名> <该口 netdev 的真实 MAC> [重试拍数] [每拍间隔秒]
# MAC 只许从 VBoxManage showvminfo / 真机烧录值传进来，不许从活口读：
# 上一次接管失败时活口 MAC 可能已被反向写脏（docs §5.4 的 S3-h/S3-j）。
set -u
IF=${1:?物理口}
TAP=${2:?TAP 名}
MAC=${3:?netdev 真实 MAC}
LIMIT=${4:-40}
STEP=${5:-0.5}

vc() { vppctl "$@" 2>&1 | tr -d '\r'; }
paired() { vc show lcp | grep -F "host-$IF " | grep -qF "$TAP"; }
cli_up() { vc show version >/dev/null 2>&1; }
netdev_up() { [ $(( $(cat "/sys/class/net/$IF/flags" 2>/dev/null || echo 0) & 1 )) -eq 1 ]; }

last=''
for shot in $(seq 1 "$LIMIT"); do
    if paired; then
        echo "OK($shot) $IF 已在 VPP 里，TAP=$TAP"
        exit 0
    fi
    if [ ! -e /run/vpp/cli.sock ]; then
        last="等 /run/vpp/cli.sock"
        sleep "$STEP"; continue
    fi
    if ! cli_up; then
        last="vppctl 连不上（VPP 主线程还没就绪）"
        sleep "$STEP"; continue
    fi
    live=$(cat "/sys/class/net/$IF/address" 2>/dev/null || echo 读不到)
    if [ "$live" != "$MAC" ]; then
        # 不猜、不就地改 MAC：带着不等的 MAC 建口就是自己造三处不一致（对端 ARP FAILED → 100% 丢包）
        echo "FAIL $IF 的 netdev MAC=$live 与传入的 $MAC 不等；先放开 VPP 再把 netdev 修回该值"
        exit 1
    fi
    netdev_up || ip link set "$IF" up 2>/dev/null
    out=$(vc create host-interface name "$IF" hw-addr "$MAC" 2>&1)
    case "$out" in *[eE]rror*) last="create: $out"; sleep "$STEP"; continue;; esac
    out=$(vc lcp create "host-$IF" host-if "$TAP" 2>&1)
    case "$out" in *[eE]rror*) last="lcp create: $out"; sleep "$STEP"; continue;; esac
    vc lcp lcp-sync on >/dev/null 2>&1
    vc lcp lcp-auto-subint on >/dev/null 2>&1
    out=$(vc set interface state "host-$IF" up 2>&1)
    case "$out" in *[eE]rror*) last="set state: $out"; sleep "$STEP"; continue;; esac
    last="四条都发完但 show lcp 里还没有 $IF/$TAP"
    sleep "$STEP"
done

echo "FAIL $IF → $TAP 在 $LIMIT 拍（约 $(awk -v a="$LIMIT" -v b="$STEP" 'BEGIN{printf "%.1f", a*b}') 秒）内没建成；最后一次：$last"
exit 1
