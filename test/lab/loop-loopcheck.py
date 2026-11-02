#!/usr/bin/env python3
"""loop-loopcheck.py - 环路实验台的**防环证据抓取器**：一条命令看清"谁把前缀送给了谁"。

防环这件事光看一张表看不出来，必须把四样东西并排放：

    1. RR 的 Loc-RIB        —— 每个前缀由哪台设备宣告（宣告者 = 所有者就是正常的；
                                不是所有者还在宣告，就是回灌）
    2. 每台 CPE 的 sr-route  —— 本地 RIB 里每个前缀有几条路径、下一跳是谁
    3. 每台 CPE 的 VPP FIB   —— 真正转发用的桶数（buckets>1 = 多条路径负载分担，
                                其中一条若是"绕回对端"的路径，客户就会按桶丢包）
    4. 客户路由器 / 盒内 FRR —— 客户侧到底看到了几条、从哪两个下一跳看到

用法：
    python3 loop-loopcheck.py <标签> [前缀...]
    # 默认前缀 = 三个 loopback；输出写 run/loop/loopcheck/<标签>.md

只读，不改任何配置。
"""
import os
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
RUN = os.path.join(HERE, "run", "loop")
OUT = os.path.join(RUN, "loopcheck")

BOXES = (1, 2, 3, 4)
LOOPS = ("172.16.12.1", "172.16.3.1", "172.16.4.1")
CUSTS = {"12": "lp-cust12", "3": "lp-cust3", "4": "lp-cust4"}
# RR 的宣告表（与 loop-bringup.sh 里的 sr-rr -status 端口一致）
RR_ROUTES = "http://127.0.0.1:18447/routes"
PY = ("python3", "-c")


def docker(c, *cmd):
    r = subprocess.run(["docker", "exec", c] + list(cmd),
                       capture_output=True, text=True, timeout=60)
    return (r.stdout or "").replace("\r", "") or "（失败）%s" % (r.stderr or "").strip()[:300]


def vpp(c, *cmd):
    return docker(c, "vppctl", *cmd)


def http(c, url):
    return docker(c, *PY, "import urllib.request;print(urllib.request.urlopen('%s').read().decode())" % url)


def fib_paths(node, prefix):
    """从 `show ip fib table 0` 里切出一个前缀的转发块，抽出桶数与下一跳。"""
    text = vpp("lp-cpe%d" % node, "show", "ip", "fib", "table", "0")
    lines = text.split("\n")
    for i, ln in enumerate(lines):
        if ln.strip() == prefix + "/32":
            block = "\n".join(lines[i:i + 12])
            buckets = "?"
            for l in block.split("\n"):
                if "buckets:" in l:
                    buckets = l.split("buckets:")[1].split()[0].rstrip("]")
                    break
            hops = []
            for l in block.split("\n"):
                if "mpls via" in l:
                    hops.append(l.split("mpls via")[1].split()[0])
            return buckets, hops
    return "-", []


def rr_table():
    """RR 的 /routes 是 {设备号: [该设备宣告的前缀]}，正好回答"谁宣告了什么"。"""
    import json
    raw = http("lp-rr", RR_ROUTES)
    try:
        d = json.loads(raw)
    except ValueError:
        return None, "RR 路由口读不到：%s" % raw[:200]
    rows = []
    for dev, routes in d.items():
        for r in routes:
            rows.append((r.get("prefix", "?"), dev, ",".join(r.get("rts", []))))
    return d, rows


def main():
    if len(sys.argv) < 2:
        sys.stderr.write(__doc__)
        sys.exit(1)
    tag = sys.argv[1]
    prefixes = sys.argv[2:] or LOOPS
    os.makedirs(OUT, exist_ok=True)
    md = ["# 防环证据 %s" % tag, ""]
    md.append("抓取时间用 `date` 单独记录；本文件只反映抓的那一刻。")
    md.append("")

    d, rows = rr_table()
    md.append("## 1. RR 收到的宣告（谁把前缀交给了反射器）")
    md.append("")
    md.append("```text")
    if rows is None:
        md.append(str(d))
    else:
        for p, dev, rts in sorted(set(rows)):
            md.append("  %-18s 宣告者=device %-3s RT=%s" % (p, dev, rts))
        # 所有者 = 这块前缀物理上属于谁。site-12 的前缀由 1、2 两台共同代表（同一
        # 台客户路由器），那是正常的；3、4 的 loopback 只该由自己宣告。
        owners = {"172.16.12.1": (1, 2), "172.16.3.1": (3,), "172.16.4.1": (4,)}
        for p in prefixes:
            who = sorted({int(dev) for (pp, dev, _) in rows if pp.startswith(p)})
            own = owners.get(p, ())
            extra = sorted(set(who) - set(own))
            if who and extra:
                md.append("  ↑ %s：宣告者 %s，其中非所有者 %s = 从 SD-WAN 学来的又宣告回 SD-WAN（回灌）"
                          % (p, who, extra))
            elif who:
                md.append("  ✓ %s：只有所有者 %s 在宣告" % (p, who))
            else:
                md.append("  ！ %s：RR 上没有任何宣告" % p)
    md.append("```")
    md.append("")

    md.append("## 2. 每台向 SD-WAN 注入的本地前缀（sr-route 的 local 列表）")
    md.append("")
    md.append("这是环路的**入口**：本地前缀 = 盒内 FRR 交给 SD-WAN 的东西。若里面出现")
    md.append("了不属于本站点的前缀，就是「SD-WAN 学到的路由又被灌回 SD-WAN」。")
    md.append("")
    md.append("| 盒子 | local 前缀 | 导入规则(sr-frr import sources) |")
    md.append("|---|---|---|")
    import json
    for n in BOXES:
        raw = http("lp-cpe%d" % n, "http://127.0.0.1:%d/status" % (19000 + n))
        locals_ = "-"
        try:
            st = json.loads(raw)
            locals_ = ", ".join(r["prefix"] for r in st.get("local", [])) or "（空）"
        except ValueError:
            locals_ = "读不到：%s" % raw[:100]
        rules = "-"
        raw2 = http("lp-cpe%d" % n, "http://127.0.0.1:%d/status" % (19010 + n))
        try:
            f2 = json.loads(raw2)
            rules = json.dumps(f2.get("import_rules", f2.get("imports", "?")), ensure_ascii=False)
        except ValueError:
            rules = "读不到"
        md.append("| cpe%d | %s | %s |" % (n, locals_, rules[:120]))
    md.append("")

    md.append("## 3. 每台 CPE 的 RIB / FIB 对照（客户前缀走哪条路）")
    md.append("")
    md.append("| 盒子 | 前缀 | sr-route 路径来源(device) | 下一跳 | VPP buckets | FIB 下一跳 |")
    md.append("|---|---|---|---|---|---|")
    import json
    for n in BOXES:
        raw = http("lp-cpe%d" % n, "http://127.0.0.1:%d/status" % (19000 + n))
        try:
            st = json.loads(raw)
        except ValueError:
            md.append("| cpe%d | 读不到 sr-route：%s | | | | |" % (n, raw[:120]))
            continue
        rib = [p for v in st.get("rib", {}).values() for p in v]
        for pre in prefixes:
            hits = [p for p in rib if p["prefix"].startswith(pre)]
            for p in hits:
                md.append("| cpe%d | %s | %s | %s | %s | %s |" % (
                    n, p["prefix"], p["device_id"], ",".join(p["next_hops"]),
                    *fib_paths(n, pre)))
            if not hits:
                md.append("| cpe%d | %s/32 | （RIB 里没有） | - | %s | %s |" % (
                    n, pre, *fib_paths(n, pre)))
        md.append("| cpe%d | 防环计数 | own_route=%s invalid=%s no_path=%s rt_mismatch=%s | | | |" % (
            n, st.get("dropped_own_route"), st.get("dropped_invalid"),
            st.get("dropped_no_path"), st.get("dropped_rt_mismatch")))
    md.append("")

    md.append("## 4. 客户侧看到了什么")
    md.append("")
    for k, c in sorted(CUSTS.items()):
        md.append("### %s（客户路由器 cust%s）" % (c, k))
        md.append("")
        md.append("```text")
        md.append(docker(c, "vtysh", "-c", "show ip route").strip())
        md.append("```")
        md.append("")
    for n in BOXES:
        md.append("### lp-cpe%d 盒内 FRR 路由" % n)
        md.append("")
        md.append("```text")
        md.append(docker("lp-cpe%d" % n, "vtysh", "-c", "show ip route").strip())
        md.append("```")
        md.append("")

    md.append("## 5. 泄露方向与 sr-frr 的取舍")
    md.append("")
    for n in BOXES:
        raw = http("lp-cpe%d" % n, "http://127.0.0.1:%d/status" % (19010 + n))
        md.append("### cpe%d sr-frr status" % n)
        md.append("")
        md.append("```text")
        md.append(raw.strip()[:1600])
        md.append("```")
        md.append("")

    path = os.path.join(OUT, "%s.md" % tag)
    with open(path, "w") as f:
        f.write("\n".join(md) + "\n")
    print("== 防环证据写入 %s（%d 行）" % (path, len(md)))


if __name__ == "__main__":
    main()
