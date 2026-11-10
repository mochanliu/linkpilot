#!/usr/bin/env python3
"""analyze.py —— exp-286 单枪判读

用法: analyze.py <WORK> <TAG>

读 shot.sh 落的三份原始数据，输出：
  1) 被测盒采样里的**变化点**（只比字段，不比时间戳），以及相对「本盒 if 翻转」的毫秒差
  2) 注入盒采样里的 if/BFD 翻转
  3) 客户 ping 的丢包窗口（按 seq 推算，客户自己的钟）
  4) fib.log 里与这一枪同窗口的关键行

所有跨机数字只比**时长**：盒间时钟互差几十分钟（本轮实测 CPE3 比 CPE1 慢 48.2 分钟）。
"""
import sys, re, os

def load(path):
    rows = []
    if not os.path.exists(path):
        return rows
    for line in open(path, encoding='utf-8', errors='replace'):
        line = line.rstrip('\n')
        if not line:
            continue
        p = line.split('|')
        if len(p) < 8:
            continue
        rows.append({
            'ts': int(p[0]), 'len': p[1], 'prefs': p[2], 'res': p[3],
            'buckets': p[4], 'carry': p[5], 'bfd': p[6], 'if': p[7].replace('if=', '')
        })
    return rows

def transitions(rows, keys):
    out = []
    prev = None
    for r in rows:
        cur = tuple(r[k] for k in keys)
        if prev is not None and cur != prev:
            diff = [(k, prev[i], cur[i]) for i, k in enumerate(keys) if prev[i] != cur[i]]
            out.append((r, diff))
        prev = cur
    return out

def fmt_delta(ms):
    return f"{ms/1000:.3f}s" if abs(ms) >= 1000 else f"{ms}ms"

def analyze_probe(path, label):
    rows = load(path)
    print(f"\n== {label} 采样 {len(rows)} 点")
    if not rows:
        print("   （没有数据）")
        return
    keys = ['len', 'prefs', 'res', 'buckets', 'carry', 'bfd', 'if']
    base = rows[0]['ts']
    # 找本盒 if 翻转（down 的第一次）
    t_ifdown = None
    for r in rows:
        if r['if'] == 'down':
            t_ifdown = r['ts']
            break
    trs = transitions(rows, keys)
    print(f"   起手: len={rows[0]['len']} prefs={rows[0]['prefs']} res={rows[0]['res']} "
          f"buckets={rows[0]['buckets']} carry={rows[0]['carry']} | {rows[0]['bfd']} | if={rows[0]['if']}")
    if not trs:
        print("   全程无变化")
    for r, diff in trs:
        rel_base = r['ts'] - base
        rel_if = (r['ts'] - t_ifdown) if t_ifdown is not None else None
        s = ", ".join(f"{k}: {a}→{b}" for k, a, b in diff)
        tag = f"  Δif={fmt_delta(rel_if)}" if rel_if is not None else ""
        print(f"   +{fmt_delta(rel_base)}{tag}  {s}   [len={r['len']} prefs={r['prefs']} buckets={r['buckets']} carry={r['carry']} {r['bfd']}]")

def analyze_ping(path):
    print(f"\n== 客户 ping")
    if not os.path.exists(path):
        print("   （没有数据）")
        return
    txt = open(path, encoding='utf-8', errors='replace').read()
    seqs = []
    times = {}
    for m in re.finditer(r'\[(\d+\.\d+)\] .*icmp_seq=(\d+)', txt):
        times[int(m.group(2))] = float(m.group(1))
    for m in re.finditer(r'icmp_seq=(\d+)', txt):
        seqs.append(int(m.group(1)))
    lost = sorted(set(range(1, max(seqs) + 1)) - set(seqs)) if seqs else []
    summary = [l for l in txt.splitlines() if 'packet loss' in l or 'min/avg' in l]
    for l in summary:
        print("   " + l.strip())
    if lost:
        # 连续段
        runs = []
        start = prev = lost[0]
        for s in lost[1:]:
            if s == prev + 1:
                prev = s
            else:
                runs.append((start, prev)); start = prev = s
        runs.append((start, prev))
        print(f"   丢包段（seq）：" + ", ".join(f"{a}-{b}({b-a+1}发)" for a, b in runs))
        for a, b in runs:
            print(f"     seq{a}-{b}: 约 {(b-a+1)*50}ms 窗口（按 20发/秒计）")
    else:
        print("   无丢包")

def analyze_log(path, label, grep_pat):
    print(f"\n== {label} fib.log（筛 {grep_pat}）")
    if not os.path.exists(path):
        return
    lines = open(path, encoding='utf-8', errors='replace').read().splitlines()
    hits = [l for l in lines if re.search(grep_pat, l)]
    for l in hits[-12:]:
        print("   " + l.strip())

if __name__ == '__main__':
    work, tag = sys.argv[1], sys.argv[2]
    obs = [f for f in os.listdir(work) if f.startswith(tag + '-obs-') and f.endswith('.csv')]
    inj = [f for f in os.listdir(work) if f.startswith(tag + '-inj-') and f.endswith('.csv')]
    for f in obs:
        analyze_probe(os.path.join(work, f), f[:-4])
    for f in inj:
        analyze_probe(os.path.join(work, f), f[:-4])
    analyze_ping(os.path.join(work, tag + '-ping.txt'))
    for f in obs + inj:
        analyze_log(os.path.join(work, f.replace('.csv', '-fiblog.txt')), f[:-4], r'BFD|警告|收敛')
    if os.path.exists(os.path.join(work, tag + '-inject.txt')):
        t = open(os.path.join(work, tag + '-inject.txt')).read().split()
        print(f"\n== 注入时间（注入盒时钟） t0={t[0]} 命令返回={t[1]} t1={t[2]} 返回={t[3]}  保持={int(t[3])-int(t[0])}ms")
