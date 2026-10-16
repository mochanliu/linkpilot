#!/usr/bin/env python3
"""
inject-sr-packet.py - 构造有效的 SR 封装 UDP 包并通过原始套接字发送。

用于在 WireGuard 握手不可用时验证 SR 数据面（transit / decap 路径）。
支持两种输出模式：
  1. --send: 通过 AF_PACKET 原始套接字直接发送（需要 root + 容器内执行）
  2. 默认: 写入 pcap 文件

用法:
  # 在容器内直接发送（推荐）:
  python3 inject-sr-packet.py --send --iface eth0 --mode transit \
      --src-ip 10.10.1.2 --dst-ip 10.10.1.3 --count 5

  # 生成 pcap 文件:
  python3 inject-sr-packet.py output.pcap --mode transit

模式:
  transit  - 生成发往中转节点 (port 51821) 的包，seg_left=1
  decap    - 生成发往终端节点 (port 51820) 的包，seg_left=0
"""
import struct
import hashlib
import hmac as hmac_mod
import sys
import os
import argparse

# ==================== SR Header 常量 ====================
SR_MAGIC = 0x53
SR_VERSION = 0x01
SR_VER_FLAGS = (SR_VERSION << 4) | 0x00
SR_FIXED_HDR_LEN = 16
SR_HMAC_LEN = 8
SR_SEG_SIZE = 6
SR_MAX_SEGS = 8
SR_ACTION_FORWARD = 0x00

SR_PORT_VPN = 51820
SR_PORT_TRANSIT = 51821

DEFAULT_KEY_HEX = "00112233445566778899aabbccddeeff"


def sr_hdr_len(seg_count):
    return SR_FIXED_HDR_LEN + seg_count * SR_SEG_SIZE + SR_HMAC_LEN


def parse_ip(s):
    """解析点分十进制 IPv4 -> 4 bytes (网络序)"""
    parts = [int(x) for x in s.split('.')]
    return struct.pack('!4B', *parts)


def parse_port(p):
    """端口号 -> 2 bytes (网络序)"""
    return struct.pack('!H', int(p))


def parse_segment(s):
    """解析 'ip:port' -> 6 bytes"""
    ip_str, port_str = s.rsplit(':', 1)
    return parse_ip(ip_str) + parse_port(port_str)


def compute_hmac(header_bytes, seg_count, key_bytes):
    """
    计算 SR Header 的 HMAC-SHA256（截断 8 字节）。
    覆盖范围：ver_flags(1) || hdr_len(1) || seg_count(1) || reserved(2) ||
              hmac_id(2) || path_id(2) || timestamp(4) || segments[0..N-1](N*6)
    排除：magic(offset 0), seg_left(offset 3), action(offset 5)
    """
    # header_bytes 是完整的 SR header（不含 HMAC 部分，或 HMAC 为零）
    # 按字段提取覆盖范围
    ver_flags = header_bytes[1:2]
    hdr_len = header_bytes[2:3]
    seg_count_b = header_bytes[4:5]
    reserved = header_bytes[6:8]
    hmac_id = header_bytes[8:10]
    path_id = header_bytes[10:12]
    timestamp = header_bytes[12:16]
    segments = header_bytes[16:16 + seg_count * SR_SEG_SIZE]

    msg = ver_flags + hdr_len + seg_count_b + reserved + hmac_id + path_id + timestamp + segments

    digest = hmac_mod.new(key_bytes, msg, hashlib.sha256).digest()
    return digest[:SR_HMAC_LEN]


def build_sr_header(seg_count, seg_left, hmac_id, path_id, timestamp, segments, key_bytes):
    """
    构建完整的 SR Header（含 HMAC）。
    segments: list of 6-byte tuples (ip+port in network order)
    返回: bytes of length sr_hdr_len(seg_count)
    """
    hdr_len = sr_hdr_len(seg_count)

    # 固定部分 (8 bytes)
    h = bytearray(hdr_len)
    h[0] = SR_MAGIC
    h[1] = SR_VER_FLAGS
    h[2] = hdr_len & 0xFF  # hdr_len fits in u8 (max 72)
    h[3] = seg_left
    h[4] = seg_count
    h[5] = SR_ACTION_FORWARD
    h[6] = 0  # reserved
    h[7] = 0  # reserved

    # 可扩展部分 (8 bytes)
    struct.pack_into('!H', h, 8, hmac_id)
    struct.pack_into('!H', h, 10, path_id)
    struct.pack_into('!I', h, 12, timestamp)

    # 段列表
    for i, seg in enumerate(segments):
        offset = 16 + i * SR_SEG_SIZE
        h[offset:offset + SR_SEG_SIZE] = seg

    # 计算 HMAC（先置零）
    hmac_offset = 16 + seg_count * SR_SEG_SIZE
    h[hmac_offset:hmac_offset + SR_HMAC_LEN] = b'\x00' * SR_HMAC_LEN

    mac = compute_hmac(bytes(h), seg_count, key_bytes)
    h[hmac_offset:hmac_offset + SR_HMAC_LEN] = mac

    return bytes(h)


def build_ip4(src_ip, dst_ip, payload_len, protocol=17, ttl=64):
    """构建 IPv4 头（20 bytes，无选项），自动计算 checksum"""
    version_ihl = (4 << 4) | 5
    tos = 0
    total_len = 20 + payload_len
    ident = 0x1234
    flags_frag = 0x4000  # Don't Fragment
    checksum = 0  # 先置零

    hdr = struct.pack('!BBHHHBBH4s4s',
                      version_ihl, tos, total_len, ident, flags_frag,
                      ttl, protocol, checksum,
                      parse_ip(src_ip), parse_ip(dst_ip))

    # 计算 checksum
    cksum = ip_checksum(hdr)
    hdr = hdr[:10] + struct.pack('!H', cksum) + hdr[12:]
    return hdr


def ip_checksum(data):
    """计算 IPv4 头校验和"""
    if len(data) % 2:
        data += b'\x00'
    s = 0
    for i in range(0, len(data), 2):
        w = (data[i] << 8) + data[i + 1]
        s += w
    while s >> 16:
        s = (s & 0xFFFF) + (s >> 16)
    return ~s & 0xFFFF


def build_udp(src_port, dst_port, payload_len):
    """构建 UDP 头（8 bytes），checksum=0（合法 for IPv4）"""
    length = 8 + payload_len
    return struct.pack('!HHHH', src_port, dst_port, length, 0)


def build_ethernet(src_mac, dst_mac, ethertype=0x0800):
    """构建 Ethernet 头（14 bytes）"""
    def mac_bytes(m):
        return bytes([int(x, 16) for x in m.split(':')])
    return mac_bytes(dst_mac) + mac_bytes(src_mac) + struct.pack('!H', ethertype)


def write_pcap(filename, packets):
    """写入 pcap 文件（linktype=1, Ethernet）"""
    with open(filename, 'wb') as f:
        # Global header: magic, ver 2.4, tz=0, sigfigs=0, snaplen=65535, linktype=1
        f.write(struct.pack('<IHHiIII', 0xa1b2c3d4, 2, 4, 0, 0, 65535, 1))
        for pkt in packets:
            # Packet header: ts_sec, ts_usec, incl_len, orig_len
            f.write(struct.pack('<IIII', 0, 0, len(pkt), len(pkt)))
            f.write(pkt)


def detect_macs(iface, dst_ip):
    """自动检测本机 MAC 和目标 MAC（通过 ARP 表或 fcntl）"""
    import socket
    import struct as st
    import fcntl

    # 获取本机接口 MAC
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        info = fcntl.ioctl(s.fileno(), 0x8927,  # SIOCGIFHWADDR
                           struct.pack('256s', iface[:15].encode('utf-8')))
        src_mac = ':'.join(f'{b:02x}' for b in info[18:24])
        s.close()
    except Exception:
        src_mac = '02:42:0a:0a:01:02'

    # 尝试从 ARP 表获取目标 MAC
    dst_mac = None
    try:
        with open('/proc/net/arp', 'r') as f:
            for line in f:
                parts = line.split()
                if len(parts) >= 4 and parts[0] == dst_ip and parts[3] != '00:00:00:00:00:00':
                    dst_mac = parts[3]
                    break
    except Exception:
        pass

    # 如果 ARP 表没有，尝试 ping 触发 ARP 解析
    if not dst_mac:
        try:
            import subprocess
            subprocess.run(['arping', '-c', '1', '-I', iface, dst_ip],
                          capture_output=True, timeout=3)
            with open('/proc/net/arp', 'r') as f:
                for line in f:
                    parts = line.split()
                    if len(parts) >= 4 and parts[0] == dst_ip and parts[3] != '00:00:00:00:00:00':
                        dst_mac = parts[3]
                        break
        except Exception:
            pass

    if not dst_mac:
        # 最后回退：从 Docker 网络的 IP 推断 MAC（02:42:xx:xx:xx:xx 格式）
        octets = dst_ip.split('.')
        dst_mac = '02:42:' + ':'.join(f'{int(o):02x}' for o in octets)

    return (src_mac, dst_mac)


def send_raw(iface, packets, delay=0.2):
    """通过 AF_PACKET 原始套接字发送以太网帧"""
    import socket
    import time
    try:
        s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW)
        s.bind((iface, 0))
    except PermissionError:
        print("ERROR: need root for AF_PACKET raw socket", file=sys.stderr)
        sys.exit(1)
    except OSError as e:
        print(f"ERROR: cannot open AF_PACKET on {iface}: {e}", file=sys.stderr)
        sys.exit(1)

    sent = 0
    for pkt in packets:
        s.send(pkt)
        sent += 1
        time.sleep(delay)
    s.close()
    return sent


def main():
    parser = argparse.ArgumentParser(description='Craft and send SR-WG test packets')
    parser.add_argument('output', nargs='?', default=None, help='Output pcap file (omit with --send)')
    parser.add_argument('--send', action='store_true',
                        help='Send via AF_PACKET raw socket (requires root)')
    parser.add_argument('--iface', default='eth0',
                        help='Interface for --send mode (default: eth0)')
    parser.add_argument('--delay', type=float, default=0.2,
                        help='Delay between packets in seconds (default: 0.2)')
    parser.add_argument('--mode', choices=['transit', 'decap', 'custom'],
                        default='transit', help='Packet mode')
    parser.add_argument('--src-ip', default='10.10.1.2')
    parser.add_argument('--dst-ip', default='10.10.1.3')
    parser.add_argument('--dst-port', type=int, default=None)
    parser.add_argument('--src-port', type=int, default=12345)
    parser.add_argument('--key-hex', default=DEFAULT_KEY_HEX)
    parser.add_argument('--hmac-id', type=int, default=1)
    parser.add_argument('--path-id', type=int, default=1)
    parser.add_argument('--timestamp', type=int, default=1700000000)
    parser.add_argument('--seg-count', type=int, default=2)
    parser.add_argument('--seg-left', type=int, default=None)
    parser.add_argument('--segments', default='10.10.2.2:51820,10.10.1.3:51821',
                        help='Comma-separated ip:port list (seg[0] first)')
    parser.add_argument('--src-mac', default=None,
                        help='Source MAC (auto-detected from iface if omitted)')
    parser.add_argument('--dst-mac', default=None,
                        help='Dest MAC (auto-detected via ARP if omitted)')
    parser.add_argument('--count', type=int, default=5,
                        help='Number of packets to generate')
    parser.add_argument('--payload-size', type=int, default=64,
                        help='Dummy payload size (simulates WG transport)')

    args = parser.parse_args()

    if not args.send and not args.output:
        parser.error("either provide an output pcap filename or use --send")

    key_bytes = bytes.fromhex(args.key_hex)
    if len(key_bytes) != 16:
        print(f"ERROR: key must be 32 hex chars (16 bytes), got {len(key_bytes)}", file=sys.stderr)
        sys.exit(1)

    # 解析 segments
    seg_list = [parse_segment(s.strip()) for s in args.segments.split(',')]
    seg_count = args.seg_count
    if len(seg_list) != seg_count:
        print(f"ERROR: --seg-count={seg_count} but got {len(seg_list)} segments", file=sys.stderr)
        sys.exit(1)

    # 确定 dst_port 和 seg_left
    if args.mode == 'transit':
        dst_port = args.dst_port or SR_PORT_TRANSIT
        seg_left = args.seg_left if args.seg_left is not None else 1
    elif args.mode == 'decap':
        dst_port = args.dst_port or SR_PORT_VPN
        seg_left = args.seg_left if args.seg_left is not None else 0
    else:
        dst_port = args.dst_port or SR_PORT_TRANSIT
        seg_left = args.seg_left if args.seg_left is not None else 1

    # 自动检测 MAC 地址
    src_mac = args.src_mac
    dst_mac = args.dst_mac
    if args.send and (not src_mac or not dst_mac):
        detected = detect_macs(args.iface, args.dst_ip)
        src_mac = src_mac or detected[0]
        dst_mac = dst_mac or detected[1]
    src_mac = src_mac or '02:42:0a:0a:01:02'
    dst_mac = dst_mac or '02:42:0a:0a:01:03'

    # 构建包
    packets = []
    for i in range(args.count):
        ts = args.timestamp + i

        # SR Header
        sr_hdr = build_sr_header(seg_count, seg_left, args.hmac_id,
                                 args.path_id, ts, seg_list, key_bytes)

        # Dummy payload (模拟 WG transport header + 加密数据)
        # WG transport 首字节 = 4 (type=transport-data), 不等于 SR_MAGIC(0x53)
        payload = b'\x04' + os.urandom(args.payload_size - 1)

        # UDP
        udp_payload_len = len(sr_hdr) + len(payload)
        udp_hdr = build_udp(args.src_port + i, dst_port, udp_payload_len)

        # IP4
        ip_payload_len = len(udp_hdr) + udp_payload_len
        ip_hdr = build_ip4(args.src_ip, args.dst_ip, ip_payload_len)

        # Ethernet
        eth_hdr = build_ethernet(src_mac, dst_mac)

        pkt = eth_hdr + ip_hdr + udp_hdr + sr_hdr + payload
        packets.append(pkt)

    if args.send:
        sent = send_raw(args.iface, packets, args.delay)
        print(f"Sent {sent} packets via {args.iface}")
    else:
        write_pcap(args.output, packets)
        print(f"Written {len(packets)} packets to {args.output}")

    print(f"  mode={args.mode} dst={args.dst_ip}:{dst_port} seg_left={seg_left} "
          f"seg_count={seg_count} sr_hdr_len={sr_hdr_len(seg_count)}")
    print(f"  HMAC key={args.key_hex} hmac_id={args.hmac_id} path_id={args.path_id}")
    print(f"  segments: {args.segments}")
    print(f"  src_mac={src_mac} dst_mac={dst_mac}")


if __name__ == '__main__':
    main()
