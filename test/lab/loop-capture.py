#!/usr/bin/env python3
"""loop-capture.py - 环路实验台的**抓表器**：一条命令把每台设备的相关表项落成一个目录。

用法：
    python3 loop-capture.py <标签>            # 抓一套，写 test/lab/run/loop/capture/<标签>/
    python3 loop-capture.py <标签> --no-matrix

标签就是场景名（baseline、tunnel-down-cpe1-wan、half-publish…），每个场景抓一次，
报告里按标签引用。目录内容：

    00-总览.md        版本/回执/在线 + 控制台侧的隧道与泄露表 + 连通矩阵
    cpe<N>.md         一台盒子的全部表项（内核 / FRR / sr-route / sr-fib / sr-frr / VPP / VRRP）
    cust<N>.md        客户路由器的表（那是客户自己的网络，只读）
    raw/<设备>/<表>.txt  未加工的原文，报告里放不下时指到这里

抓的都是**读**动作：不写配置、不重启进程。唯一的例外是连通矩阵里的 ping，那是客户
视角的探测包，本身不改变任何状态。
"""
import importlib.util
import json
import os
import re
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
RUN = os.path.join(HERE, "run", "loop")
CAP = os.path.join(RUN, "capture")

# 复用 loop-console.py 的会话与租户凭据：抓表也要用同一个租户视角。
_spec = importlib.util.spec_from_file_location(
    "loopconsole", os.path.join(HERE, "loop-console.py"))
lc = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(lc)

BOXES = (1, 2, 3, 4)
CUSTS = {"12": ("lp-cust12", "172.16.12.1"), "3": ("lp-cust3", "172.16.3.1"),
         "4": ("lp-cust4", "172.16.4.1")}
LOOPS = ("172.16.12.1", "172.16.3.1", "172.16.4.1")
ROUTE_PORT = {1: 19001, 2: 19002, 3: 19003, 4: 19004}
FIB_PORT = {1: 19011, 2: 19012, 3: 19013, 4: 19014}


def docker(c, *cmd):
    r = subprocess.run(["docker", "exec", c] + list(cmd),
                       capture_output=True, text=True, timeout=120)
    out = (r.stdout or "").replace("\r", "")
    if r.returncode != 0:
        return "（命令失败 rc=%d）%s" % (r.returncode, (r.stderr or out).strip()[:400])
    return out


def write(path, text):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(text)


def section(md, rawdir, device, title, text, lang="text"):
    write(os.path.join(rawdir, "%s--%s.txt" % (device, title.replace(" ", "_")[:40])), text)
    md.append("### %s\n" % title)
    md.append("```%s\n%s\n```\n" % (lang, text.rstrip() or "（空）"))


FIB_KEEP = re.compile(r"^(172\.16\.|10\.250\.|10\.66\.|10\.76\.)")


def fib_digest(text, want_prefixes):
    """show ip fib 太长；按前缀白名单抽出条目本体（含 bucket/via/label 行）。"""
    out, cur, keep = [], None, False
    for line in text.splitlines():
        if line.startswith("ipv4-VRF:"):
            out.append("\n== %s" % line.split(",")[0])
            keep = False
            continue
        if not line.startswith(" ") and line.strip():
            # 白名单是不带掩码的地址；fib 里的行是 "172.16.12.1/32"，要先去掉 /len 再比
            keep = line.split("/")[0] in want_prefixes
            cur = line
            if keep:
                out.append("\n%s" % line)
            continue
        if keep:
            if "dpo-drop" in line and "cfg-flags:drop" in line:
                continue
            out.append(line)
    return "\n".join(x for x in out if x is not None)


def vrrp_dump(c):
    txt = docker(c, "vrrp-probe", "-dump")
    return txt if "vr" in txt or "失败" in txt else "（这张盒子没有 VRRP 实例）"


def capture(tag, matrix=True):
    outdir = os.path.join(CAP, tag)
    rawdir = os.path.join(outdir, "raw")
    os.makedirs(rawdir, exist_ok=True)
    creds = lc.read_creds()
    api = lc.Api()
    api.login_tenant(creds["username"], creds["password"])
    st = api.call("GET", "/api/state")
    u = api.call("GET", "/api/underlay")
    ov = api.call("GET", "/api/overlay")
    leak = api.call("GET", "/api/leak")
    bynode = {n["node_id"]: n for n in u["nodes"]}

    # ---- 总览 ----
    md = ["# 抓表 %s\n" % tag, "时间：%s\n" % time.strftime("%F %T"), ""]
    md.append("## 控制台侧（租户视角）\n")
    md.append("| node | 设备 | 状态 | 基础配置 发布/设备 | 隧道 下发/生效 | 站点 |")
    md.append("|---|---|---|---|---|---|")
    for nd in sorted(st["nodes"], key=lambda x: x["node_id"]):
        n = nd["node_id"]
        cv = api.call("GET", "/api/config?node=%d" % n)
        ds = cv.get("device_state") or {}
        td = (bynode[n].get("tunnel_delivery") or {})
        md.append("| %d | %s | %s | v%s/v%s%s | v%s/v%s%s（%d 条） | %s |" % (
            n, nd["serial"], nd["status"], cv.get("published_version"),
            ds.get("version"), "" if ds.get("ok") else " ⚠",
            td.get("version"), td.get("applying_version"),
            "" if td.get("ok") else " ⚠", td.get("tunnels"),
            (nd.get("runtime") or {}).get("site_name") or "-"))
    md.append("")
    for nd in sorted(st["nodes"], key=lambda x: x["node_id"]):
        ds = (api.call("GET", "/api/config?node=%d" % nd["node_id"]).get("device_state") or {})
        if ds.get("error"):
            md.append("- **node %d 的下发回执带错误**：%s\n" % (nd["node_id"], ds["error"]))
    section(md, rawdir, "console", "隧道表", json.dumps(u["tunnels"], ensure_ascii=False, indent=1), "json")
    section(md, rawdir, "console", "overlay 全局与导通",
            json.dumps({"escape_all_colors": ov.get("escape_all_colors"),
                        "groups": ov.get("groups"),
                        "rr_binds": ov.get("rr_binds"),
                        "nodes": ov.get("nodes")},
                       ensure_ascii=False, indent=1), "json")
    section(md, rawdir, "console", "RR 库",
            json.dumps(ov.get("rr_library"), ensure_ascii=False, indent=1), "json")
    section(md, rawdir, "console", "路由泄露行",
            json.dumps({"prefix_lists": leak.get("prefix_lists"),
                        "nodes": leak.get("nodes")},
                       ensure_ascii=False, indent=1), "json")

    if matrix:
        md.append("## 客户视角连通矩阵（每台 3 个包，源=自己的 loopback）\n")
        md.append("| 源 | " + " | ".join(LOOPS) + " |")
        md.append("|---|" + "---|" * len(LOOPS))
        for key, (container, src) in CUSTS.items():
            row = ["%s（cust%s，源 %s）" % (container, key, src)]
            for dst in LOOPS:
                if dst == src:
                    row.append("自己")
                    continue
                txt = docker(container, "ping", "-I", src, "-c", "3", "-W", "1", dst)
                # busybox 的汇总行是「3 packets transmitted, 3 packets received, 0% packet loss」
                got = re.search(r"(\d+) packets? received", txt)
                rtt = re.search(r"= [\d.]+/([\d.]+)/", txt)
                n = int(got.group(1)) if got else 0
                cell = "通 %d/3" % n if n == 3 else "**丢 %d/3**" % (3 - n)
                if n and rtt:
                    cell += "，avg %sms" % rtt.group(1)
                row.append(cell)
            md.append("| " + " | ".join(row) + " |")
        md.append("")

    # ---- 每台盒子 ----
    for n in BOXES:
        c = "lp-cpe%d" % n
        md2 = ["# lp-cpe%d（node %d）%s\n" % (n, n, time.strftime("抓于 %F %T")), ""]
        md2.append("## 内核（盒子自己的 netns）\n")
        section(md2, rawdir, c, "ip -4 addr", docker(c, "ip", "-4", "addr", "show"))
        section(md2, rawdir, c, "ip route", docker(c, "ip", "route", "show"))
        section(md2, rawdir, c, "ip route table all 摘要",
                docker(c, "bash", "-lc",
                       "ip route show table all | grep -vE '^(local|10\\.99\\.|unreachable)' | head -40"))
        md2.append("## 盒内 FRR（客户侧协议）\n")
        section(md2, rawdir, c, "frr running-config 关键段",
                docker(c, "bash", "-lc",
                       "vtysh -c 'show running-config' | sed -n '/router /,$p'"))
        section(md2, rawdir, c, "ospf neighbor", docker(c, "vtysh", "-c", "show ip ospf neighbor"))
        section(md2, rawdir, c, "frr ip route", docker(c, "vtysh", "-c", "show ip route"))
        section(md2, rawdir, c, "bgp summary", docker(c, "vtysh", "-c", "show ip bgp summary"))
        section(md2, rawdir, c, "bgp ipv4 unicast", docker(c, "vtysh", "-c", "show bgp ipv4 unicast"))
        md2.append("## SD-WAN 自己的三个进程\n")
        for proc, url in (("sr-route", "http://127.0.0.1:%d/status" % ROUTE_PORT[n]),
                          ("sr-fib", "http://127.0.0.1:%d/status" % FIB_PORT[n]),
                          ("sr-frr", None)):
            if url:
                txt = docker(c, "curl", "-sf", url)
            else:
                txt = docker(c, "curl", "-sf", "--unix-socket", "/run/sr-frr/control.sock",
                             "http://unix/status")
            try:
                txt = json.dumps(json.loads(txt), ensure_ascii=False, indent=1)
            except ValueError:
                pass
            section(md2, rawdir, c, proc + " status", txt, "json")
        md2.append("## VPP 数据面\n")
        section(md2, rawdir, c, "vpp interface addr",
                docker(c, "vppctl", "show", "interface", "addr"))
        fibraw = docker(c, "vppctl", "show", "ip", "fib")
        section(md2, rawdir, c, "vpp ip fib 全文", fibraw)
        section(md2, rawdir, c, "vpp ip fib 摘要（客户前缀+overlay）",
                fib_digest(fibraw, set(LOOPS) | {t["local_overlay"] for t in
                          (json.load(open(os.path.join(RUN, "tunnels.json")))["by_node"].get(str(n)) or [])}))
        section(md2, rawdir, c, "vpp 接口计数器（wg/tap/host）",
                docker(c, "bash", "-lc",
                       "vppctl show interface | grep -E '^(wg|tap|host|local)' -A 9 | "
                       "awk '/^[wgta]/||/^host/{name=$1} /rx packets|tx packets|drops|punt/{print name\" \"$0}'"))
        section(md2, rawdir, c, "vpp bfd sessions", docker(c, "vppctl", "show", "bfd", "sessions"))
        section(md2, rawdir, c, "vrrp-probe -dump", vrrp_dump(c))
        md2.append("## agent 运行时\n")
        section(md2, rawdir, c, "agent /status",
                docker(c, "curl", "-sf", "http://127.0.0.1:8089/status"))
        section(md2, rawdir, c, "盘上三份配置的口径",
                docker(c, "bash", "-lc",
                       "python3 -c \"import json;"
                       "r=json.load(open('/etc/sdwan/route.json'));"
                       "f=json.load(open('/etc/sdwan/fib.json'));"
                       "s=json.load(open('/etc/sr-frr.json'));"
                       "print('route.json: rrs=%s tunnels=%d vrfs=%s' % "
                       "(json.dumps(r.get('rrs')), len(r.get('tunnels') or []), json.dumps(r.get('vrfs'))));"
                       "print('fib.json: vrfs=%s tunnels=%d' % (f.get('vrfs'), len(f.get('tunnels') or [])));"
                       "print('sr-frr.json: export_vrfs=%s import_rules=%s' % "
                       "(json.dumps(s.get('export_vrfs')), json.dumps(s.get('import_rules'), ensure_ascii=False)))\""))
        write(os.path.join(outdir, "cpe%d.md" % n), "\n".join(md2))

    # ---- 客户路由器 ----
    for key, (container, src) in CUSTS.items():
        md3 = ["# %s（客户自己的路由器，站点 %s）\n" % (container, key), ""]
        section(md3, rawdir, container, "ip -4 addr", docker(container, "ip", "-4", "addr", "show"))
        section(md3, rawdir, container, "ip route", docker(container, "ip", "route", "show"))
        section(md3, rawdir, container, "frr ip route",
                docker(container, "vtysh", "-c", "show ip route"))
        section(md3, rawdir, container, "ospf neighbor",
                docker(container, "vtysh", "-c", "show ip ospf neighbor"))
        section(md3, rawdir, container, "bgp ipv4 unicast",
                docker(container, "vtysh", "-c", "show bgp ipv4 unicast"))
        section(md3, rawdir, container, "frr running-config",
                docker(container, "vtysh", "-c", "show running-config"))
        write(os.path.join(outdir, "cust%s.md" % key), "\n".join(md3))

    write(os.path.join(outdir, "00-总览.md"), "\n".join(md))
    api.call("POST", "/api/logout", {})
    print("== 抓表完成：%s（%d 个文件）" % (outdir, len(os.listdir(outdir))))
    return outdir


if __name__ == "__main__":
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(2)
    capture(sys.argv[1], matrix="--no-matrix" not in sys.argv)
