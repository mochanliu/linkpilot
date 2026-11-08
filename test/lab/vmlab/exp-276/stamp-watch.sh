#!/bin/bash
# #276 的一次性探针：**占用 sr-route /watch 的唯一订阅位**，把每一帧 SSE 打上盒内墙钟毫秒戳。
# 用途只有一个：把装路边 ok→ours 那段切成「sr-route 什么时候把事件发出来」和
# 「sr-fib 拿到事件之后写了多久」两半。跑它之前 sr-fib 必须让位（见 watch-timeline.sh），
# 所以这段时间数据面是**故意冻结**的，脚本自己负责在同一轮里把它放回原样。
#
# 每行格式： <epoch.ms> <原始帧行>   —— 原始行有三种：`event: routes`、`data: {…}`、`: ping`。
# 只在收到 data: 那一行取时间戳；ping 也记（它是"流活着但没话可说"的证据）。
set -u
URL=${1:-http://127.0.0.1:19001/watch}
curl -sN --max-time 300 "$URL" | while IFS= read -r ln; do
  printf '%s %s\n' "$(date +%s.%3N)" "$ln"
done
