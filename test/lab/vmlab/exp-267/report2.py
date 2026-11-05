#!/usr/bin/env python3
"""问题5 对照的第二把尺子：插件探针 rtt last=（纯数据面 CPE<->CPE，不经客户 VM），
   以及每块的宿主负载（判断臂间差是不是时段差）。"""
import os, re, glob, statistics

RAW = os.path.join(os.path.dirname(os.path.abspath(__file__)), 'raw')
last_re = re.compile(r'rtt last=([\d.]+)ms')
loss_re = re.compile(r'loss=([\d.]+)%')
load_re = re.compile(r'\{\s*([\d.]+)')


def q(vals, p):
    s = sorted(vals)
    return s[min(len(s) - 1, int(p / 100.0 * len(s)))]


def desc(vals):
    return 'n=%d avg=%.2f p50=%.2f p90=%.2f p99=%.2f max=%.2f sd=%.2f' % (
        len(vals), sum(vals) / len(vals), q(vals, 50), q(vals, 90), q(vals, 99),
        max(vals), statistics.pstdev(vals))


print('=== 探针 rtt last=（ms，纯数据面）按块 ===')
per = {}
for d in sorted(glob.glob(os.path.join(RAW, '*-*'))):
    tag = os.path.basename(d)
    arm = tag.split('-')[-1]
    load = [float(m.group(1)) for m in load_re.finditer(open(os.path.join(d, 'host.txt'), errors='replace').read())]
    print('%-24s host_load1 avg=%s max=%s' % (
        tag, '%.1f' % (sum(load) / len(load)) if load else '-', '%.1f' % max(load) if load else '-'))
    for f in sorted(glob.glob(os.path.join(d, 'probe-*.txt'))):
        box = os.path.basename(f)[6:-4]
        vals = [float(m.group(1)) for m in last_re.finditer(open(f, errors='replace').read())]
        if not vals:
            continue
        print('    %-5s %-5s %s' % (box, arm, desc(vals)))
        per.setdefault((arm, box), []).extend(vals)

print()
print('=== 探针 rtt last= 按臂合并 ===')
for (arm, box), vals in sorted(per.items()):
    print('%-4s %-5s %s' % (arm, box, desc(vals)))
