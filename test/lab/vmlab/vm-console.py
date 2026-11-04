#!/usr/bin/env python3
"""vm-console.py - VM 研发台的所有配置动作**只走控制台**。

和 test/lab/loop-console.py 同一套做法：不发一条 vppctl、不写一个 /etc/frr/frr.conf、
不碰 route.json 的路由内容。它只做浏览器会做的事 —— 登录后 PUT/POST 那些 /api/* 端点，
字段与控制台页面逐一对应，所以每一步都能在你复核时按步骤复述。

用法（顺序 = 搭建顺序，中间夹着盒子的 bringup）：
    vm-console.py code        # 租户给这张授权重新生成入网码（写 state/claim.txt，0600）
    vm-console.py preauth     # 四台设备的预授权（名称 + 序列号），enroll 时按名字认领
    (vm-bringup.sh --phase E) # 四台盒子拿码入网、起心跳
    vm-console.py sites       # 站点：site-12 放 1、2 两台；site-3、site-4 各一台
    vm-console.py underlay    # 全局配置 + 发布全局配置（发完之后才能建隧道）
    vm-console.py tunnels     # 批量：full-mesh(1,3,4) 然后 hub-spoke(hub=2, nodes=2,3,4)
    vm-console.py config      # 每台基础配置（客户口/VPN/OSPF/BGP），**这一轮不发 VRRP**
    vm-console.py overlay     # 导通组 + 每台 RR 主备 + prefix-list + 路由泄露
    vm-console.py publish     # 逐台发布（overlay/RR/泄露都是这一份包的第 4 段）
    vm-console.py status      # 打一张版本/在线/下发回执的表
    vm-console.py dump <path> # 读一个 GET 端点，便于抓证据

口令、入网码、RR 私钥只在 ~/srvmlab/state/（仓库外），权限 0600；
请求日志 ~/srvmlab/state/console-log.jsonl 里这些字段一律打星。
"""
import hashlib
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request

RUN = os.path.expanduser("~/srvmlab/state")
CONSOLE = os.environ.get("VM_CONSOLE", "http://127.0.0.1:8088")

TENANT = "tenant-vm"
OPS_TOKEN_FILE = os.path.join(RUN, "console-token")
CRED_FILE = os.path.join(RUN, "credentials.json")
CLAIM_FILE = os.path.join(RUN, "claim.txt")
LOG_FILE = os.path.join(RUN, "console-log.jsonl")

# ---- 拓扑常量（与 bringup-mac.sh 的地址表一致；MAC/地址都是手工配的，控制器不猜）----
# 这一轮没有管理通道（拍板 4）：路由 router-id 用 WAN 地址，不再有 10.99 那张网。
# 客户 IP 一律由 cust 盒子的 loopback 播（拍板 5），汇聚前缀沿用容器 loop lab 的 172.16/16。
LCP = "lcp-x0"          # 客户口的 TAP 名（config_render.go 写死 arp_ignore 的那张）
CUSTOMER_AGG = "172.16.0.0/16"
BOX = {
    1: dict(serial="CPE1", cust="172.31.12.1", gw=None, wan="192.168.1.101",
            proto="ospf", vpn=0, area=0, cust_hint="lan12"),
    2: dict(serial="CPE2", cust="172.31.12.2", gw=None, wan="192.168.1.102",
            proto="ospf", vpn=0, area=0, cust_hint="lan12"),
    3: dict(serial="CPE3", cust="172.31.3.1", gw="172.31.3.2", wan="192.168.1.103",
            proto="bgp", vpn=0, asn=65103, peer_as=65003, cust_hint="lan3"),
    4: dict(serial="CPE4", cust="172.31.4.1", gw="172.31.4.2", wan="192.168.1.104",
            proto="bgp", vpn=0, asn=65104, peer_as=65004, cust_hint="lan4"),
}
RR_PRIMARY = "VM RR-A"
RR_BACKUP = "VM RR-B"
OVERLAY_CIDR = "10.250.0.0/16"


def log(msg):
    print("== %s" % msg, flush=True)


def note(msg):
    print("   %s" % msg, flush=True)


def die(msg):
    print("FATAL: %s" % msg, file=sys.stderr, flush=True)
    sys.exit(2)


SECRET_KEYS = {"password", "old_password", "new_password", "code", "key_pem",
               "cert_pem", "ca_pem", "token", "claim_code"}
CODE_RE = re.compile(r"sr[ck]_[0-9A-Z]{16,40}")


def redact(obj):
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

    def login_tenant(self, username, password):
        self.call("POST", "/api/login", {"username": username, "password": password})
        s = self.call("GET", "/api/session")
        self.who = s.get("role")
        if s.get("role") != "tenant" or s.get("tenant_id") != TENANT:
            die("租户登录后身份不对：%s" % json.dumps(s))
        return s

    def logout(self):
        self.call("POST", "/api/logout", {})
        self.cookie = None


def read_creds():
    with open(CRED_FILE) as f:
        return json.load(f)


def api_tenant():
    """每个子命令开头：租户登录；结束时由调用方 logout。"""
    creds = read_creds()
    a = Api()
    a.login_tenant(creds["username"], creds["password"])
    return a, creds


# ---------------------------------------------------------------- 步骤
def cmd_code(api):
    """码只在租户手里生成（运营方不持码）；这张码给四台设备共用，名额 4。"""
    creds = read_creds()
    code = api.call("POST", "/api/claims/code",
                    {"id": creds["claim_id"], "ttl_sec": 86400})
    with open(CLAIM_FILE, "w") as f:
        f.write(code["code"])
    os.chmod(CLAIM_FILE, 0o600)
    note("入网码写入 state/claim.txt（到 %s，可用 %s 次）" %
         (time.strftime("%m-%d %H:%M:%S", time.localtime(code["expires_at_unix"])),
          code.get("max_uses", "?")))


def cmd_preauth(api):
    """四台的预授权：先在册，enroll 时按名字认领，配置可以提前挂上去。"""
    creds = read_creds()
    have = {p["name"]: p for p in api.call("GET", "/api/preauth")["preauths"]}
    for n in (1, 2, 3, 4):
        name = BOX[n]["serial"]
        if name in have:
            p = have[name]
            note("预授权 %s 已存在（id=%s 认领=%s）" %
                 (name, p["id"], p.get("bound_node_id") or "未认领"))
            continue
        d = api.call("POST", "/api/preauth",
                     {"claim_id": creds["claim_id"], "name": name, "serial": name})
        p = d.get("preauth") or d
        note("建预授权 %s = %s" % (name, p.get("id")))


def cmd_sites(api):
    log("sites: 站点 12 是一台客户路由器接两台盒子，3/4 各一台")
    want = [("VM 站点 12（两台盒子）", 116.4, 39.9, [1, 2]),
            ("VM 站点 3", 121.5, 31.2, [3]),
            ("VM 站点 4", 113.3, 23.1, [4])]
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


def cmd_underlay(api):
    log("underlay: 写全局配置草稿，然后发布全局配置（发布之后才能建隧道）")
    st = (api.call("GET", "/api/underlay").get("settings") or {})
    body = {
        "vpn_ports": st.get("vpn_ports") or {"min": 20000, "max": 20999},
        "transit_ports": st.get("transit_ports") or {"min": 15000, "max": 15999},
        "overlay_cidr": st.get("overlay_cidr") or OVERLAY_CIDR,
        "probe_interval_ms": st.get("probe_interval_ms") or 300,
        "bfd_min_tx_ms": st.get("bfd_min_tx_ms") or 300,
        "bfd_min_rx_ms": st.get("bfd_min_rx_ms") or 300,
        "bfd_detect_mult": st.get("bfd_detect_mult") or 3,
    }
    api.call("PUT", "/api/underlay/settings", body)
    note("草稿：%s" % json.dumps(body, ensure_ascii=False))
    u = api.call("POST", "/api/underlay/settings/publish", {})["underlay"]
    note("已发布：configured=%s，每台的中转端口 %s" %
         (u.get("configured"), [(n["node_id"], n.get("transit_port")) for n in u["nodes"]]))


BATCHES = [
    ("full-mesh(1,3,4)", {"nodes": [1, 3, 4], "mode": "full-mesh"},
     [(1, 3), (1, 4), (3, 4)]),
    ("hub-spoke(hub=2, nodes=[2,3,4], spoke_mode=none)",
     {"nodes": [2, 3, 4], "mode": "hub-spoke", "hub": 2, "spoke_mode": "none"},
     [(2, 3), (2, 4)]),
]


def pick_wan_iface(node_view, doc):
    """隧道要用的出口口：设备自己上报的清单里带着 WAN 地址的那张 host- 接口。"""
    wan = BOX[node_view]["wan"]
    for i in (doc.get("ifaces") or []):
        if not (i.get("name") or "").startswith("host-"):
            continue
        if any(a.split("/")[0] == wan for a in (i.get("addrs") or [])):
            return i["name"]
    die("node %s 的出口口挑不出来（WAN %s 没出现在上报清单里）" % (node_view, wan))


def pick_customer_iface(node_view, doc):
    """从设备自己上报的 VPP 接口清单里挑客户口：不是 WAN 那张的 host- 接口。"""
    wan = BOX[node_view]["wan"]
    cands = []
    for i in (doc.get("ifaces") or []):
        name = i.get("name") or ""
        if not name.startswith("host-"):
            continue
        if any(a.split("/")[0] == wan for a in (i.get("addrs") or [])):
            continue
        cands.append(i)
    if len(cands) != 1:
        die("node %s 的客户口挑不出来（候选 %s）" %
            (node_view, [(c.get("name"), c.get("addrs")) for c in cands]))
    return cands[0]["name"]


def cmd_tunnels(api):
    log("tunnels: 1/3/4 先 full-mesh，再 2 为 hub 的 hub-spoke(2,3,4)")
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
                note("  批次建成 %s：%s <-> %s" % (t["id"], t["a"]["serial"], t["b"]["serial"]))
            continue
        # 批量入口只认反射器观测到的外层地址；这台子是同一段 LAN（192.168.1.0/24），
        # 观测值可能还没回来。改走同一控制器的「手工建隧道」表单 —— endpointAddr()
        # 明确写了「显式地址优先，其次才是观测」，填 WAN 地址是产品支持的入口。
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
            note("  手工建成 %s：%s(%s:%s) <-> %s(%s:%s)，overlay %s<->%s，MTU %s，颜色 %s" %
                 (t["id"], t["a"]["serial"], t["a"]["iface"], t["a"]["port"],
                  t["b"]["serial"], t["b"]["iface"], t["b"]["port"],
                  t["a"]["overlay"], t["b"]["overlay"], t.get("mtu"), t.get("color")))
    if gaps:
        with open(os.path.join(RUN, "gap-observation.json"), "w") as f:
            json.dump(gaps, f, ensure_ascii=False, indent=1)
        note("批量入口让路给了手填地址，证据写入 state/gap-observation.json")

    u = api.call("GET", "/api/underlay")
    # 断言：1 与 2 之间**不许**有隧道（站点内靠共享 LAN，不靠隧道）。
    bad = [t["id"] for t in u["tunnels"] if {t["a"]["node_id"], t["b"]["node_id"]} == {1, 2}]
    if bad:
        die("1 与 2 之间出现了隧道 %s，违背测试拓扑" % bad)
    want = {frozenset(p) for _, _, ps in BATCHES for p in ps}
    got = {frozenset((t["a"]["node_id"], t["b"]["node_id"])) for t in u["tunnels"]}
    if got != want:
        die("隧道集合和计划不符：多 %s / 少 %s" %
            (sorted([tuple(s) for s in got - want]), sorted([tuple(s) for s in want - got])))
    note("现有隧道 %d 条：%s" % (len(u["tunnels"]), [t["id"] for t in u["tunnels"]]))

    # 隧道 MTU 只在建/改那一刻按设备上报的链路 MTU 算，存量隧道不会自己跟着变
    # （controller/internal/underlay/tunnel.go 的 tunnelMTU 只在 Create/Update 里调）。
    # 盒子的接管路径以前把 af-packet 出生那格 9000 当真值上报，算出来的 8868 是假值，
    # agent 按真实链路预算会硬拒装隧道。所以收口完盒子 MTU 必须逐条走一次「只按 id 编辑」
    # 把 MTU 拉回真实预算；不删口、不动端口和 overlay，WG 槽位也不变。
    before = {t["id"]: t.get("mtu") for t in u["tunnels"]}
    for tid in sorted(before):
        api.call("PUT", "/api/underlay/tunnels", {"id": tid})
    u = api.call("GET", "/api/underlay")
    for t in u["tunnels"]:
        note("  MTU 重算 %s：%s → %s" % (t["id"], before.get(t["id"]), t.get("mtu")))

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
    note("每台一份隧道清单写入 state/tunnels.json（fib 同步要用本端 overlay→接口名）")


def basic_config(node, vpp_if):
    b = BOX[node]
    vpn = {"id": b["vpn"], "name": "vpn0",
           "bgp": {"on": b["proto"] == "bgp", "asn": b.get("asn", 0),
                   "router_id": b["wan"],
                   "neighbors": ([{"ip": b["gw"], "remote_as": b["peer_as"],
                                   "description": "cust%d" % node}]
                                 if b["proto"] == "bgp" else [])},
           "ospf": {"on": b["proto"] == "ospf", "router_id": b["wan"],
                    "area": b.get("area", 0)}}
    # 拍板 7：这一轮不发 VRRP —— 站点 12 两台只跑"共享 LAN + 各自路由"。
    return {"hostname": b["serial"], "vpns": [vpn],
            "ifaces": [{"vpp": vpp_if, "lcp": LCP, "ip": b["cust"], "prefix_len": 24,
                        "vpn": b["vpn"], "mtu": 1500, "arp_ignore": True, "up": True}],
            "statics": [], "vrrp": []}


def cmd_config(api, publish=True):
    log("config: 逐台下基础配置（客户口 + VPN + OSPF/BGP；VRRP 这一轮不发）")
    u = api.call("GET", "/api/underlay")
    bynode = {n["node_id"]: n for n in u["nodes"]}
    for n in (1, 2, 3, 4):
        if n not in bynode:
            die("控制台里看不到 node %d（先跑 vm-bringup.sh --phase E 入网）" % n)
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


def cmd_overlay(api):
    log("overlay: 导通组 + 每台 RR 主备 + prefix-list + 路由泄露")
    ov = api.call("GET", "/api/overlay")
    if not ov["groups"]:
        g = api.call("POST", "/api/overlay/group",
                     {"name": "VM 互通域",
                      "members": [{"node_id": n, "vpn_id": 0} for n in (1, 2, 3, 4)]})
        note("导通组 %s：RT %s，成员 1/2/3/4 的 vpn0" % (g["group"]["id"], g["group"]["rt"]))
    else:
        note("导通组已存在：%s" % [x["id"] for x in ov["groups"]])

    rrs = api.call("GET", "/api/rrs")["rrs"]
    by_name = {r["name"]: r["id"] for r in rrs}
    for label, name in (("RR 1", RR_PRIMARY), ("RR 2", RR_BACKUP)):
        if name not in by_name:
            die("RR 库里没有「%s」：现有 %s" % (name, list(by_name)))
    rid, bid = by_name[RR_PRIMARY], by_name[RR_BACKUP]
    for n in (1, 2, 3, 4):
        api.call("PUT", "/api/overlay/device-rr", {"node_id": n, "primary": rid, "backup": bid})
        note("node %d 的 RR 1=%s RR 2=%s" % (n, rid, bid))

    pl = api.call("GET", "/api/leak")
    plist = None
    for x in pl["prefix_lists"]:
        if x["name"] == "客户 loopback":
            plist = x["id"]
    if not plist:
        d = api.call("POST", "/api/prefix-lists",
                     {"name": "客户 loopback", "entries": [{"prefix": CUSTOMER_AGG, "le": 32}]})
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


def _publish_all(api):
    log("publish: 逐台发布 —— overlay/RR/泄露都是这一份包的第 4 段")
    for n in (1, 2, 3, 4):
        doc = api.call("POST", "/api/config/publish", {"node_id": n})
        note("node %d 发布 v%s（published=%s）" %
             (n, doc["version"], doc.get("published_version")))


def cmd_publish(api):
    _publish_all(api)


def cmd_status(api):
    log("status")
    st = api.call("GET", "/api/state")
    u = api.call("GET", "/api/underlay")
    bynode = {n["node_id"]: n for n in u["nodes"]}
    for nd in sorted(st["nodes"], key=lambda x: x["node_id"]):
        n = nd["node_id"]
        cv = api.call("GET", "/api/config?node=%d" % n)
        td = bynode.get(n, {}).get("tunnel_delivery") or {}
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


def cmd_dump(api, path):
    print(json.dumps(api.call("GET", path), ensure_ascii=False, indent=1))


COMMANDS = {
    "code": lambda a: cmd_code(a),
    "preauth": lambda a: cmd_preauth(a),
    "sites": lambda a: cmd_sites(a),
    "underlay": lambda a: cmd_underlay(a),
    "tunnels": lambda a: cmd_tunnels(a),
    "config": lambda a: cmd_config(a, publish="--no-publish" not in sys.argv),
    "overlay": lambda a: cmd_overlay(a),
    "publish": lambda a: cmd_publish(a),
    "status": lambda a: cmd_status(a),
}


def main():
    if len(sys.argv) < 2 or sys.argv[1] not in COMMANDS and sys.argv[1] != "dump":
        print(__doc__)
        sys.exit(2)
    cmd = sys.argv[1]
    api, _ = api_tenant()
    try:
        if cmd == "dump":
            cmd_dump(api, sys.argv[2])
        else:
            COMMANDS[cmd](api)
    finally:
        api.logout()


if __name__ == "__main__":
    main()
