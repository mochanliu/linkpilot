#!/usr/bin/env python3
# ---------------------------------------------------------------------------
# 运营商重标记模拟器：内联 L2 跳（tap 用户态桥）
#
#   wanbr1{a-wan, m0}  <--mangler-->  wanbr2{t-wan1, m1}     A <-> T 方向
#   wanbr3{t-wan2, m2} <--mangler-->  wanbr4{b-wan, m3}      T <-> B 方向
#
# 为什么不用 tc pedit：pedit 改外层 ToS 不修正 IPv4 校验和，VPP 的
# ip4-input 会以 bad ip4 checksum 丢包（实测 40/40 全丢）。本进程在
# 用户态重写 ToS 并重算校验和，与真实运营商重标记行为一致。
#
# 重写规则（模式文件 /tmp/mangler-mode，1=Phase1，2=Phase1+2）：
#   m0->m1 且 UDP dst 51821（A->T 的 SR 包）：ToS -> 0x00      （Phase 1 起）
#   m2->m3 且 UDP dst 20000（T->B）：        ToS -> 0x00      （Phase 2 起）
# 其余帧（ARP/握手/ICMP）原样转发。
# ---------------------------------------------------------------------------
import os, struct, fcntl, select

TUNSETIFF = 0x400454ca
IFF_TAP, IFF_NO_PI = 0x0002, 0x1000
MODE_FILE = "/tmp/mangler-mode"

def open_tap(name):
    fd = os.open("/dev/net/tun", os.O_RDWR)
    fcntl.ioctl(fd, TUNSETIFF, struct.pack("16sH", name.encode(), IFF_TAP | IFF_NO_PI))
    return fd

def ip_csum(hdr):
    if len(hdr) % 2:
        hdr += b"\x00"
    s = 0
    for i in range(0, len(hdr), 2):
        s += (hdr[i] << 8) | hdr[i + 1]
    while s >> 16:
        s = (s & 0xffff) + (s >> 16)
    return (~s) & 0xffff

def rewrite_tos(frame, new_tos):
    ihl = (frame[14] & 0x0F) * 4
    if ihl != 20:
        return frame            # 带 IP 选项的外层头不存在（本隧道不产生），防御性跳过
    f = bytearray(frame)
    f[15] = new_tos
    f[24:26] = b"\x00\x00"
    f[24:26] = struct.pack("!H", ip_csum(bytes(f[14:34])))
    return bytes(f)

def classify(tap, frame):
    if len(frame) < 38 or frame[12:14] != b"\x08\x00":
        return None
    dport = (frame[36] << 8) | frame[37]
    mode = MODE[0]
    if tap == "m0" and dport == 51821 and mode >= 1:
        return rewrite_tos(frame, 0x00)
    if tap == "m2" and dport == 20000 and mode >= 2:
        return rewrite_tos(frame, 0x00)
    return None

MODE = [1]
last_mtime = -1
fds = {t: open_tap(t) for t in ("m0", "m1", "m2", "m3")}
peer = {"m0": "m1", "m1": "m0", "m2": "m3", "m3": "m2"}
by_fd = {fds[t]: t for t in fds}
print("mangler up: m0<->m1 (A<->T)  m2<->m3 (T<->B)", flush=True)

while True:
    try:
        m = os.stat(MODE_FILE).st_mtime
        if m != last_mtime:
            last_mtime = m
            with open(MODE_FILE) as fh:
                MODE[0] = int(fh.read().strip() or "1")
            print("mode ->", MODE[0], flush=True)
    except FileNotFoundError:
        pass
    r, _, _ = select.select(list(fds.values()), [], [], 1.0)
    for fd in r:
        tap = by_fd[fd]
        try:
            frame = os.read(fd, 65535)
        except OSError:
            continue
        nf = classify(tap, frame)
        try:
            os.write(fds[peer[tap]], nf if nf is not None else frame)
        except OSError:
            pass
