#!/bin/bash
# teardown.sh - 拆掉 sdwan-lab（只动 lab-*，不碰 srwg-*）
#
# 默认保留 test/lab/run/ 里的生成配置与这套 lab 一次性使用的控制器 CA（全部
# 在 .gitignore 里）；--purge 一并删掉。
#
# 用法: test/lab/teardown.sh [--purge]
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
COMPOSE="${HERE}/lab-compose.yml"
PROJECT=sdwan-lab

PURGE=0
for a in "$@"; do
    case "$a" in
      --purge) PURGE=1 ;;
      -h|--help) sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
      *) echo "unknown option: $a" >&2; exit 2 ;;
    esac
done

echo "== 拆掉 $PROJECT（容器/网络/匿名卷）"
docker compose -f "$COMPOSE" -p "$PROJECT" down -v --remove-orphans || {
    echo "compose down 失败，可手工检查：docker ps -a --filter name=lab-" >&2
}

if [ "$PURGE" = "1" ]; then
    echo "== 删除 ${HERE}/run"
    rm -rf "${HERE}/run"
else
    echo "== 保留 ${HERE}/run（--purge 可删）"
fi

echo "完成。srwg-* 未受影响："
docker ps --filter 'name=srwg-' --format '  {{.Names}}\t{{.Status}}' | head -10
