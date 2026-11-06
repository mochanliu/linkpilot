#!/bin/bash
# §9.7 六方向：ping -c 50 -i 0.2 -W 1 -I <源 loopback>
echo "# 六方向 $(date -u +%FT%TZ) 标签=${TAG:-?}"
printf "%-22s %-12s %-10s %s\n" 源 目的 丢包 "rtt min/avg/max/mdev"
run(){ ssh -n $1 "ping -c 50 -i 0.2 -W 1 -I $2 $3" 2>&1 | awk -v s="$2" -v d="$3" '/packet loss/{gsub(",","");for(i=1;i<=NF;i++){if($i~/loss/)l=$(i-1)}printf "%-22s %-12s %-10s ","",$3,l} /rtt min|round-trip/{print $NF" "$0}' | sed "s/          */ /g"; }
for spec in "cust12 172.16.12.1 172.16.3.1" "cust12 172.16.12.1 172.16.4.1" "cust3 172.16.3.1 172.16.12.1" "cust3 172.16.3.1 172.16.4.1" "cust4 172.16.4.1 172.16.12.1" "cust4 172.16.4.1 172.16.3.1"; do
  set -- $spec; H=$1; SRC=$2; DST=$3
  out=$(ssh -n $H "ping -c 50 -i 0.2 -W 1 -I $SRC $DST" 2>&1)
  loss=$(echo "$out" | grep -oE '[0-9.]+% packet loss' | cut -d' ' -f1)
  rtt=$(echo "$out" | grep -oE 'min/avg/max[/a-z]* = [0-9.]+/[0-9.]+/[0-9.]+/[0-9.]+' | sed 's/.* = //')
  printf "%-22s %-12s %-10s %s\n" "$H($SRC)" "$DST" "${loss:-超时}" "${rtt:--}"
done
