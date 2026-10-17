#!/usr/bin/env python3
"""VPP binary-API probe, part 2: are *plugin-owned* messages reachable on api.sock,
and can a WireGuard interface be created with a **static private key** over it?

Part 1 (vpp-api-probe.py) established the framing and the name->id table
handshake. It appeared that plugin messages (wireguard_*, sr_wg_*) were silent
while core messages (show_version, ip_route_dump) answered. That conclusion was
an observation artifact: every silent case was a *_dump, and a VPP dump with an
empty result set sends **no reply at all** - there is no end-of-dump marker.
The node under test had 0 WireGuard interfaces, 0 p-keys and 0 observations.

This probe therefore uses plugin messages that carry an explicit reply, then
re-runs a dump whose table is now non-empty:

  sr_wg_stats_get   -> sr_wg_stats_get_reply     (read-only)
  sr_wg_key_set     -> autoreply                 (installs hmac_id 99)
  sr_wg_key_dump    -> stream sr_wg_key_details  (must now return 1 entry)
  sr_wg_key_del     -> autoreply                 (back to empty)
  wireguard_interface_create  (generate_key=0 + private_key[32])  <- 定案 3
  wireguard_interface_dump    (show_private_key=1, must return 1 entry)
  wireguard_interface_delete  (cleanup; port 51999 keeps 51820/51821 conventions free)

Wire structs (all __attribute__((packed)), read from the installed headers):
  vl_api_wireguard_interface_t = u32 user_instance, u32 sw_if_index,
      u8 private_key[32], u8 public_key[32], u16 port, vl_api_address_t src_ip
  vl_api_address_t             = u8 af (ADDRESS_IP4 = 0), union{ ip4[4], ip6[16] }
                               -> 1 + 16 = 17 bytes, NOT 8
  create = hdr(10) + 91 + bool generate_key      = 102
  dump   = hdr(10) + bool show_private_key + u32 = 15
  delete = hdr(10) + u32 sw_if_index             = 14
  create_reply = msg_id(2) + context(4) + i32 retval(4) + u32 sw_if_index(4) = 14

Run inside a VPP container: python3 vpp-api-probe2.py [src-ip]
"""
import random
import socket
import struct
import sys
import time

SOCK = "/run/vpp/api.sock"
HDR = struct.Struct(">QII")          # outer frame: msg_id (always 0), length, flags
REQ = struct.Struct(">HII")          # message header: msg_id, client_index, context
REPLY16 = struct.Struct(">HIIiIH")   # msg_id, client_index, context, response, index, count
ENT = struct.Struct(">H64s")         # table entry: id, "name_<crc>"
WGP = 51999                          # throwaway listen port, see docstring
AF_IP4 = 0

PASS = 0
FAIL = 0


def ok(msg):
    global PASS
    PASS += 1
    print("   [PASS] %s" % msg)


def bad(msg):
    global FAIL
    FAIL += 1
    print("   [FAIL] %s" % msg)


def send(s, payload):
    s.sendall(HDR.pack(0, len(payload), 0) + payload)


def recv_frames(s, timeout):
    """Read whatever arrives before the timeout, split on the 16-byte frame header."""
    s.settimeout(timeout)
    buf = b""
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            chunk = s.recv(65536)
        except socket.timeout:
            break
        if not chunk:
            break
        buf += chunk
    frames, off = [], 0
    while len(buf) - off >= 16:
        _, ln, _ = HDR.unpack_from(buf, off)
        if ln == 0 or len(buf) - off - 16 < ln:
            break
        frames.append(buf[off + 16: off + 16 + ln])
        off += 16 + ln
    return frames


def v4addr(ip):
    return bytes([AF_IP4]) + socket.inet_aton(ip) + b"\0" * 12   # 17 bytes


def main():
    src = sys.argv[1] if len(sys.argv) > 1 else "10.10.1.2"
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.connect(SOCK)
    send(s, struct.pack(">HI", 15, 0x1234) + b"srwg-probe2".ljust(64, b"\0"))
    frames = recv_frames(s, 3.0)
    if not frames:
        bad("no sockclnt_create_reply")
        return 1
    body = frames[0]
    mid, cli_idx0, ctx, resp, index, count = REPLY16.unpack_from(body, 0)
    if mid != 16 or resp != 0:
        bad("registration reply: msg_id=%d response=%d" % (mid, resp))
        return 1
    if len(body) != 20 + count * 66:
        bad("message table short: %d != %d" % (len(body), 20 + count * 66))
        return 1
    table = {}
    for i in range(count):
        idx, nm = ENT.unpack_from(body, 20 + i * 66)
        table.setdefault(nm.split(b"\0")[0].decode().rsplit("_", 1)[0], idx)
    print("registered: client index 0x%x, %d messages" % (index, count))
    ci = index

    def call(label, name, fields=b"", timeout=2.0):
        """Send `name`, return every frame that came back (may be 0 for an empty dump)."""
        mid = table.get(name)
        if mid is None:
            bad("%s: %s not in message table" % (label, name))
            return []
        send(s, REQ.pack(mid, ci, 0x55) + fields)
        got = recv_frames(s, timeout)
        for f in got:
            rid = struct.unpack_from(">H", f, 0)[0]
            rname = [k for k, v in table.items() if v == rid]
            print("   %-20s id=%-5d -> %-30s %d B %s" % (
                label, mid, rname[:1], len(f),
                "" if len(f) < 10 else "retval=%d" % struct.unpack_from(">i", f, 6)[0]))
        return got

    def expect(label, name, fields=b"", timeout=2.0):
        got = call(label, name, fields, timeout)
        want = table.get(name + "_reply")
        hit = [f for f in got if want is not None and struct.unpack_from(">H", f, 0)[0] == want]
        if hit:
            ok("%s got %s" % (label, name + "_reply"))
        else:
            bad("%s: no %s_reply among %d frame(s)" % (label, name, len(got)))
        return got

    print("\n== 1. plugin messages with an explicit reply (not a dump)")
    expect("stats_get", "sr_wg_stats_get")
    priv = bytes(random.getrandbits(8) for _ in range(32))
    expect("key_set(99)", "sr_wg_key_set", struct.pack(">H", 99) + priv)

    print("\n== 2. the same dump that was silent before, table now non-empty")
    got = call("key_dump", "sr_wg_key_dump")
    det = table.get("sr_wg_key_details")
    n = len([f for f in got if struct.unpack_from(">H", f, 0)[0] == det])
    (ok if n == 1 else bad)("sr_wg_key_details count = %d (want 1)" % n)

    print("\n== 3. other plugin dumps, still legitimately empty on this node")
    call("config_dump", "sr_wg_config_dump")
    got = call("obs_dump", "sr_wg_obs_dump")
    print("   obs_dump returned %d frame(s); 0 is expected when the table is empty" % len(got))

    print("\n== 4. WireGuard interface with a STATIC private key (定案 3)")
    iface = struct.pack(">II", 0, 0xFFFFFFFF) + priv + b"\0" * 32 + struct.pack(">H", WGP) + v4addr(src)
    create = iface + b"\0"          # generate_key = false
    print("   interface struct = %d B, create message = %d B" % (len(iface), 10 + len(create)))
    got = expect("wg_create", "wireguard_interface_create", create)
    swif = None
    want = table.get("wireguard_interface_create_reply")
    for f in got:
        if want is not None and struct.unpack_from(">H", f, 0)[0] == want:
            rv, swif = struct.unpack_from(">iI", f, 6)
            print("   create_reply retval=%d sw_if_index=%d" % (rv, swif))
            if rv != 0:
                bad("create failed with retval=%d" % rv)

    got = call("wg_dump", "wireguard_interface_dump", b"\1" + struct.pack(">I", 0xFFFFFFFF))
    det = table.get("wireguard_interface_details")
    n = len([f for f in got if struct.unpack_from(">H", f, 0)[0] == det])
    (ok if n >= 1 else bad)("wireguard_interface_details count = %d (want >=1)" % n)
    for f in got:
        # vl_api_wireguard_interface_details = msg_id(2) + context(4) + interface(91) = 97
        if det is not None and struct.unpack_from(">H", f, 0)[0] == det and len(f) >= 97:
            off = 2 + 4                       # msg_id, context
            uinst, sidx = struct.unpack_from(">II", f, off)
            shown_priv = f[off + 8: off + 40]
            pub = f[off + 40: off + 72]
            port = struct.unpack_from(">H", f, off + 72)[0]
            src = socket.inet_ntoa(f[off + 75: off + 79])
            print("   if idx=%d port=%d src=%s pub=%s…" % (sidx, port, src, pub[:8].hex()))
            (ok if shown_priv == priv else bad)(
                "dumped private_key == what we installed (%s…)" % shown_priv[:4].hex())

    if swif is not None:
        expect("wg_delete", "wireguard_interface_delete", struct.pack(">I", swif))
    else:
        bad("cannot clean up: no sw_if_index from create_reply")

    print("\n== 5. cleanup sr_wg key")
    expect("key_del(99)", "sr_wg_key_del", struct.pack(">H", 99))
    det = table.get("sr_wg_key_details")
    got = call("key_dump2", "sr_wg_key_dump")
    n = len([f for f in got if struct.unpack_from(">H", f, 0)[0] == det])
    (ok if n == 0 else bad)("after key_del: %d detail(s), 0 expected (empty dump is silent)" % n)
    s.close()
    print("\n%d passed, %d failed" % (PASS, FAIL))
    return 0 if FAIL == 0 else 1


sys.exit(main())
