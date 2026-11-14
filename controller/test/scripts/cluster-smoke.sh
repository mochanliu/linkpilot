#!/usr/bin/env bash
# cluster-smoke.sh —— 控制面 HA 的端到端冒烟（容器里就能跑，不依赖 VM lab）。
#
# 走的是客户会走的那条路：
#   1. 一台单机 → 控制台点「开启集群」
#   2. 用加入码把第 2、3 台接进来（learner → voter）；同一个地址加第二次要被拒
#   3. kill -9 掉 leader：剩下的两台要能选出新主，写还要能提交
#   4. 把挂掉的那台装回来（同一份数据目录，命令行里还留着旧加入码）：它要自己追上
#   5. 替换一台：新机器跑一条命令，旧的那台自动下线
#   6. 下线一台
#
# 每一步都用控制台的 HTTP API 判定，不读状态文件、不改数据 —— 断言的是"控制台
# 里看到的样子"，也就是客户看到的样子。
#
# 用法： cluster-smoke.sh [工作目录]
#   SR_CTRL=/path/to/sr-ctrl  指定二进制（默认 /tmp/sr-ctrl；没有就 go build）
#   P_BASE=18443 C_BASE=18081 改端口基址（同机跑多套时错开）
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
WORK=${1:-/tmp/cluster-smoke}
BIN=${SR_CTRL:-/tmp/sr-ctrl}
P_BASE=${P_BASE:-18443}
C_BASE=${C_BASE:-18081}
TOK=cluster-smoke-$$
FAILED=0
PIDS=()

say()  { printf '\n== %s\n' "$*"; }
ok()   { printf '  ok    %s\n' "$*"; }
bad()  { printf '  FAIL  %s\n' "$*"; FAILED=$((FAILED + 1)); }
info() { printf '        %s\n' "$*"; }

cleanup() { for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null; done; }
trap cleanup EXIT

# ---------------------------------------------------------------- 工具

p_port() { echo $((P_BASE + $1 - 1)); }
c_port() { echo $((C_BASE + $1 - 1)); }

# api <n> <method> <path> [json]
api() {
	local n=$1 m=$2 path=$3 body=${4:-}
	local cj="$WORK/jar$n"
	if [ -n "$body" ]; then
		curl -sS -m 10 -b "$cj" -c "$cj" -X "$m" -H 'Content-Type: application/json' \
			-d "$body" "http://127.0.0.1:$(c_port "$n")$path"
	else
		curl -sS -m 10 -b "$cj" -c "$cj" -X "$m" "http://127.0.0.1:$(c_port "$n")$path"
	fi
}

# pick <path> —— 从 stdin 的 JSON 取一个值（a.b / members[0].role）
pick() {
	python3 -c '
import json,sys
cur=json.load(sys.stdin)
for seg in sys.argv[1].split("."):
    while seg and "[" in seg:
        name,rest=seg.split("[",1)
        if name: cur=cur[name]
        idx,seg=rest.split("]",1)
        cur=cur[int(idx)]
    if seg: cur=cur[seg]
print(cur if not isinstance(cur,bool) else str(cur).lower())
' "$1"
}

login() { api "$1" POST /api/login "{\"token\":\"$TOK\"}" >/dev/null; }

# 会话是每个节点各自内存里的一张表，进程重启就没了 —— 所以按节点懒登录。
view() {
	[ -f "$WORK/.sess$1" ] || { login "$1" && touch "$WORK/.sess$1"; }
	api "$1" GET /api/cluster
}

start_node() { # <n> <name> [extra...]
	local n=$1 name=$2; shift 2
	mkdir -p "$WORK/c$n"
	"$BIN" -data "$WORK/c$n" -listen "127.0.0.1:$(p_port "$n")" \
		-console "127.0.0.1:$(c_port "$n")" -admin-token "$TOK" \
		-node-name "$name" "$@" >"$WORK/c$n.log" 2>&1 &
	PIDS+=($!)
	rm -f "$WORK/.sess$n"
}

waitfor() { # <秒> <描述> <命令...>
	local secs=$1 desc=$2; shift 2
	local end=$(( $(date +%s) + secs ))
	while [ "$(date +%s)" -lt "$end" ]; do
		if "$@" >/dev/null 2>&1; then ok "$desc"; return 0; fi
		sleep 0.3
	done
	bad "$desc（${secs}s 超时）"
	return 1
}

member_field() { # <n> <name> <field>
	view "$1" | python3 -c '
import json,sys
d=json.load(sys.stdin); name,field=sys.argv[1],sys.argv[2]
for m in d.get("members") or []:
    if m.get("name")==name:
        print(m.get(field,"")); raise SystemExit
print("")
' "$2" "$3"
}

voters() { # <n> → 活跃 voter 数
	view "$1" | python3 -c '
import json,sys
d=json.load(sys.stdin)
print(sum(1 for m in (d.get("members") or []) if m.get("role")=="voter" and m.get("status")=="active"))
'
}

leader_idx() { # 哪一号节点是 leader（找不到输出空）
	local n
	for n in 1 2 3 4; do
		[ "$(view "$n" 2>/dev/null | pick role)" = leader ] && { echo "$n"; return; }
	done
}

# ---- 判定用的谓词
is_leader()     { [ "$(view "$1" | pick role)" = leader ]; }
is_standalone() { [ "$(view "$1" | pick mode)" = standalone ]; }
has_voters()    { [ "$(voters "$1")" -eq "$2" ]; }
is_voter()      { [ "$(member_field "$1" "$2" role)" = voter ]; }
is_gone()       { [ "$(member_field "$1" "$2" status)" = decommissioned ]; }
has_leader_other_than() { # <n> <不许是这台>
	local l; l=$(view "$1" | pick leader)
	[ -n "$l" ] && [ "$l" != "$2" ]
}
caught_up() { # applied 追平 commit
	local a c
	a=$(view "$1" | pick applied); c=$(view "$1" | pick commit)
	[ -n "$a" ] && [ -n "$c" ] && [ "$a" -ge "$c" ]
}

endpoint_refused() { # <n> <endpoint> —— 期望签发被拒（地址已被某个活跃成员占着）
	api "$1" POST /api/cluster \
		"{\"action\":\"token\",\"name\":\"dup\",\"endpoint\":\"$2\"}" 2>/dev/null |
		grep -q "已经是成员"
}

ignored_stale_join() { # 日志里要有"忽略 -join"
	grep -q "忽略 -join" "$WORK/c$1.log"
}

replace_addr_ok() { # <n> <旧成员 id> <旧地址> —— 替换场景要能签发加入码
	api "$1" POST /api/cluster \
		"{\"action\":\"token\",\"kind\":\"replace\",\"replace_id\":\"$2\",\"name\":\"tmpx\",\"endpoint\":\"$3\"}" \
		2>/dev/null | grep -q '"token"'
}

# 写一笔真实数据（建租户）：既是硬状态的一行，也是客户最常做的动作。
# 写到 leader 上 —— follower 上的写会由**服务端代理**转给 leader（见
# write_via_follower：浏览器不会跟着 307 把会话带过去，所以这里不靠重定向）。
write_ok() { # <tenant_id>
	local l; l=$(leader_idx)
	[ -z "$l" ] && return 1
	api "$l" POST /api/tenants "{\"tenant_id\":\"$1\",\"name\":\"冒烟租户 $1\"}" | grep -q '"tenant_id"'
}

write_matches() { # <n> <tenant_id>：写进去的那一行在别的节点上也读得到
	view "$1" >/dev/null 2>&1   # 这个节点还没登录过就先登一下（会话是每节点一份）
	api "$1" GET /api/state | python3 -c '
import json,sys
d=json.load(sys.stdin)
print("yes" if any(t.get("tenant_id")==sys.argv[1] for t in (d.get("tenants") or [])) else "no")
' "$2" | grep -qx yes
}

# 在 follower 的控制台上写：会话是本机的，写由 follower 代理给 leader 提交，
# 应答原样回来。客户看到的是"任何一台都能改"，不需要知道谁是 leader。
# follower 上的"有多数派"必须和 leader 说的一致。
# 走进 lab 之前这条是错的：lastHeard 只有 leader 会打点，follower 按同一套算法
# 数永远只数到自己 → 一切正常也喊"无多数派 · 只读"（2026-11-14 实测）。
follower_quorum() {
	local n l
	l=$(leader_idx); [ -z "$l" ] && return 1
	for n in 1 2 3; do
		[ "$n" = "$l" ] && continue
		view "$n" | grep -q '"quorum":true' || return 1
	done
	return 0
}

write_via_follower() {
	local n l out
	l=$(leader_idx); [ -z "$l" ] && return 1
	for n in 1 2 3; do
		[ "$n" = "$l" ] && continue
		view "$n" >/dev/null 2>&1 || continue
		out=$(api "$n" POST /api/tenants '{"tenant_id":"smoke-proxy","name":"经 follower 写"}')
		printf '%s' "$out" | grep -q '"tenant_id"' || continue
		api "$l" GET /api/state | grep -q 'smoke-proxy' && return 0
	done
	return 1
}

# ---------------------------------------------------------------- 0. 准备

say "准备"
if [ ! -x "$BIN" ]; then
	( cd "$REPO" && go build -o "$BIN" ./cmd/sr-ctrl ) || { bad "go build 失败"; exit 1; }
	ok "已构建 $BIN"
else
	ok "使用 $BIN"
fi
rm -rf "$WORK"; mkdir -p "$WORK"
info "工作目录 $WORK"

say "1. 单机 → 开启集群"
start_node 1 c1
waitfor 25 "控制台起来了" login 1
waitfor 5 "初始是单机" is_standalone 1
api 1 POST /api/cluster "{\"action\":\"init\",\"endpoint\":\"127.0.0.1:$(p_port 1)\",\"console\":\"127.0.0.1:$(c_port 1)\",\"name\":\"c1\"}" >/dev/null
waitfor 25 "第 1 台成为 leader" is_leader 1
waitfor 10 "写能提交" write_ok smoke-a

say "2. 加入第 2、3 台"
for n in 2 3; do
	tok=$(api 1 POST /api/cluster "{\"action\":\"token\",\"name\":\"c$n\",\"endpoint\":\"127.0.0.1:$(p_port "$n")\",\"console\":\"127.0.0.1:$(c_port "$n")\"}" | pick token)
	if [ -z "$tok" ] || [ "$tok" = None ]; then bad "签发 c$n 的加入码失败"; continue; fi
	ok "拿到 c$n 的加入码"
	TOKS[$n]=$tok
	start_node "$n" "c$n" -join "https://127.0.0.1:$(p_port 1)" -join-token "$tok"
	waitfor 30 "c$n 成为 voter" is_voter 1 "c$n"
done
waitfor 10 "三台都是活跃 voter" has_voters 1 3
waitfor 10 "follower 也说有多数派" follower_quorum

say "2.1 同一个地址不能被两个成员占着"
# 两个成员指向同一个地址会把多数派算虚（leader 发给同一台两次，另一份永远追不上），
# 所以这一步要在"点生成加入码"时就拒掉，而不是等新机器跑起来再猜。
waitfor 10 "撞已有地址的加入码被拒" endpoint_refused 1 "127.0.0.1:$(p_port 2)"

say "2.2 替换可以接管旧机器的地址"
# 现场最常见的换机方式：旧机器坏了，新机器直接用它原来的 IP。签发与加入两边
# 都必须放行（以前加入侧只认"地址属于活着的成员"，会把这条路堵死）。
cid=$(member_field 1 c3 id)
waitfor 10 "替换目标可以复用地址" replace_addr_ok 1 "$cid" "127.0.0.1:$(p_port 3)"

say "3. kill -9 掉 leader"
old=$(view 1 | pick leader)
lnode=$(leader_idx); lnode=${lnode:-1}
info "leader = $old（第 $lnode 台）"
kill -9 "${PIDS[$((lnode - 1))]}" 2>/dev/null
ok "已 kill -9 第 $lnode 台"
s1=1; s2=2; [ "$lnode" = 1 ] && { s1=2; s2=3; }; [ "$lnode" = 2 ] && { s1=1; s2=3; }
waitfor 20 "剩下的两台选出新 leader" has_leader_other_than "$s1" "$old" ||
	waitfor 20 "剩下的两台选出新 leader" has_leader_other_than "$s2" "$old"
waitfor 10 "切换期间写还能提交" write_ok smoke-b
waitfor 10 "写的内容已经复制到另一台" write_matches "$s2" smoke-b
waitfor 10 "follower 上的写被代理到 leader 并提交成功" write_via_follower

say "4. 把挂掉的那台装回来（命令行里还留着上一次的加入码）"
# 部署时把 -join 顺手写进 systemd 单元是常见写法。加入码是一次性的、30 分钟过期，
# 重放一定被拒 —— 如果进程在这上面 log.Fatal，这台机器就再也起不来了。所以这里
# 故意带着旧加入码重启：它必须忽略它、接着跑，并在日志里说清楚。
start_node "$lnode" "c$lnode" -join "https://127.0.0.1:$(p_port 1)" -join-token "${TOKS[$lnode]:-stale-token}"
waitfor 30 "重启的节点追上日志" caught_up "$lnode"
waitfor 5 "旧的 -join 被忽略（不是 fatal）" ignored_stale_join "$lnode"

say "5. 替换一台"
L=$(leader_idx); L=${L:-$s1}
victim=$(view "$L" | python3 -c '
import json,sys
d=json.load(sys.stdin)
for m in d.get("members") or []:
    if m.get("role")=="voter" and m.get("status")=="active" and not m.get("self"):
        print(m["name"]); break
')
[ -z "$victim" ] && victim=c2
vid=$(member_field "$L" "$victim" id)
info "替换目标：$victim（$vid）"
tok=$(api "$L" POST /api/cluster "{\"action\":\"token\",\"kind\":\"replace\",\"replace_id\":\"$vid\",\"name\":\"c4\",\"endpoint\":\"127.0.0.1:$(p_port 4)\",\"console\":\"127.0.0.1:$(c_port 4)\"}" | pick token)
if [ -z "$tok" ] || [ "$tok" = None ]; then
	bad "签发替换用的加入码失败"
else
	start_node 4 c4 -join "https://127.0.0.1:$(p_port "$L")" -join-token "$tok"
	waitfor 40 "c4 成为 voter" is_voter "$L" c4
	waitfor 40 "$victim 被自动下线" is_gone "$L" "$victim"
fi

say "6. 下线一台"
cid=$(member_field "$L" c4 id)
if [ -n "$cid" ]; then
	api "$L" POST /api/cluster "{\"action\":\"remove\",\"id\":\"$cid\"}" >/dev/null
	waitfor 20 "c4 已下线" is_gone "$L" c4
fi
info "现在活跃 voter：$(voters "$L") 台"

say "结果"
if [ "$FAILED" -eq 0 ]; then
	printf '  全部通过\n'
else
	printf '  %d 项失败（日志在 %s/c*.log）\n' "$FAILED" "$WORK"
fi
exit "$FAILED"
