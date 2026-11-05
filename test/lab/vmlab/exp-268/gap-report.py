#!/usr/bin/env python3
"""把 ping-probe.sh 的输出切成「客户可见的中断窗口」，并对齐到 gate 的相位。

两处口径上的坑，这一版都按实测改掉了：

1. 时间轴不在一个基准上。探针跑在客户 VM 上、判据日志跑在 CPE 盒子上，
   两边时钟能差好几小时（实测 cust4 比 Mac 快 28677s）。所以探针文件里的
   epoch 必须先减掉 clock-anchor.sh 量到的偏移，才能和 run.out 的相位对齐。
2. 「相邻两条 OK 的间隔」量不出黑洞。路由被摘掉时内核立刻回
   Destination Host Unreachable，那条 LOSS 只花几十毫秒，OK 之间的间隔一直很小
   —— 上一版就是这么报出「没有超过 0.5s 的中断：全程连续」，而同一时刻
   客户前缀一条都不在表里。真正的中断 = 连续 LOSS 的这一段，
   从第一条 LOSS 到它后面第一条 OK。

用法：gap-report.py --anchor raw/clock.txt --events raw/run.out \\
          raw/probe-cust4.out=cust4=cust4→cust3 raw/probe-cust3.out=cust3=cust3→cust4
      anchor 文件每行 `<主机> <该主机 epoch - Mac epoch 的秒数>`，由 clock-anchor.sh 生成；
      第二个字段是这台探针所在的主机名（用来查偏移），第三个字段才是显示用的标签。
      盒子自己那一行用 --box-anchor（默认主机名 box，写的是 CPE 的 ssh 别名）。
"""
import argparse
import calendar
import datetime
import re
import sys


def load(path):
    rows = []
    for ln in open(path):
        f = ln.split()
        if len(f) < 2 or not f[0][0].isdigit():
            continue
        rows.append((float(f[0]), f[1], (f[2] if len(f) > 2 else '')))
    return rows


def hms(t):
    return datetime.datetime.fromtimestamp(t).strftime('%m-%d %H:%M:%S')


def read_anchor(path):
    a = {}
    if not path:
        return a
    for ln in open(path):
        f = ln.split()
        if len(f) >= 2:
            a[f[0]] = int(float(f[1]))
    return a


def read_events(path, box_offset):
    """run.out 里的 [YYYY-MM-DD HH:MM:SS] 是盒子自己显示的时钟。

    盒子 TZ=UTC，所以把字面按 UTC 解释就是盒子自己的 epoch；
    再减掉 (盒子 epoch - Mac epoch) 就抬到 Mac 的时间轴上。
    """
    ev = []
    if not path:
        return ev
    for ln in open(path):
        m = re.match(r'\[(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d)\] (== .*|阶段.*|第 \d+s.*|判据.*)', ln)
        if m:
            t = datetime.datetime.strptime(m.group(1), '%Y-%m-%d %H:%M:%S')
            ev.append((calendar.timegm(t.timetuple()) - box_offset, m.group(2).strip()))
    return ev


def report(rows, label, events, thresh):
    total = len(rows)
    loss_rows = [r for r in rows if r[1] == 'LOSS']
    loss = len(loss_rows)
    print(f'\n### {label}  样本 {total} 条，LOSS {loss} 条（丢包率 {100.0*loss/max(total,1):.1f}%）')
    if not rows:
        print('    空的')
        return
    print(f'    跨度 {hms(rows[0][0])} → {hms(rows[-1][0])}')
    rtts = sorted(float(r[2].replace('ms', '')) for r in rows if r[1] == 'OK' and r[2])
    if rtts:
        print(f'    通的时候 rtt min/中位/max = {rtts[0]:.2f}/{rtts[len(rtts)//2]:.2f}/{rtts[-1]:.2f} ms')
    # 连续 LOSS 归成一个中断窗口，窗口结束于其后第一条 OK
    windows = []
    i = 0
    while i < len(rows):
        if rows[i][1] != 'LOSS':
            i += 1
            continue
        start = rows[i][0]
        n = 0
        while i < len(rows) and rows[i][1] == 'LOSS':
            n += 1
            i += 1
        end = rows[i][0] if i < len(rows) else rows[-1][0]
        windows.append((start, end, end - start, n, i >= len(rows)))
    windows = [w for w in windows if w[2] >= thresh or w[4]]
    if not windows:
        print(f'    没有 >= {thresh}s 的中断窗口（零散 LOSS {loss} 条不计）')
        return
    print(f'    >= {thresh}s 的中断窗口 {len(windows)} 个（按时长降序，前 8 个）：')
    for a, b, d, n, runny in sorted(windows, key=lambda w: -w[2])[:8]:
        tail = '  <-- 到探针结束仍未恢复' if runny else ''
        print(f'      {hms(a)} → {hms(b)}  中断 {d:.1f}s（{n} 条 LOSS）{tail}')
        for t, name in events:
            if a - 3 <= t <= b + 3:
                print(f'          落在相位：{hms(t)}  {name}')


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--anchor', help='clock-anchor.sh 的输出')
    ap.add_argument('--events', help='gate 的 run.out，用来标注相位')
    ap.add_argument('--box-anchor', default='box', help='anchor 文件里盒子那行的主机名')
    ap.add_argument('--thresh', type=float, default=0.5, help='中断窗口计时门槛（秒）')
    ap.add_argument('specs', nargs='*', help='<探针文件>=<标签>')
    a = ap.parse_args()

    anchors = read_anchor(a.anchor)
    if a.anchor and not anchors:
        print('!! anchor 文件是空的 —— 时间轴没有对齐，下面的时刻都别当真', file=sys.stderr)
    box_off = anchors.get(a.box_anchor, 0)
    events = read_events(a.events, box_off)
    if events:
        print('### gate 相位（已对齐到 Mac 时间轴）')
        for t, name in events[:40]:
            print(f'    {hms(t)}  {name}')
    for spec in a.specs:
        path, _, rest = spec.partition('=')
        host, _, label = rest.partition('=')
        off = anchors.get(host, 0)
        if not anchors:
            print(f'\n(没有 anchor：{path} 按探针自己的时钟报，不能和 gate 相位对齐)', file=sys.stderr)
        rows = [(t - off, r, x) for t, r, x in load(path)]
        report(rows, label or host or path, events, a.thresh)


if __name__ == '__main__':
    main()
