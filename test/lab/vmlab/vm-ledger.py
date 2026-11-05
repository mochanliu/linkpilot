#!/usr/bin/env python3
"""vm-ledger.py - 把控制台的隧道台账 + 手工配的客户侧 loopback 摊成一张 TSV。

VM 台的隧道是控制台配的（S6），台账不在脚本里；容器台 verify.sh 那种
`CPE_WG=("wg0:51820:192.168.99.1 …")` 硬抄表在这里一定会过期。所以期望值分两种来源：

  隧道 / 端口 / 颜色 / MTU / overlay /32  ← 控制台 GET /api/underlay（就是页面上那五行）
  本站播出去的客户 loopback、cust 虚拟机的 ssh 别名、盒内客户口地址 ← 本文件顶部那几张
  手工表（拍板 5：客户 IP 由 cust 盒子的 loopback 播）

wg 接口名不在这里取 —— 只有盒子自己知道 10.250.0.2 落在本机哪张口，
verify/status 用 `vppctl show interface addr` 现读，跟台账里的 overlay 对上。

每行第一列是记录类型：
    node  1  CPE1  192.168.1.101  online  15000  20000,20001  2  8  true  sr-agent/0.1 …
    box   1  CPE1  172.16.12.1  172.31.12.1  172.31.12.13  cust12  ospf
    tun   1  t2  10.250.0.1  20000  192.168.1.101  host-enp0s9  3  10.250.0.2  20000  192.168.1.103  0  1368
    rib   1  172.16.3.1/32  10.250.0.2
    rib   3  172.16.12.1/32  10.250.0.1,10.250.0.7   # 同站点两台各播一次 ⇒ 两个下一跳
    local 1  172.16.12.1/32
    ping  cust12  172.16.12.1  172.16.4.1        # 从哪台 cust、用什么源、ping 谁

用法：vm-ledger.py > /tmp/vm-verify-ledger.tsv
      （vm-verify.sh 和 vm-status.sh 默认读这个路径，两边必须是同一份台账；
       要换位置就给两个脚本传同一个 VM_LEDGER=<path>。）
"""
import importlib.util
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("vm_console", os.path.join(HERE, "vm-console.py"))
vm = importlib.util.module_from_spec(spec)
spec.loader.exec_module(vm)

# ---- 手工表（lab 值，控制器不猜）----
# node_id → 本站对外播的客户 loopback。1 和 2 同站点，所以同一个地址。
SITE_LOOPBACK = {1: "172.16.12.1", 2: "172.16.12.1", 3: "172.16.3.1", 4: "172.16.4.1"}
# node_id → 那台客户虚拟机的 ssh 别名（ProxyJump 走自己的盒子，见 ~/.ssh/config）
CUST_SSH = {1: "cust12", 2: "cust12", 3: "cust3", 4: "cust4"}
# node_id → 盒内客户口地址 / 对面客户机的地址 / 客户侧路由协议（S5 手工配的）
BOX = {
    1: dict(cust_ip="172.31.12.1", peer_cust="172.31.12.13", proto="ospf"),
    2: dict(cust_ip="172.31.12.2", peer_cust="172.31.12.13", proto="ospf"),
    3: dict(cust_ip="172.31.3.1", peer_cust="172.31.3.2", proto="bgp"),
    4: dict(cust_ip="172.31.4.1", peer_cust="172.31.4.2", proto="bgp"),
}


def main():
    api, _ = vm.api_tenant()
    try:
        und = api.call("GET", "/api/underlay")
        nodes = {int(n["node_id"]): n for n in und["nodes"]}

        for nid in sorted(nodes):
            n = nodes[nid]
            td = n.get("tunnel_delivery") or {}
            print("\t".join(str(x) for x in [
                "node", nid, n["serial"], n.get("observed_addr") or "",
                n.get("status") or "", n.get("transit_port") or "",
                ",".join(str(p) for p in (n.get("ports") or [])),
                n.get("tunnel_count") or 0,
                td.get("version") if td.get("version") is not None else "-",
                "true" if td.get("ok") else "false",
                n.get("agent_version") or "-"]))
            b = BOX[nid]
            print("\t".join(["box", str(nid), n["serial"], SITE_LOOPBACK[nid],
                             b["cust_ip"], b["peer_cust"], CUST_SSH[nid], b["proto"]]))

        # 隧道按本端摊开：一条隧道在两端各有一个 overlay /32 和一个监听端口。
        # 列序：本端(node,tid,overlay,port,外层地址,VPP 出口口) + 对端(node,overlay,port,地址) + 颜色/MTU
        per_node = {}
        for t in und.get("tunnels") or []:
            for end, peer in ((t["a"], t["b"]), (t["b"], t["a"])):
                per_node.setdefault(int(end["node_id"]), []).append(
                    "\t".join(str(x) for x in [
                        "tun", end["node_id"], t["id"], end["overlay"], end["port"],
                        end["addr"], end["iface"],
                        peer["node_id"], peer["overlay"], peer["port"], peer["addr"],
                        t.get("color") or 0, t.get("mtu")]))
        for nid in sorted(per_node):
            for line in sorted(per_node[nid]):
                print(line)

        # 期望的 sr-route RIB：本站之外的每个 loopback，下一跳 = 到那个站点里
        # 任一台盒子的隧道对端 overlay。同站点两台都在的（site-12）会是两个下一跳。
        for nid in sorted(nodes):
            hops = {}
            for t in und.get("tunnels") or []:
                for end, peer in ((t["a"], t["b"]), (t["b"], t["a"])):
                    if int(end["node_id"]) != nid:
                        continue
                    lb = SITE_LOOPBACK.get(int(peer["node_id"]))
                    if lb and lb != SITE_LOOPBACK[nid]:
                        hops.setdefault(lb + "/32", set()).add(peer["overlay"])
            for pfx in sorted(hops):
                print("\t".join(["rib", str(nid), pfx, ",".join(sorted(hops[pfx]))]))
            print("\t".join(["local", str(nid), SITE_LOOPBACK[nid] + "/32"]))

        # 客户视角：每台 cust 用自己的 loopback 作源，ping 其它站点的 loopback。
        # 源必须是 loopback —— overlay 只承载 172.16/16 里的 /32，用客户局域网地址
        # 当源，回程会在对端盒子里黑洞掉（2026-11-05 为此误判过一次"六方向全丢包"）。
        # 同站点两台盒子共用一台 cust 虚拟机，(别名, 源) 去重后只出一份方向。
        sources = []
        for nid in sorted(CUST_SSH):
            key = (CUST_SSH[nid], SITE_LOOPBACK[nid])
            if key not in sources:
                sources.append(key)
        for ssh_alias, src in sources:
            for other in sorted(set(SITE_LOOPBACK.values()) - {src}):
                print("\t".join(["ping", ssh_alias, src, other]))
    finally:
        api.logout()


if __name__ == "__main__":
    sys.exit(main())
