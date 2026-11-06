#!/bin/bash
for B in CPE2 CPE3 CPE4; do
  ( time (scp -q -o BatchMode=yes /tmp/sr-frr.gz $B:/root/vmlab/artifacts/linux/sr-frr.gz && \
     ssh -n -o BatchMode=yes $B 'cd /root/vmlab/artifacts/linux && gunzip -c sr-frr.gz > sr-frr.new && sha256sum sr-frr.new | cut -c1-16 && install -m 0755 sr-frr.new sr-frr && rm -f sr-frr.new sr-frr.gz && sha256sum /usr/local/bin/sr-frr | cut -c1-16' ) > /tmp/p271/push-$B.log 2>&1 ; echo "done $B rc=$?" >> /tmp/p271/push-status.txt ) &
done
wait
echo ALL_PUSHED >> /tmp/p271/push-status.txt
