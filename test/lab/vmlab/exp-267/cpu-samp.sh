#!/bin/bash
# 只读采样：每核 busy 分项 + steal；跑 N 秒。不改任何配置、不重启任何进程。
N=${1:-10}
grep -E '^cpu[0-9]* ' /proc/stat > /tmp/s1.$$
sleep "$N"
grep -E '^cpu[0-9]* ' /proc/stat > /tmp/s2.$$
paste /tmp/s1.$$ /tmp/s2.$$ | awk -v n="$N" '{
  t1=$2+$3+$4+$5+$6+$7+$8+$9;  i1=$5+$6;
  t2=$13+$14+$15+$16+$17+$18+$19+$20; i2=$16+$17;
  dt=t2-t1; if(dt<=0) dt=1;
  printf "%-5s busy=%5.1f%%  user=%5.1f%% sys=%5.1f%% hardirq=%5.1f%% softirq=%5.1f%% steal=%5.1f%% idle+iowait=%5.1f%%\n",
    $1, 100*(dt-(i2-i1))/dt, 100*($13+$14-$2-$3)/dt, 100*($15-$4)/dt,
    100*($18-$7)/dt, 100*($19-$8)/dt, 100*($20-$9)/dt, 100*(i2-i1)/dt;
}'
rm -f /tmp/s1.$$ /tmp/s2.$$
