#!/usr/bin/env python3
"""vpp-api-probe3.py —— 定位 wireguard_interface_create 的 retval=-31。

背景：今天早些时候同一个探针的第 4 节是 PASS（dump 回来的私钥逐字节等于装进去
的那把），现在却回 -31 = VNET_API_ERROR_INVALID_REGISTRATION，而且 node1 上多出
一个 wg0(idx=1)。这里把 create 的每个字段都当成变量逐个试，把"哪个字段导致 -31"
问出来；同时 dump 一帧 details 的原始字节，按头文件偏移做交叉核对。

用法（容器内）：python3 vpp-api-probe3.py [src_ip]
"""
import socket
import struct
import sys

HDR = struct.Struct(">QII")
REQ = struct.Struct(">HII")
REPLY16 = struct.Struct(">HIIiIH")
ENT = struct.Struct(">H64s")
AF_IP4 = 0


def v4addr(ip):
    return bytes([AF_IP4]) + socket.inet_aton(ip) + b"\0" * 12


class VPP:
    def __init__(self, path, name):
        self.s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.s.connect(path)
        self.buf = b""
        self.table = {}
        self.ci = self._register(name)

    def _send(self, payload):
        self.s.sendall(HDR.pack(0, len(payload), 0) + payload)

    def _read(self, timeout=3.0):
        self.s.settimeout(timeout)
        while True:
            if len(self.buf) >= 16:
                length = struct.unpack_from(">I", self.buf, 8)[0]
                if length and len(self.buf) >= 16 + length:
                    frame = self.buf[16:16 + length]
                    self.buf = self.buf[16 + length:]
                    return frame
            chunk = self.s.recv(65536)
            if not chunk:
                raise EOFError("VPP 关闭了连接")
            self.buf += chunk

    def _register(self, name):
        self._send(struct.pack(">HI", 15, 0x9999) + name.ljust(64, b"\0")[:64])
        f = self._read()
        mid, ci, ctx, resp, index, count = REPLY16.unpack_from(f, 0)
        assert mid == 16 and resp == 0, (mid, resp)
        off = 20
        for _ in range(count):
            mid_i, raw = ENT.unpack_from(f, off)
            off += 66
            nm = raw.split(b"\0")[0].decode()
            base, _, crc = nm.rpartition("_")
            if base:
                self.table[base] = (mid_i, crc)
        return index

    def call(self, name, fields, wait=True):
        mid = self.table[name][0]
        self._send(REQ.pack(mid, self.ci, 0x77) + fields)
        if not wait:
            return []
        got = []
        try:
            while True:
                got.append(self._read(timeout=1.5))
        except (socket.timeout, TimeoutError):
            pass
        except EOFError:
            pass
        return got

    def reply(self, name, fields):
        """返回 (retval, 原始应答帧)；应答没有 retval 时 retval=None。"""
        want = self.table.get(name + "_reply")
        for f in self.call(name, fields):
            if want and struct.unpack_from(">H", f, 0)[0] == want[0]:
                rv = struct.unpack_from(">i", f, 6)[0] if len(f) >= 10 else None
                return rv, f
        return None, None


def main():
    src = sys.argv[1] if len(sys.argv) > 1 else "10.10.1.2"
    v = VPP("/run/vpp/api.sock", b"srwg-probe3")
    print("client_index = 0x%x, table = %d 条" % (v.ci, len(v.table)))

    print("\n== 0. 现状：已存在的 WG 接口（原始 details 帧）")
    det = v.table.get("wireguard_interface_details")
    for f in v.call("wireguard_interface_dump", b"\1" + struct.pack(">I", 0xFFFFFFFF)):
        if det and struct.unpack_from(">H", f, 0)[0] == det[0]:
            print("   len=%d hex=%s" % (len(f), f.hex()))
            off = 6
            ui, sidx = struct.unpack_from(">II", f, off)
            port = struct.unpack_from(">H", f, off + 72)[0]
            af = f[off + 74]
            ip = ".".join(str(x) for x in f[off + 75:off + 79])
            print("   user_instance=%d sw_if_index=%d port=%d af=%d src=%s" % (ui, sidx, port, af, ip))

    print("\n== 1. create 变量扫描（port=51999，src=%s，静态私钥，generate_key=0）" % src)
    priv = bytes(range(32))
    variants = [
        ("instance=0 swif=~0", struct.pack(">II", 0, 0xFFFFFFFF)),
        ("instance=999 swif=~0", struct.pack(">II", 999, 0xFFFFFFFF)),
        ("instance=0 swif=0", struct.pack(">II", 0, 0)),
        ("instance=999 swif=~0 generate_key=1", struct.pack(">II", 999, 0xFFFFFFFF)),
    ]
    for label, head in variants:
        gk = b"\1" if "generate_key=1" in label else b"\0"
        body = head + priv + b"\0" * 32 + struct.pack(">H", 51999) + v4addr(src) + gk
        rv, f = v.reply("wireguard_interface_create", body)
        print("   %-40s body=%3d B retval=%s" % (label, len(body), rv))
        if rv == 0 and f:
            sidx = struct.unpack_from(">I", f, 10)[0]
            print("      -> 建起来了 sw_if_index=%d，顺手删掉" % sidx)
            print("      delete retval=%s" % (v.reply("wireguard_interface_delete", struct.pack(">I", sidx))[0],))

    print("\n== 2. 参照：CLI 路径能不能建（同样的参数）")
    print("   （见脚本外的 vppctl 对照）")

    print("\n== 3. sr_wg 自己的 rpc 仍然正常吗")
    print("   key_set retval   = %s" % (v.reply("sr_wg_key_set", struct.pack(">H", 99) + b"\x11" * 16)[0],))
    print("   stats_get retval = %s" % (v.reply("sr_wg_stats_get", b"")[0],))
    print("   key_del retval   = %s" % (v.reply("sr_wg_key_del", struct.pack(">H", 99))[0],))


if __name__ == "__main__":
    main()
