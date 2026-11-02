#!/usr/bin/env python3
"""loop-probe.py - 环路实验台的**客户视角探针**：只测连通与中断窗口，不动任何配置。

两种模式：

    loop-probe.py snap  <标签> [--n 20] [--i 1.0]
        六条环回对环回的流**逐条**打 N 个包，输出一张丢包/时延表。
        用在每个场景抓表之后，作为「客户到底掉没掉」的量化口径。

    loop-probe.py watch <标签> <秒数> [--i 0.2]
        六条流**同时**连续打，打完算每条流丢了哪几段序号（= 客户可见的中断窗口）。
        用在故障注入的前后：先起 watch，再注入，让它把整个窗口录下来。
        序号是每条流自己的 seq，所以窗口 = (丢失段末尾 seq - 起始 seq) × 发包间隔。

为什么区分两种：snap 是安静口径（一次只有一条流在跑，容器不会被自己压出丢包），
用来做场景间的对照；watch 是加压口径，只用来量中断时长。混用会把「并发探测本身
造成的丢包」记到故障头上。

结果写 test/lab/run/loop/probe/<标签>.md（外加 .json 存原始 seq，报告里可复核）。
"""
import json
import os
import re
import subprocess
import sys
import threading
import time

HERE = os.path.dirname(os.path.abspath(__file__))
PROBE = os.path.join(HERE, "run", "loop", "probe")

# 六条流：源容器、源 loopback、目的 loopback。环路站点的客户侧默认路由指向 VRRP 虚地址，
# 所以 cust12 的出方向会因 MASTER 是谁而变；172.16.12.1 的入方向则由 cpe1/cpe2 谁的
# K 路由/OSPF 在生效决定 —— 这正是 HA 场景要量的东西。
FLOWS = [
    ("lp-cust12", "172.16.12.1", "172.16.3.1"),
    ("lp-cust12", "172.16.12.1", "172.16.4.1"),
    ("lp-cust3", "172.16.3.1", "172.16.12.1"),
    ("lp-cust3", "172.16.3.1", "172.16.4.1"),
    ("lp-cust4", "172.16.4.1", "172.16.12.1"),
    ("lp-cust4", "172.16.4.1", "172.16.3.1"),
]


def name(c, src, dst):
    return "%s→%s" % (src, dst)


def ping(c, src, dst, n, interval):
    r = subprocess.run(["docker", "exec", c, "ping", "-I", src, "-c", str(n),
                        "-i", str(interval), "-W", "2", dst],
                       capture_output=True, text=True,
                       timeout=int(n) * float(interval) + 60)
    out = (r.stdout or "") + (r.stderr or "")
    seqs = [int(m) for m in re.findall(r"seq=(\d+)", out)]
    loss = re.search(r"(\d+) packets transmitted, (\d+) packets? received, (\d+)% packet loss", out)
    rtt = re.search(r"= [\d.]+/([\d.]+)/", out)
    return {"transmitted": int(loss.group(1)) if loss else n,
            "received": int(loss.group(2)) if loss else len(seqs),
            "loss_pct": int(loss.group(3)) if loss else None,
            "rtt_avg_ms": float(rtt.group(1)) if rtt else None,
            "seqs": sorted(seqs),
            "raw": out}


def lost_ranges(seqs, total):
    """把缺掉的 seq 归并成连续段，返回 [(起, 止), ...]（含端点）。"""
    got = set(seqs)
    out, start = [], None
    for s in range(total):
        if s not in got:
            if start is None:
                start = s
        else:
            if start is not None:
                out.append((start, s - 1))
            start = None
    if start is not None:
        out.append((start, total - 1))
    return out


def snap(tag, n=20, interval=1.0):
    os.makedirs(PROBE, exist_ok=True)
    rows, detail = [], {}
    for c, src, dst in FLOWS:
        d = ping(c, src, dst, n, interval)
        detail[name(c, src, dst)] = d
        rows.append("| %s | %d/%d | %s%% | %s | %s |" % (
            name(c, src, dst), d["received"], d["transmitted"],
            d["loss_pct"], ("%.3f" % d["rtt_avg_ms"]) if d["rtt_avg_ms"] else "未上报",
            "、".join("%d-%d" % (a, b) if a != b else str(a)
                     for a, b in lost_ranges(d["seqs"], d["transmitted"])) or "-"))
        time.sleep(0.5)
    md = ["# 探针 snap %s\n" % tag, "时间：%s；每条流 %d 个包、间隔 %ss（逐条，安静口径）\n"
          % (time.strftime("%F %T"), n, interval), "",
          "| 流 | 收到/发出 | 丢包 | RTT avg(ms) | 丢失 seq 段 |",
          "|---|---|---|---|---|"] + rows + [""]
    write(tag, md, {k: {kk: vv for kk, vv in v.items() if kk != "raw"}
                    for k, v in detail.items()},
            {k: v["raw"] for k, v in detail.items()})
    print("\n".join(md))


def watch(tag, secs, interval=0.2):
    os.makedirs(PROBE, exist_ok=True)
    n = max(1, int(secs / interval))
    res, lock = {}, threading.Lock()

    def run(c, src, dst):
        d = ping(c, src, dst, n, interval)
        with lock:
            res["%s→%s" % (src, dst)] = d

    ts = [threading.Thread(target=run, args=f) for f in FLOWS]
    t0 = time.time()
    for t in ts:
        t.start()
    for t in ts:
        t.join()
    rows = []
    for k, d in res.items():
        rngs = lost_ranges(d["seqs"], d["transmitted"])
        # 一段连续丢失 = 一个中断窗口；窗口时长 = (段长) × 间隔，再加上边界误差一个间隔
        wins = "、".join("%.1fs@seq%d-%d" % ((b - a + 1) * interval, a, b) for a, b in rngs) or "无"
        rows.append("| %s | %d/%d | %s%% | %s |" % (k, d["received"], d["transmitted"],
                                                    d["loss_pct"], wins))
    md = ["# 探针 watch %s\n" % tag,
          "时间：%s；持续 %ss、六条流同时打、间隔 %ss（加压口径，只用来量窗口）\n"
          % (time.strftime("%F %T"), secs, interval), "",
          "| 流 | 收到/发出 | 丢包 | 中断窗口（时长@seq 段） |",
          "|---|---|---|---|"] + rows + [""]
    write(tag, md, {k: {kk: vv for kk, vv in v.items() if kk != "raw"} for k, v in res.items()},
            {k: v["raw"] for k, v in res.items()})
    print("\n".join(md))
    print("（探测起点 unix %.3f，用于和注入时刻对齐）" % t0)


def write(tag, md, summary, raw):
    with open(os.path.join(PROBE, tag + ".md"), "w") as f:
        f.write("\n".join(md))
    with open(os.path.join(PROBE, tag + ".json"), "w") as f:
        json.dump(summary, f, ensure_ascii=False, indent=1)
    with open(os.path.join(PROBE, tag + ".raw.txt"), "w") as f:
        for k, v in raw.items():
            f.write("==== %s\n%s\n" % (k, v))


if __name__ == "__main__":
    a = sys.argv[1:]
    if len(a) < 2:
        print(__doc__)
        sys.exit(2)
    mode, tag = a[0], a[1]
    extra = [x for x in a[2:] if not x.startswith("--")]
    n = next((int(x.split("=")[1]) for x in a if x.startswith("--n=")), 20)
    iv = next((float(x.split("=")[1]) for x in a if x.startswith("--i=")), None)
    if mode == "snap":
        snap(tag, n, iv if iv else 1.0)
    elif mode == "watch":
        watch(tag, int(extra[0]), iv if iv else 0.2)
    else:
        print("未知模式：%s（snap|watch）" % mode)
        sys.exit(2)
