#!/usr/bin/env python3
"""loop-edit.py - 环路实验台的**改配置器**：只走控制台，留一条审计。

场景注入要改隧道/VRRP/逃生开关，但改法必须和租户在浏览器里点的一样：同一个登录、
同一批 /api/* 端点、同一个租户账号。所以这里不拼 curl、不碰设备文件，直接复用
loop-console.py 的 Api 会话（含请求日志 run/loop/console-log.jsonl，口令/入网码
已在写入前打星）。

用法：
    loop-edit.py tunnels                     # 列隧道（id、两端、颜色、状态）
    loop-edit.py GET  /api/overlay
    loop-edit.py PUT  /api/config?node=1 @/tmp/cfg1.json
    loop-edit.py POST /api/config/publish '{"node_id":1}'
    loop-edit.py DEL  '/api/underlay/tunnels?id=t-xxxx'

DELETE 用 DEL 是因为 GET/PUT/POST/DELETE 里只有 DELETE 会撞 shell 的关键字习惯；
路径要带引号，查询串里的 & 会被 shell 截断。
"""
import importlib.util
import json
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location(
    "loopconsole", os.path.join(HERE, "loop-console.py"))
lc = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(lc)


def die(msg):
    sys.stderr.write("FATAL: %s\n" % msg)
    sys.exit(1)


def api():
    creds = lc.read_creds()
    a = lc.Api()
    a.login_tenant(creds["username"], creds["password"])
    return a


def body_of(arg):
    if arg is None:
        return None
    if arg.startswith("@"):
        with open(arg[1:]) as f:
            return json.load(f)
    return json.loads(arg)


def cmd_tunnels(api):
    u = api.call("GET", "/api/underlay")
    print("%-8s %-9s %-6s %-6s %-6s %s" % ("id", "两端", "颜色", "mtu", "状态", "监听端口"))
    for t in u.get("tunnels", []):
        a, b = t.get("a", {}), t.get("b", {})
        print("%-8s %-9s %-6s %-6s %-6s %s" % (
            t.get("id", "?"),
            "%d-%d" % (a.get("node_id", 0), b.get("node_id", 0)),
            t.get("color", "?"), t.get("mtu", "?"), t.get("state", "?"),
            "%s / %s" % (a.get("port"), b.get("port"))))
    print("共 %d 条" % len(u.get("tunnels", [])))


def main():
    if len(sys.argv) < 2:
        die(__doc__)
    cmd = sys.argv[1]
    a = api()
    try:
        if cmd == "tunnels":
            cmd_tunnels(a)
            return
        if cmd == "DEL":
            doc = a.call("DELETE", sys.argv[2])
        else:
            method = cmd.upper()
            if method not in ("GET", "PUT", "POST"):
                die("只支持 GET/PUT/POST/DEL/tunnels：%s" % cmd)
            path = sys.argv[2]
            doc = a.call(method, path, body_of(sys.argv[3] if len(sys.argv) > 3 else None))
        json.dump(doc, sys.stdout, ensure_ascii=False, indent=1)
        print()
    finally:
        a.call("POST", "/api/logout", {})


if __name__ == "__main__":
    main()
