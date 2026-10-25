# CPE lab 镜像：在 srwg-dev（VPP 26.06 + Go）上再装 FRR 10.7.1。
#
# 盒子里 FRR 和 VPP 是同一个盒子里的两半：VPP 负责转发，FRR 与客户侧路由器
# 跑路由协议。lab 不再把 FRR 拆成独立容器，所以这里必须把 FRR 装进 CPE 镜像。
#
# FRR 版本与 test/docker 的 quay.io/frrouting/frr:10.7.1 严格对齐：ZAPI v6
# 的线格式跨大版本会变，sr-frr 只对这一个版本负责。
FROM srwg-dev:latest

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates gnupg \
 && curl -fsSL https://deb.frrouting.org/frr/keys.gpg -o /usr/share/keyrings/frrouting.gpg \
 && echo 'deb [signed-by=/usr/share/keyrings/frrouting.gpg] https://deb.frrouting.org/frr jammy frr-10' \
        > /etc/apt/sources.list.d/frr.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends frr frr-pythontools \
 && rm -rf /var/lib/apt/lists/*

# zebra + bgpd 由 bringup.sh 用 /etc/frr/frr.conf 起；这里只把 daemons 打开。
# mgmtd 保持默认开启：FRR 10 的 watchfrr 会按 daemons 文件把整套拉起来。
RUN sed -i 's/^zebra=no/zebra=yes/; s/^bgpd=no/bgpd=yes/' /etc/frr/daemons \
 && mkdir -p /run/frr /var/log/frr \
 && chown frr:frr /run/frr /var/log/frr

EXPOSE 51820/udp 51821/udp
