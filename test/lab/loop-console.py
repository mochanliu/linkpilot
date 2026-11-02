#!/usr/bin/env python3
"""loop-console.py - 环路实验台的所有配置动作**只走控制台**。

这个脚本不发一条 vppctl、不写一个 /etc/frr/frr.conf、不碰 route.json 的路由内容。
它只做浏览器会做的事：登录后 PUT/POST 那些 /api/* 端点，字段与控制台页面逐一对应。
所以它跑出来的每一份配置都是「从控制器上操作」的结果，报告里可以按步骤复述。

用法（顺序就是环路的搭建顺序，中间夹着 loop-bringup.sh 的阶段）：
    loop-console.py setup         # 运营方建租户 + 授权；租户自己生成入网码
    (bringup --stage enroll)      # 4 台盒子拿码入网
    (bringup --stage base)        # 盒子本地 bootstrap（管道字段）
    loop-console.py rrs           # RR 库 + 由控制台签发 RR 身份（证书私钥只在响应里出现一次）
    (bringup --stage rr)          # 装身份、起 sr-rr
    loop-console.py underlay      # 全局配置 + 发布全局配置（发完之后才能建隧道）
    loop-console.py sites         # 站点：site-12 放 1、2 两台；site-3、site-4 各一台
    loop-console.py tunnels       # 批量：full-mesh(1,3,4) 然后 hub-spoke(hub=2, nodes=2,3,4)
    loop-console.py config        # 每台的基础配置（客户口/VPN/OSPF/BGP/VRRP），只存草稿
    loop-console.py overlay       # 导通组 + 每台 RR 绑定 + prefix-list + 路由泄露
    loop-console.py publish       # 逐台发布（overlay/RR/泄露都是这一份包的第 4 段）
    loop-console.py status        # 打一张版本/在线/下发回执的表
    loop-console.py dump <path>   # 直接读一个 GET 端点，便于抓证据

口令、入网码、RR 私钥写到 test/lab/run/loop/（.gitignore 覆盖），权限 0600；
请求日志 run/loop/console-log.jsonl 里这些字段一律打星。
"""
import hashlib
import json
import os
import re
import secrets
import sys
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
RUN = os.path.join(HERE, "run", "loop")
CONSOLE = os.environ.get("LOOP_CONSOLE", "http://127.0.0.1:8089")

TENANT = "tenant-loop"
TENANT_NAME = "环路实验租户"
OPS_TOKEN_FILE = os.path.join(RUN, "console-token")
CRED_FILE = os.path.join(RUN, "credentials.json")
CLAIM_FILE = os.path.join(RUN, "claim.txt")
LOG_FILE = os.path.join(RUN, "console-log.jsonl")

# ---- 拓扑常量（与 loop-bringup.sh 的地址表一致） ----
SITE_VIP = "10.76.12.10"
LCP = "lcp-x0"
BOX = {
    1: dict(serial="lp-cpe1", cust="10.76.12.11", gw=None, wan="10.88.2.11",
            mgmt="10.99.2.11", proto="ospf", vpn=0, area=0, prio=150,
            cust_if_hint="site12"),
    2: dict(serial="lp-cpe2", cust="10.76.12.12", gw=None, wan="10.88.2.12",
            mgmt="10.99.2.12", proto="ospf", vpn=0, area=0, prio=100,
            cust_if_hint="site12"),
    3: dict(serial="lp-cpe3", cust="10.76.3.2", gw="10.76.3.3", wan="10.88.2.13",
            mgmt="10.99.2.13", proto="bgp", vpn=0, asn=65103, peer_as=65003,
            cust_if_hint="xconn3"),
    4: dict(serial="lp-cpe4", cust="10.76.4.2", gw="10.76.4.3", wan="10.88.2.14",
            mgmt="10.99.2.14", proto="bgp", vpn=0, asn=65104, peer_as=65004,
            cust_if_hint="xconn4"),
}
RR_NAME = "环路 RR"
RR_ADDR = "10.99.2.100:18446"
RR_LOCATION = "本地"
CUSTOMER_AGG = "172.16.0.0/16"   # 客户侧 loopback 的汇聚前缀，prefix-list 用它


def log(msg):
    print("== %s" % msg, flush=True)


def note(msg):
    print("   %s" % msg, flush=True)


def die(msg):
    print("FATAL: %s" % msg, file=sys.stderr, flush=True)
    sys.exit(2)


SECRET_KEYS = {"password", "old_password", "new_password", "code", "key_pem",
               "cert_pem", "ca_pem", "token", "claim_code"}
CODE_RE = re.compile(r"sr_[0-9A-Z]{32}")


def redact(obj):
    """递归地把口令/明文码/PEM 换成摘要，日志里不能出现可用凭据。"""
    if isinstance(obj, dict):
        out = {}
        for k, v in obj.items():
            if k in SECRET_KEYS and isinstance(v, str) and v:
                out[k] = "<redacted sha256:%s len=%d>" % (
                    hashlib.sha256(v.encode()).hexdigest()[:8], len(v))
            else:
                out[k] = redact(v)
        return out
    if isinstance(obj, list):
        return [redact(v) for v in obj]
    if isinstance(obj, str) and CODE_RE.search(obj):
        return CODE_RE.sub("<redacted-code>", obj)
    return obj


def append_log(rec):
    os.makedirs(RUN, exist_ok=True)
    rec["ts"] = round(time.time(), 3)
    with open(LOG_FILE, "a") as f:
        f.write(json.dumps(redact(rec), ensure_ascii=False) + "\n")


class Api:
    def __init__(self):
        self.cookie = None
        self.who = None

    def call(self, method, path, body=None, want_status=None):
        url = CONSOLE + path
        data = None
        headers = {}
        if body is not None:
            data = json.dumps(body).encode()
            # POST 没有 application/json 会被 415 挡掉（CSRF 面的唯一门槛）。
            headers["Content-Type"] = "application/json"
        if self.cookie:
            headers["Cookie"] = self.cookie
        req = urllib.request.Request(url, data=data, headers=headers, method=method)
        t0 = time.time()
        try:
            with urllib.request.urlopen(req, timeout=30) as r:
                raw = r.read().decode()
                status = r.status
                setc = r.headers.get("Set-Cookie")
        except urllib.error.HTTPError as e:
            raw = e.read().decode()
            status = e.code
            setc = e.headers.get("Set-Cookie")
        except urllib.error.URLError as e:
            append_log({"method": method, "path": path, "error": str(e.reason)})
            die("%s %s 连不上：%s" % (method, path, e.reason))
        if setc:
            self.cookie = setc.split(";")[0]
        try:
            doc = json.loads(raw) if raw else {}
        except ValueError:
            doc = {"_raw": raw[:400]}
        append_log({"method": method, "path": path, "body": body,
                    "status": status, "ms": round((time.time() - t0) * 1000, 1),
                    "resp_keys": sorted(doc.keys()) if isinstance(doc, dict) else None})
        if want_status is not None and status != want_status:
            die("%s %s -> %d：%s" % (method, path, status,
                                     json.dumps(doc, ensure_ascii=False)[:600]))
        if status >= 400:
            die("%s %s -> %d：%s" % (method, path, status,
                                     json.dumps(doc, ensure_ascii=False)[:600]))
        return doc

    def login_ops(self):
        token = open(OPS_TOKEN_FILE).read().strip()
        self.call("POST", "/api/login", {"token": token})
        s = self.call("GET", "/api/session")
        self.who = s.get("role")
        if s.get("role") != "operator":
            die("运营方登录后角色不是 operator：%s" % json.dumps(s))
        return s

    def login_tenant(self, username, password):
        self.call("POST", "/api/login", {"username": username, "password": password})
        s = self.call("GET", "/api/session")
        self.who = s.get("role")
        if s.get("role") != "tenant" or s.get("tenant_id") != TENANT:
            die("租户登录后身份不对：%s" % json.dumps(s))
        return s


def gen_password(n=16):
    alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789"
    return "".join(secrets.choice(alphabet) for _ in range(n))


def read_creds():
    with open(CRED_FILE) as f:
        return json.load(f)


# ---------------------------------------------------------------- 步骤
def cmd_setup(api):
    os.makedirs(RUN, exist_ok=True)
    api.login_ops()
    log("setup: 运营方会话 OK")
    log("setup: 运营方建租户（同时下发第一张授权 = 名称 + 名额，不含码）")
    creds = {}
    pw = gen_password()
    doc = api.call("POST", "/api/tenants", {
        "tenant_id": TENANT, "name": TENANT_NAME,
        "account": "loop-tenant", "password": pw, "claim_max_uses": 4,
    })
    creds["tenant_id"] = TENANT
    creds["username"] = doc["account"]["username"]
    creds["password"] = doc.get("password") or pw
    creds["claim_id"] = doc["claim"]["id"]
    with open(CRED_FILE, "w") as f:
        json.dump(creds, f, ensure_ascii=False, indent=1)
    os.chmod(CRED_FILE, 0o600)
    note("租户账号 %s（口令见 run/loop/credentials.json 与本行输出：%s）"
         % (creds["username"], creds["password"]))

    api.call("POST", "/api/logout", {})
    api.cookie = None
    api.login_tenant(creds["username"], creds["password"])
    log("setup: 租户自己给这张授权生成入网码（运营方不持码）")
    code = api.call("POST", "/api/claims/code", {"id": creds["claim_id"], "ttl_sec": 3600})
    with open(CLAIM_FILE, "w") as f:
        f.write(code["code"])
    os.chmod(CLAIM_FILE, 0o600)
    note("码写入 run/loop/claim.txt（有效期到 %s）" %
         time.strftime("%H:%M:%S", time.localtime(code["expires_at_unix"])))
    api.call("POST", "/api/logout", {})


def cmd_rrs(api):
    creds = read_creds()
    api.login_tenant(creds["username"], creds["password"])
    log("rrs: 建 RR 库条目并由控制台签发身份")
    doc = api.call("POST", "/api/rrs",
                   {"name": RR_NAME, "addr": RR_ADDR, "location": RR_LOCATION})
    row = doc.get("rr") or (doc.get("rrs") or [None])[-1]
    if row is None:
        die("建 RR 没拿到行：%s" % json.dumps(doc)[:400])
    rid = row["id"]
    note("RR 行 id=%s addr=%s" % (rid, row["addr"]))
    ident = api.call("POST", "/api/rrs/identity", {"id": rid})
    for field, path in (("cert_pem", "rr.crt"), ("key_pem", "rr.key"), ("ca_pem", "ca.pem")):
        if not ident.get(field):
            die("签发响应里没有 %s" % field)
        with open(os.path.join(RUN, path), "w") as f:
            f.write(ident[field])
        os.chmod(os.path.join(RUN, path), 0o600)
    note("RR 身份 CN=%s 指纹=%s，SAN=%s（sr-route 用 tls_server_name=%s 校验）"
         % (ident.get("rr_id"), (ident.get("fingerprint") or "")[:16],
            RR_ADDR.split(":")[0], RR_ADDR.split(":")[0]))
    with open(os.path.join(RUN, "rr-id"), "w") as f:
        f.write(rid)
    api.call("POST", "/api/logout", {})


def cmd_sites(api):
    creds = read_creds()
    api.login_tenant(creds["username"], creds["password"])
    log("sites: 站点 12 是一台客户路由器接两台盒子，3/4 各一台")
    want = [("环路站点 12（两台盒子）", 116.4, 39.9, [1, 2]),
            ("环路站点 3", 121.5, 31.2, [3]),
            ("环路站点 4", 113.3, 23.1, [4])]
    by_name = {s["name"]: s for s in api.call("GET", "/api/underlay")["sites"]}
    for name, lon, lat, nodes in want:
        # 站点 id 由控制器分配（POST 不许带 id），按名字找已有行，保持可重跑。
        row = by_name.get(name)
        if row is None:
            row = api.call("POST", "/api/underlay/sites",
                           {"name": name, "lon": lon, "lat": lat})["site"]
            note("建站点 %s = %s" % (row["id"], name))
        else:
            note("站点 %s 已存在（%s）" % (row["id"], name))
        for n in nodes:
            api.call("PUT", "/api/underlay/node-site", {"node_id": n, "site_id": row["id"]})
            note("node %d → %s" % (n, row["id"]))
    api.call("POST", "/api/logout", {})


def cmd_underlay(api):
    creds = read_creds()
    api.login_tenant(creds["username"], creds["password"])
    log("underlay: 写全局配置草稿，然后发布全局配置（发布之后才能建隧道）")
    cur = api.call("GET", "/api/underlay")
    st = cur.get("settings") or {}
    body = {
        "vpn_ports": st.get("vpn_ports") or {"min": 20000, "max": 20999},
        "transit_ports": st.get("transit_ports") or {"min": 15000, "max": 15999},
        "overlay_cidr": st.get("overlay_cidr") or "10.250.0.0/16",
        "probe_interval_ms": st.get("probe_interval_ms") or 300,
        "bfd_min_tx_ms": st.get("bfd_min_tx_ms") or 300,
        "bfd_min_rx_ms": st.get("bfd_min_rx_ms") or 300,
        "bfd_detect_mult": st.get("bfd_detect_mult") or 3,
    }
    api.call("PUT", "/api/underlay/settings", body)
    note("草稿：%s" % json.dumps(body, ensure_ascii=False))
    pub = api.call("POST", "/api/underlay/settings/publish", {})
    u = pub["underlay"]
    note("已发布：configured=%s，每台的中转端口 %s" %
         (u.get("configured"), [(n["node_id"], n.get("transit_port")) for n in u["nodes"]]))
    api.call("POST", "/api/logout", {})


BATCHES = [
    ("full-mesh(1,3,4)", {"nodes": [1, 3, 4], "mode": "full-mesh"},
     [(1, 3), (1, 4), (3, 4)]),
    ("hub-spoke(hub=2, nodes=[2,3,4], spoke_mode=none)",
     {"nodes": [2, 3, 4], "mode": "hub-spoke", "hub": 2, "spoke_mode": "none"},
     [(2, 3), (2, 4)]),
]


def cmd_tunnels(api):
    creds = read_creds()
    api.login_tenant(creds["username"], creds["password"])
    log("tunnels: 按用户口径 —— 1/3/4 先 full-mesh，再 2 为 hub 的 hub-spoke(2,3,4)")
    bynode = {n["node_id"]: n for n in api.call("GET", "/api/underlay")["nodes"]}
    gaps = []
    for title, req, pairs in BATCHES:
        plan = api.call("POST", "/api/underlay/auto/plan", req)
        adds, skips = plan["plan"]["add"], plan["plan"].get("skip") or []
        note("%s 计划：新增 %d 条，跳过 %d 条" % (title, len(adds), len(skips)))
        for s in skips:
            note("  跳过 %s<->%s：%s" % (s.get("serial_a"), s.get("serial_b"), s.get("reason")))
        if adds:
            ap = api.call("POST", "/api/underlay/auto/apply", req)
            for t in ap["plan"]["add"]:
                note("  批次建成 %s：%s <-> %s" %
                     (t["id"], t["a"]["serial"], t["b"]["serial"]))
            continue
        # 批量入口一条也没建成：它只认反射器观测到的外层地址，AutoRequest 里没有
        # 手填地址的位置。改走同一控制器的「手工建隧道」表单 —— endpointAddr()
        # 明确写了「显式地址优先，其次才是观测」，所以填 WAN 地址是产品支持的入口，
        # 不是绕开控制器。差在哪一层，记进 gaps 让报告点名。
        gaps.append(dict(batch=title, skip_reasons=[s.get("reason") for s in skips],
                         pairs=[list(p) for p in pairs]))
        taken = {frozenset((t["a"]["node_id"], t["b"]["node_id"]))
                 for t in api.call("GET", "/api/underlay")["tunnels"]}
        for (x, y) in pairs:
            if frozenset((x, y)) in taken:
                note("  已存在 %s<->%s，跳过" % (BOX[x]["serial"], BOX[y]["serial"]))
                continue
            body = {"a": {"node_id": x, "iface": pick_wan_iface(x, bynode[x]),
                          "addr": BOX[x]["wan"]},
                    "b": {"node_id": y, "iface": pick_wan_iface(y, bynode[y]),
                          "addr": BOX[y]["wan"]}}
            t = api.call("POST", "/api/underlay/tunnels", body, want_status=201)["tunnel"]
            note("  手工建成 %s：%s(%s:%s) <-> %s(%s:%s)，overlay %s<->%s，MTU %s" %
                 (t["id"], t["a"]["serial"], t["a"]["iface"], t["a"]["port"],
                  t["b"]["serial"], t["b"]["iface"], t["b"]["port"],
                  t["a"]["overlay"], t["b"]["overlay"], t.get("mtu")))
    if gaps:
        with open(os.path.join(RUN, "gap-observation.json"), "w") as f:
            json.dump(gaps, f, ensure_ascii=False, indent=1)
        note("批量入口对这 4 台全部让路给了手填地址，证据写入 run/loop/gap-observation.json")

    u = api.call("GET", "/api/underlay")
    # 断言：1 与 2 之间**不许**有隧道（站点内靠共享 LAN，不靠隧道）。
    bad = [t["id"] for t in u["tunnels"]
           if {t["a"]["node_id"], t["b"]["node_id"]} == {1, 2}]
    if bad:
        die("1 与 2 之间出现了隧道 %s，违背测试拓扑" % bad)
    want = {frozenset(p) for _, _, ps in BATCHES for p in ps}
    got = {frozenset((t["a"]["node_id"], t["b"]["node_id"])) for t in u["tunnels"]}
    if got != want:
        die("隧道集合和计划不符：多 %s / 少 %s" %
            (sorted([tuple(s) for s in got - want]), sorted([tuple(s) for s in want - got])))
    note("现有隧道 %d 条：%s" % (len(u["tunnels"]), [t["id"] for t in u["tunnels"]]))
    per = {}
    for t in u["tunnels"]:
        for side, other in (("a", "b"), ("b", "a")):
            per.setdefault(t[side]["node_id"], []).append(
                dict(tunnel=t["id"], peer=t[other]["node_id"],
                     local_overlay=t[side]["overlay"], peer_overlay=t[other]["overlay"],
                     iface=t[side]["iface"], port=t[side]["port"], color=t.get("color"),
                     mtu=t.get("mtu")))
    with open(os.path.join(RUN, "tunnels.json"), "w") as f:
        json.dump({"tunnels": u["tunnels"], "by_node": per}, f, ensure_ascii=False, indent=1)
    note("每台一份隧道清单写入 run/loop/tunnels.json（fib 同步要用本端 overlay→接口名）")
    api.call("POST", "/api/logout", {})



def pick_wan_iface(node_view, doc):
    """隧道要用的出口口：设备自己上报的清单里带着 WAN 地址的那张 host- 接口。

    页面上这是「本端物理端口」下拉框的一项，数据同源（心跳里的 Inventory）。
    """
    wan = BOX[node_view]["wan"]
    for i in (doc.get("ifaces") or []):
        if not (i.get("name") or "").startswith("host-"):
            continue
        if any(a.split("/")[0] == wan for a in (i.get("addrs") or [])):
            return i["name"]
    die("node %s 的出口口挑不出来（WAN %s 没出现在上报清单里）" % (node_view, wan))


def pick_customer_iface(node_view, doc):
    """从设备自己上报的 VPP 接口清单里挑客户口：不是 WAN 那张的 host- 接口。

    控制台页面上这一步是人看的下拉框（清单来自心跳里的 Inventory）；这里用同
    一份数据，判据是「host- 开头且不带 WAN 地址」。
    """
    wan = BOX[node_view]["wan"]
    ifs = (doc.get("ifaces") or [])
    cands = []
    for i in ifs:
        name = i.get("name") or ""
        if not name.startswith("host-"):
            continue
        addrs = i.get("addrs") or []
        if any(a.split("/")[0] == wan for a in addrs):
            continue
        cands.append(i)
    if len(cands) != 1:
        die("node %s 的客户口挑不出来（候选 %s）" %
            (node_view, [(c.get("name"), c.get("addrs")) for c in cands]))
    return cands[0]["name"]


def basic_config(node, vpp_if):
    b = BOX[node]
    vpn = {"id": b["vpn"], "name": "vpn0",
           "bgp": {"on": b["proto"] == "bgp", "asn": b.get("asn", 0),
                   "router_id": b["mgmt"],
                   "neighbors": ([{"ip": b["gw"], "remote_as": b["peer_as"],
                                   "description": "lp-cust%d" % node}]
                                 if b["proto"] == "bgp" else [])},
           "ospf": {"on": b["proto"] == "ospf", "router_id": b["mgmt"],
                    "area": b.get("area", 0)}}
    cfg = {"hostname": b["serial"], "vpns": [vpn],
           "ifaces": [{"vpp": vpp_if, "lcp": LCP, "ip": b["cust"], "prefix_len": 24,
                       "vpn": b["vpn"], "mtu": 1500, "arp_ignore": True, "up": True}],
           "statics": [], "vrrp": []}
    if node in (1, 2):
        cfg["vrrp"].append({"vpp": vpp_if, "vr_id": 1, "priority": b["prio"],
                            "interval_ms": 1000, "preempt": True, "accept": False,
                            "virtual_addr": SITE_VIP})
    return cfg


def cmd_config(api, publish=True):
    creds = read_creds()
    api.login_tenant(creds["username"], creds["password"])
    log("config: 逐台下基础配置（客户口 + VPN + OSPF/BGP + VRRP）")
    u = api.call("GET", "/api/underlay")
    bynode = {n["node_id"]: n for n in u["nodes"]}
    for n in (1, 2, 3, 4):
        if n not in bynode:
            die("控制台里看不到 node %d（先 --stage enroll）" % n)
        vpp_if = pick_customer_iface(n, bynode[n])
        cfg = basic_config(n, vpp_if)
        doc = api.call("PUT", "/api/config?node=%d" % n, {"node_id": n, "config": cfg})
        errs = [c["text"] for c in doc.get("checks", []) if c["severity"] == "error"]
        note("node %d 草稿 v%s，接口 %s，检查 %d 条%s" %
             (n, doc["version"], vpp_if, len(doc.get("checks") or []),
              ("，错误：" + "; ".join(errs)) if errs else ""))
        if errs:
            die("node %d 的基础配置有 error 检查，先解决" % n)
    if publish:
        _publish_all(api)
    api.call("POST", "/api/logout", {})


def cmd_overlay(api):
    creds = read_creds()
    api.login_tenant(creds["username"], creds["password"])
    log("overlay: 导通组 + 每台 RR 绑定 + prefix-list + 路由泄露")
    ov = api.call("GET", "/api/overlay")
    if not ov["groups"]:
        g = api.call("POST", "/api/overlay/group",
                     {"name": "环路互通域",
                      "members": [{"node_id": n, "vpn_id": 0} for n in (1, 2, 3, 4)]})
        note("导通组 %s：RT %s，成员 1/2/3/4 的 vpn0" % (g["group"]["id"], g["group"]["rt"]))
    else:
        note("导通组已存在：%s" % [x["id"] for x in ov["groups"]])
    rrfile = os.path.join(RUN, "rr-id")
    if not os.path.exists(rrfile):
        die("缺 run/loop/rr-id：先跑 rrs 步骤")
    rid = open(rrfile).read().strip()
    for n in (1, 2, 3, 4):
        api.call("PUT", "/api/overlay/device-rr",
                 {"node_id": n, "primary": rid, "backup": ""})
        note("node %d 的 RR 1 = %s（RR 2 留空：这台实验只有一台反射器）" % (n, rid))
    pl = api.call("GET", "/api/leak")
    plist = None
    for x in pl["prefix_lists"]:
        if x["name"] == "客户 loopback":
            plist = x["id"]
    if not plist:
        d = api.call("POST", "/api/prefix-lists",
                     {"name": "客户 loopback",
                      "entries": [{"prefix": CUSTOMER_AGG, "le": 32}]})
        plist = d["list"]["id"]
    note("prefix-list 客户 loopback = %s（%s le 32）" % (plist, CUSTOMER_AGG))
    for n in (1, 2, 3, 4):
        proto = BOX[n]["proto"]
        api.call("PUT", "/api/leak/rows",
                 {"node_id": n, "direction": "to_sdwan", "vpn": 0,
                  "protocols": [proto], "prefix_list": plist})
        api.call("PUT", "/api/leak/rows",
                 {"node_id": n, "direction": "to_cpe", "vpn": 0,
                  "protocols": [proto], "prefix_list": ""})
        note("node %d：客户→SDWAN 收 %s（只放 %s）；SDWAN→客户 写回 %s"
             % (n, proto, CUSTOMER_AGG, proto))
    ov = api.call("GET", "/api/overlay")
    note("逃生开关当前 = %s（要改是 PUT /api/overlay/settings）" % ov["escape_all_colors"])
    errs = [c["text"] for c in ov.get("checks", []) if c["severity"] == "error"]
    if errs:
        die("overlay 有 error 检查：%s" % "; ".join(errs))
    api.call("POST", "/api/logout", {})


def _publish_all(api):
    log("publish: 逐台发布 —— overlay/RR/泄露都是这一份包的第 4 段")
    for n in (1, 2, 3, 4):
        doc = api.call("POST", "/api/config/publish", {"node_id": n})
        note("node %d 发布 v%s（published=%s）" %
             (n, doc["version"], doc.get("published_version")))


def cmd_publish(api):
    creds = read_creds()
    api.login_tenant(creds["username"], creds["password"])
    _publish_all(api)
    api.call("POST", "/api/logout", {})


def cmd_status(api):
    creds = read_creds()
    api.login_tenant(creds["username"], creds["password"])
    log("status")
    st = api.call("GET", "/api/state")
    u = api.call("GET", "/api/underlay")
    bynode = {n["node_id"]: n for n in u["nodes"]}
    for nd in sorted(st["nodes"], key=lambda x: x["node_id"]):
        n = nd["node_id"]
        cv = api.call("GET", "/api/config?node=%d" % n)
        td = bynode[n].get("tunnel_delivery") or {}
        ds = cv.get("device_state") or {}
        print("   node %-2d %-9s %-6s 基础配置 发布v%s/设备v%s%s  隧道设备v%s%s  站点 %s" % (
            n, nd["serial"], nd["status"], cv.get("published_version"),
            ds.get("version"), "" if ds.get("ok") else " [未确认]",
            td.get("version"), "" if td.get("ok") else " [未确认]",
            (nd.get("runtime") or {}).get("site_name") or "-"))
        if ds.get("error"):
            print("      基础配置错误：%s" % ds["error"])
        if td.get("error"):
            print("      隧道错误：%s" % td["error"])
        if cv.get("overlay_pending"):
            print("      overlay 有未发布的改动")
    api.call("POST", "/api/logout", {})


def cmd_dump(api, path):
    creds = read_creds()
    api.login_tenant(creds["username"], creds["password"])
    print(json.dumps(api.call("GET", path), ensure_ascii=False, indent=1))


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(2)
    cmd = sys.argv[2 - 1]
    api = Api()
    if cmd == "setup":
        cmd_setup(api)
    elif cmd == "rrs":
        cmd_rrs(api)
    elif cmd == "sites":
        cmd_sites(api)
    elif cmd == "underlay":
        cmd_underlay(api)
    elif cmd == "tunnels":
        cmd_tunnels(api)
    elif cmd == "config":
        cmd_config(api, publish="--no-publish" not in sys.argv)
    elif cmd == "overlay":
        cmd_overlay(api)
    elif cmd == "publish":
        cmd_publish(api)
    elif cmd == "status":
        cmd_status(api)
    elif cmd == "dump":
        cmd_dump(api, sys.argv[2])
    else:
        die("未知步骤：%s" % cmd)


# loop-capture.py 会 import 这个模块复用会话与租户凭据；没有这道门的话，
# 被 import 时也会跑一遍 main()（没参数就打印用法并 exit 2）。
if __name__ == "__main__":
    main()
