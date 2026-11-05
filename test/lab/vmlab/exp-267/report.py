#!/usr/bin/env python3
"""汇总 exp-267/raw/*-* 各块：每条路径的样本数、丢包、min/avg/p50/p90/p99/max、mdev。
   按臂聚合（同臂多块合并）并给出每块一行，便于看时段漂移。"""
import os, re, glob, statistics, sys
from collections import defaultdict

RAW = os.path.join(os.path.dirname(os.path.abspath(__file__)), 'raw')
pkt = re.compile(r'time=(\d+(?:\.\d+)?)\s*ms')
def mean(v):
    return sum(v)/len(v)

summ = re.compile(r'(\d+) packets transmitted, (\d+) received,?\s*([\d.]+)% packet loss')

blocks = []
for d in sorted(glob.glob(os.path.join(RAW, '*-*'))):
    tag = os.path.basename(d)
    arm = tag.split('-')[-1]
    row = {'tag': tag, 'arm': arm, 'paths': {}}
    for f in sorted(glob.glob(os.path.join(d, 'ping-*.txt'))):
        name = os.path.basename(f)[5:-4]
        txt = open(f, errors='replace').read()
        ts = [float(m.group(1)) for m in pkt.finditer(txt)]
        s = summ.search(txt)
        if not ts:
            row['paths'][name] = None
            continue
        ts_s = sorted(ts)
        q = lambda p: ts_s[min(len(ts_s)-1, int(p/100.0*len(ts_s)))]
        row['paths'][name] = {
            'n': len(ts),
            'loss': float(s.group(3)) if s else None,
            'min': ts_s[0], 'avg': mean(ts), 'med': q(50),
            'p90': q(90), 'p99': q(99), 'max': ts_s[-1],
            'stdev': statistics.pstdev(ts) if len(ts) > 1 else 0.0,
        }
    blocks.append(row)

paths = sorted({p for b in blocks for p in b['paths']})
if not blocks:
    print('没有块数据'); sys.exit(1)

print('=== 每块（单位 ms）===')
hdr = f"{'块':<28}{'路径':<10}{'n':>5}{'loss%':>7}{'min':>8}{'avg':>8}{'p50':>8}{'p90':>8}{'p99':>8}{'max':>9}{'sd':>8}"
print(hdr)
for b in blocks:
    for p in paths:
        v = b['paths'].get(p)
        if not v:
            print(f"{b['tag']:<28}{p:<10}  无样本"); continue
        print(f"{b['tag']:<28}{p:<10}{v['n']:>5}{v['loss'] if v['loss'] is not None else -1:>7.1f}"
              f"{v['min']:>8.2f}{v['avg']:>8.2f}{v['med']:>8.2f}{v['p90']:>8.2f}{v['p99']:>8.2f}{v['max']:>9.2f}{v['stdev']:>8.2f}")

print()
print('=== 按臂合并（同臂所有块的全部样本）===')
print(f"{'臂':<6}{'路径':<10}{'n':>6}{'avg':>8}{'p50':>8}{'p90':>8}{'p99':>8}{'max':>9}{'sd':>8}{'avg差(vs 首臂)':>18}")
arms = []
for b in blocks:
    if b['arm'] not in arms:
        arms.append(b['arm'])
for p in paths:
    pool = {}
    for a in arms:
        vals = [float(m.group(1)) for b in blocks if b['arm'] == a for f in glob.glob(os.path.join(RAW, b['tag'], f'ping-{p}.txt'))
                for m in pkt.finditer(open(f, errors='replace').read())]
        if vals:
            pool[a] = sorted(vals)
    base = arms[0] if arms and arms[0] in pool else None
    for a in arms:
        if a not in pool:
            continue
        ts_s = pool[a]
        q = lambda pr: ts_s[min(len(ts_s)-1, int(pr/100.0*len(ts_s)))]
        avg = mean(ts_s)
        d = f"{avg - mean(pool[base]):>+8.2f} ms" if base and base in pool else ''
        print(f"{a:<6}{p:<10}{len(ts_s):>6}{avg:>8.2f}{q(50):>8.2f}{q(90):>8.2f}{q(99):>8.2f}{ts_s[-1]:>9.2f}"
              f"{statistics.pstdev(ts_s):>8.2f}{d:>18}")
    print()
