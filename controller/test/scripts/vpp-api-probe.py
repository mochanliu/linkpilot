#!/usr/bin/env python3
"""VPP 二进制 API 手工探针：注册握手 + ID 表 + 一次只读往返。

为什么需要它：定案 3 要求 agent 用自己的 X25519 私钥建 WG 接口，而 vppctl 没有这个
入口（`wireguard create` 只认 generate-key），只能走二进制 API。写 Go 客户端之前，
先向**真机**确认帧格式、注册消息、消息 ID 的来源；没有这一步，Go 里的每个常量都是猜测。

真机确认到的格式（不是抄文档，是这份脚本打印出来的）：
  · /run/vpp/api.sock 是 AF_UNIX + SOCK_STREAM；
  · 每条消息前置 16 字节头 struct ">QII" = (msgid 恒 0, length, flags)；
  · 注册消息 sockclnt_create 的 ID 是 VPP 保留的 15，回复 ID 16；
  · 回复负载 = ">HIIiIH" (msg_id, client_index, context, response, index, count)，
    紧接 count 条 (u16 index, u8 name[64])，name 形如 "show_version_51077d14"
    （即 名字_CRC32，CRC 变了就说明消息布局变了，客户端必须拒绝继续）；
  · 之后每条请求自己带 msg_id + client_index + context，client_index 用注册回复里的那个。

用法：docker cp 到 srwg-node1:/tmp，容器里 python3 /tmp/vpp-api-probe.py
"""
import socket
import struct
import sys

API_SOCK = "/run/vpp/api.sock"
SOCKCLNT_CREATE = 15
SOCKCLNT_CREATE_REPLY = 16
HDR = struct.Struct(">QII")
REPLY = struct.Struct(">HIIiIH")
ENTRY = struct.Struct(">H64s")


def recv_exact(sock, n):
    buf = b""
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            return None
        buf += chunk
    return buf


def send(sock, payload):
    sock.sendall(HDR.pack(0, len(payload), 0) + payload)


def recv_one(sock):
    h = recv_exact(sock, HDR.size)
    if not h:
        return None
    msgid, length, flags = HDR.unpack(h)
    return (msgid, length, flags), recv_exact(sock, length)


def main():
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(8)
    s.connect(API_SOCK)

    # --- 1. 注册 ---
    send(s, struct.pack(">HI", SOCKCLNT_CREATE, 124) + b"srwg-probe".ljust(64, b"\0"))
    (rmsgid, rlen, _), body = recv_one(s)
    mid, cidx, ctx, resp, idx, count = REPLY.unpack_from(body, 0)
    print("注册回复: body 前缀 hdr=(%d,%d) msg_id=%d client_index=%d context=%d" %
          (rmsgid, rlen, mid, cidx, ctx))
    print("  response=%d index=0x%08x count=%d" % (resp, idx, count))
    assert mid == SOCKCLNT_CREATE_REPLY, "回复 ID 不对"
    assert resp == 0, "注册被拒"
    n_ent = (len(body) - REPLY.size) // ENTRY.size
    if n_ent != count or REPLY.size + count * ENTRY.size != len(body):
        print("  条目数对不上：表里 %d 条 vs count=%d，总长 %d —— 布局假设错了" %
              (n_ent, count, len(body)))
        return 1

    table = {}
    p = REPLY.size
    for _ in range(count):
        e_idx, e_name = ENTRY.unpack_from(body, p)
        table[e_name.split(b"\0")[0].decode()] = e_idx
        p += ENTRY.size
    print("ID 表: %d 条，长度精确吻合（%d + %d*%d = %d）" %
          (count, REPLY.size, count, ENTRY.size, len(body)))
    for probe in ("show_version", "wireguard_interface_dump", "wireguard_interface_create",
                  "wireguard_interface_delete", "sr_wg_key_dump", "sr_wg_obs_dump",
                  "sr_wg_path_add_del"):
        hits = {k: v for k, v in table.items() if k.startswith(probe + "_")}
        print("  %-28s -> %s" % (probe, hits or "不在表里"))

    # --- 2. 只读往返：wireguard_interface_dump ---
    key = next((k for k in table if k.startswith("wireguard_interface_dump_")), None)
    if key is None:
        print("表里没有 wireguard_interface_dump（wireguard 插件没加载？）")
        return 1
    mid = table[key]
    # {u16 msg_id, u32 client_index, u32 context, bool show_private_key, u32 sw_if_index}
    send(s, struct.pack(">HII", mid, cidx, 1) + struct.pack(">BI", 1, 0xFFFFFFFF))
    n = 0
    while True:
        got = recv_one(s)
        if not got:
            print("没有回复：msg_id 或负载布局错了，VPP 会静默丢弃")
            return 1
        (_, rl, _), rb = got
        rid = struct.unpack_from(">H", rb, 0)[0]
        rname = next((k for k, v in table.items() if v == rid), "?id=%d" % rid)
        print("  回复 %d: len=%d msg_id=%d (%s) %s" %
              (n, rl, rid, rname, rb[:24].hex()))
        if rid == mid:
            break
        n += 1
        if n > 20:
            print("  超过 20 条，停")
            break
    print("结论：握手、ID 表、请求往返都以真机为准；Go 客户端可据此实现")
    s.close()
    return 0


sys.exit(main())
