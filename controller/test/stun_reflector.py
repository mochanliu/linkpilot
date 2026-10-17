#!/usr/bin/env python3
"""stun_reflector.py -- a minimal RFC 5389 STUN binding reflector, for the
vBOND acceptance harness only.
搬运自 develop 的 src/control/test/stun_reflector.py，报文编码逐字节未改。

It answers Binding Requests with a Binding Success Response carrying
XOR-MAPPED-ADDRESS, and -- crucially for the harness's central assertion --
logs EVERY observed source tuple with the transaction id, so the harness can
prove that the mapping the discover tool REPORTED equals the mapping the
reflector OBSERVED. "Discovery works" is an equality between two independent
witnesses, not the absence of an error.

Usage: stun_reflector.py [port]      (default 3478)
Log:   /tmp/stun-observed.log  one JSON object per datagram seen.
"""
import json
import socket
import struct
import sys
import time

MAGIC = 0x2112A442
XPORT_MASK = MAGIC >> 16  # X-Port = port XOR most significant 16 bits


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 3478
    log = open("/tmp/stun-observed.log", "a", buffering=1)
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("0.0.0.0", port))
    print(f"stun_reflector: up on udp/{port}", flush=True)

    while True:
        data, addr = s.recvfrom(4096)
        txid = data[8:20].hex() if len(data) >= 20 else ""
        log.write(json.dumps({
            "ts": time.time(),
            "src": addr[0],
            "port": addr[1],
            "txid": txid,
        }) + "\n")

        if len(data) < 20 or data[0:2] != b"\x00\x01":
            continue  # not a Binding Request; logged, not answered

        xport = addr[1] ^ XPORT_MASK
        xip = struct.unpack("!I", socket.inet_aton(addr[0]))[0] ^ MAGIC

        resp = bytearray(data[:20])
        resp[0:2] = b"\x01\x01"                       # Binding Success
        resp[2:4] = struct.pack("!H", 12)             # one 8-byte attribute + pad
        resp += struct.pack("!HH", 0x0020, 8)         # XOR-MAPPED-ADDRESS
        resp += struct.pack("!BBHI", 0, 0x01, xport, xip)
        s.sendto(bytes(resp), addr)


if __name__ == "__main__":
    main()
