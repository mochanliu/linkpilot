#!/bin/bash
# 缺陷⑦ (#268) 的修复件铺到 CPE1/2/3。
#
# 每台次序：记下改前 → 停 watch 定时器（缺陷⑧ 会在同一秒把 sr-fib 起两份，读数会脏）
# → 传件并校哈希 → 同盘临时件 + mv 原子换（覆盖运行中的 ELF 会 ETXTBSY）
# → pkill sr-fib，交回产品开机清单 start.sh up → 推隧道表回内存（G-2 的 lab 代偿）
# → 改后读数 → 恢复定时器原状。
# 全程不碰 VPP：客户流量只在 sr-fib 那 1~2 秒的进程空档里有理论风险，实测见日志。
set -uo pipefail

# 用法（在 Mac 上跑，node 是 1/2/3）：
#   BIN=/path/to/linux/sr-fib bash roll-268.sh 1 2 3
# D 是读数和日志的落盘目录，默认建在临时目录；原始数据不进仓库。
D=${D:-$(mktemp -d)/gate-268}
BIN=${BIN:?要显式给出待铺的 sr-fib 二进制（容器里 GOOS=linux go build 出来的那个）}
EXPECT=$(shasum -a 256 "$BIN" | cut -d' ' -f1)
LOGD=$D/roll
mkdir -p "$LOGD"

readout() {  # $1=node
  ssh "CPE$1" 'bash -s' <<'EOS' 2>/dev/null
p=$(pgrep -x sr-fib | tr '\n' ',')
h=$(sha256sum /usr/local/bin/sr-fib | cut -c1-12)
# 主表里的 172.16/32 有两种来源，读数必须分开：
#   r_sr = sr-fib 装的 overlay 客户前缀（下一跳是 10.250.0.x 的 wg 口）
#   r_all= 主表里所有 172.16/32，含 agent 经 linux-cp 装的 lcp-rt-dynamic 直连路由
# 只数 r_all 会把 agent 那条也算进 sr-fib 的功劳里（CPE4 实测 3 vs 2）。
read -r rs ra <<<"$(vppctl 'show ip fib' | awk '
  /^172\.16\.[0-9]+\.[0-9]+\/32/{p=$1; a[$1]=1}
  /via 10\.250\.0\.[0-9]+ wg/{if(p!=""&&s[p]++==0)n++}
  END{c=0; for(k in a)c++; printf "%d %d", n+0, c}')"
t=$(vppctl "show ip fib table 250" | grep -cE 'via 10\.250\.0\.[0-9]+ wg')
s=$(curl -s --max-time 2 http://127.0.0.1:19011/status)
tn=$(printf '%s' "$s" | python3 -c 'import json,sys;print(len(json.load(sys.stdin).get("tunnels") or []))' 2>/dev/null)
cv=$(printf '%s' "$s" | python3 -c 'import json,sys;print((json.load(sys.stdin).get("last") or {}).get("converged"))' 2>/dev/null)
bp=$(grep -c "broken pipe" /var/log/sr-box/fib.log 2>/dev/null)
tm=$(systemctl is-active sr-box-watch.timer)
echo "pid=$p sha=$h r_sr=$rs r_all=$ra t250=$t tunnels=$tn converged=$cv broken_pipe=$bp timer=$tm"
EOS
}

for n in "$@"; do
  L="$LOGD/cpe$n.log"
  : > "$L"
  say() { echo "[$(date '+%T')] $*" | tee -a "$L"; }

  say "== node $n 改前 =="
  readout "$n" | tee -a "$L"

  say "1/5 停 watch 定时器"
  ssh "CPE$n" 'systemctl stop sr-box-watch.timer; systemctl is-active sr-box-watch.timer || true' 2>/dev/null | tee -a "$L"

  say "2/5 传件（gzip，实测链路 ~13KB/s，需要几分钟）"
  gzip -9 -c "$BIN" | ssh "CPE$n" 'gunzip > /tmp/sr-fib.new' 2>>"$L"
  got=$(ssh "CPE$n" 'sha256sum /tmp/sr-fib.new | cut -d" " -f1' 2>/dev/null)
  if [ "$got" != "$EXPECT" ]; then
    say "!! 传输校验失败 got=$got expect=$EXPECT —— 这台不动，继续下一台"
    ssh "CPE$n" 'systemctl start sr-box-watch.timer' 2>/dev/null
    continue
  fi
  say "   哈希一致：$got"

  say "3/5 原子换二进制（进程未动）"
  ssh "CPE$n" 'cp /tmp/sr-fib.new /usr/local/bin/sr-fib.roll-tmp && chmod 0755 /usr/local/bin/sr-fib.roll-tmp && mv -f /usr/local/bin/sr-fib.roll-tmp /usr/local/bin/sr-fib && sha256sum /usr/local/bin/sr-fib | cut -c1-12' 2>>"$L" | tee -a "$L"

  say "4/5 只重启 sr-fib，经产品开机清单带回"
  ssh "CPE$n" 'pkill -x sr-fib; sleep 1; /usr/local/lib/sr-box/start.sh up; echo "up 退出码=$?"; pgrep -x sr-fib' 2>>"$L" | tee -a "$L"
  say "   重启后日志（此刻隧道表为空、路径被跳过 —— 这是 #288 之前 G-2 的现场）:"
  ssh "CPE$n" 'tail -4 /var/log/sr-box/fib.log' 2>/dev/null | sed 's/^/     /' | tee -a "$L"

  # 5/5 原先跑的是 lab 代偿 vm-fib-sync.sh（手工 PUT 隧道表）。#288 起这张表由 sr-agent
  # 每拍派生，产品里有人管了，lab 再手工补一枪就等于把缺口盖回去 —— 所以改成"等它自己回来"，
  # 并且把等待时间打出来：等不到就是回归，不是"lab 环境问题"。
  say "5/5 等 sr-agent 把隧道表派生回来（#288：产品自己维护，不再手工推）"
  t0=$(python3 -c 'import time;print(int(time.time()))')
  tun=0
  for i in $(seq 1 40); do
    # tail -1：known_hosts 的 "Permanently added" 会混进 stdout（不受 LogLevel 控制），
    # 不剃掉的话 `${tun:-0} -gt 0` 拿到的是两行文本，判据永远是假红。
    tun=$(ssh "CPE$n" "curl -s --max-time 3 http://127.0.0.1:19011/status | python3 -c 'import json,sys;print(len(json.load(sys.stdin).get(\"tunnels\") or []))'" 2>/dev/null | tr -d '\r' | tail -1)
    [ "${tun:-0}" -gt 0 ] 2>/dev/null && break
    sleep 1
  done
  say "   隧道表=${tun:-0} 条，等了 $(( $(python3 -c 'import time;print(int(time.time()))') - t0 ))s"
  [ "${tun:-0}" -gt 0 ] && say "   OK：表自己回来了" \
      || say "   !! 40s 内没回来 —— 派生器没在推（看 /var/log/sr-box/agent.log 的「sr-fib 隧道表」行）"

  say "恢复定时器原状"
  ssh "CPE$n" 'systemctl start sr-box-watch.timer; systemctl is-active sr-box-watch.timer' 2>/dev/null | tee -a "$L"

  say "== node $n 改后 =="
  readout "$n" | tee -a "$L"
  say ""
done
