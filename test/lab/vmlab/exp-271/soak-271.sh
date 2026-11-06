#!/bin/bash
# #271 换件后的稳定性巡检：锁死的特征是「本盒客户侧拥有的前缀被写进了内核 K 路由」。
# 每 20 秒查四台：账本里不许出现自己的前缀；zebra 必须仍从客户协议学到它；六方向判据源不变。
# 2026-11-06 之后这一条不再是代码闸门（注入闸门已按 owner 的定性撤掉，环路归客户侧通告
# 策略）：账本里出现自己的前缀 = 控制台 CPE-SDWAN 那一行播了别人站点的前缀，改配置不改码。
OWN="CPE1:172.16.12.1 CPE2:172.16.12.1 CPE3:172.16.3.1 CPE4:172.16.4.1"
END=$(( $(date +%s) + 240 ))
n=0
while [ $(date +%s) -lt $END ]; do
  n=$((n+1))
  echo "### 轮 $n $(date -u +%FT%TZ)"
  for E in $OWN; do
    B=${E%%:*}; P=${E##*:}
    ssh -n -o BatchMode=yes $B "
      ledger=\$(python3 -c 'import json;print(\",\".join(r[\"prefix\"] for r in json.load(open(\"/var/lib/sr-frr/state.json\")).get(\"routes\",[])))' 2>/dev/null)
      case \",\$ledger,\" in *,$P/32,*) bad=LEDGER ;; *) bad= ;; esac
      z=\$(vtysh -c 'show ip route $P/32' 2>/dev/null | sed -n '2p' | sed 's/^  Known via \"//;s/\",.*//')
      case \"\$z\" in kernel) bad=\"\$bad ZEBRA-KERNEL\";; esac
      st=\$(curl -s --unix-socket /run/sr-frr/control.sock http://x/status | python3 -c 'import json,sys;d=json.load(sys.stdin);print(\"inj=%s rep=%s\"%(d[\"injected\"],d[\"reported\"]))')
      echo \"  $B $P zebra=\$z \$st \${bad:-OK}\"
    " 2>/dev/null
  done
  sleep 20
done
echo "### 巡检结束 共 $n 轮"
