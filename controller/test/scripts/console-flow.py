#!/usr/bin/env python3
"""console-flow.py -- 控制台管理面的端到端脚本（只用标准库）。

跑在控制器容器里：控制台在 http://127.0.0.1:8088（明文 + 会话 cookie），
设备入网在 https://127.0.0.1:8444（mTLS，但 enroll 这一步没有证书）。

A 段是运营方：租户 / 入网码 / 入网 / 吊销，与 GUI 里点的那几步一一对应。
B 段是把"多角色"当成契约来测：权限在服务端收口，页面只是一层皮。

A 段输出：
  unauth_state=401          没有会话读状态必须被拒
  bad_login=401             错误 token 必须被拒
  login=200                 正确 token 换到会话 cookie
  page=SROU 控制台           单页控制台能取到
  state_tenants=<n>         登录后能读到状态
  tenant=tenant-new         建租户
  tenant_account=...        建租户同时得到第一个租户账号
  claim=<code>              建租户同时拿到第一个入网码
  dup_tenant=409            同 ID 再建一次必须冲突
  bad_id=400                非法 tenant_id 被拒（不是"建了再报错"）
  no_json=415               非 JSON 的 POST 被拒（cookie 不是 CSRF 杠杆）
  new_claim=<code>          给已有租户追加签发
  enroll=200                用新租户的入网码入网成功
  enroll_tenant=tenant-new  入网落在新租户下
  enroll_node=1             新租户的第一台设备
  node_status=pending       入网不等于上线：还没有心跳
  node_tenant=tenant-new    状态里的设备归属
  claim_serials=1           入网码记录了被谁用过
  revoke=200                吊销入网码
  claim_revoked=true        状态里反映吊销
  enroll_revoked=403        吊销后不许再入网

B 段输出：
  tenant_login / tenant_role / tenant_scope
  tenant_tenants / tenant_nodes / tenant_claims    租户只看到自己的
  tenant_foreign_nodes / tenant_foreign_claims     必须是 0
  tenant_node_runtime=true                         租户看得到自己设备的运行时
  operator_node_runtime=false                      运营方状态里没有 runtime
  operator_config_leak=false                       原始 JSON 里没有端口/配置字样
  tenant_deny_tenants|claims|revoke|accounts|...   全是 403
  account_password_generated=19                    一次性密码长度
  account_has_password_field=false                 账号列表不回传口令哈希
  short_password=400 / bad_password=401 / unknown_user=401
  disabled_login=401 / reenabled_login=200
  password_change=200 / old_cookie_after_change=401
  old_password_login=401 / new_password_login=200
  initial_password_cleared=false
  token_change_password=400                        应急令牌没有口令可改
  empty_nodes_is_list=true                         空集合是 [] 不是 null
  sse_foreign=0 / sse_own=<n>                      SSE 只投递自己的事件
  login_lock=429                                   连续失败锁定
  tenant_logout=401

入网用的公钥是 32 字节随机数（控制器只校验长度与 base64）：这个脚本要证明的是
"claim code 能不能授权入网"，真设备证书链由 test-vbond.sh 里 openssl 那几条负责。
"""
import base64
import http.client
import http.cookiejar
import json
import os
import ssl
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

CONSOLE = os.environ.get("CONSOLE", "http://127.0.0.1:8088")
CTRL = os.environ.get("CTRL", "https://127.0.0.1:8444")
CA = os.environ.get("CA", "/tmp/ctrl-data/ca.pem")
TOKEN = sys.argv[1] if len(sys.argv) > 1 else ""

jar = http.cookiejar.CookieJar()
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
plain = urllib.request.build_opener()
tls = ssl.create_default_context(cafile=CA)


def call(method, url, body=None, json_ct=True, opener_=opener, cookie=None):
    data = None
    req = urllib.request.Request(url, method=method)
    if body is not None:
        data = json.dumps(body).encode()
        if json_ct:
            req.add_header("Content-Type", "application/json")
    if cookie is not None:
        req.add_header("Cookie", cookie)
        opener_ = plain
    try:
        with opener_.open(req, data, timeout=10) as r:
            raw = r.read()
            return r.status, raw
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except urllib.error.URLError as e:
        return 0, str(e).encode()


def jstat(code, raw):
    if code == 200 and raw:
        try:
            return json.loads(raw)
        except ValueError:
            return None
    return None


def say(k, v):
    print("%s=%s" % (k, v))


def cookie_header(j):
    return "; ".join("%s=%s" % (c.name, c.value) for c in j)


def enroll(claim, serial):
    # 32 bytes, base64: 控制器只校验长度。见文件头说明。
    pub = base64.b64encode(os.urandom(32)).decode()
    body = json.dumps({
        "serial": serial,
        "claim_code": claim,
        "public_key": pub,
        "protocol_ver": 3,
        "agent_version": "console-flow",
    }).encode()
    req = urllib.request.Request(CTRL + "/v1/enroll", data=body, method="POST")
    req.add_header("Content-Type", "application/json")
    plain_tls = urllib.request.build_opener(urllib.request.HTTPSHandler(context=tls))
    try:
        with plain_tls.open(req, timeout=10) as r:
            return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except urllib.error.URLError as e:
        return 0, str(e).encode()


class SSEWriter:
    """读过 /api/events 的原始事件流，只记 (event, tenant_id)。

    订阅过滤必须在服务端发生；这个 reader 用租户会话连上去，运营方再给
    两家租户各打一个事件，收到别家的就算失败。
    """

    def __init__(self, cookie):
        self.cookie = cookie
        self.events = []
        self.hello = threading.Event()
        self.status = 0

    def run(self):
        u = urllib.parse.urlsplit(CONSOLE)
        host, port = u.hostname, u.port or 80
        try:
            conn = http.client.HTTPConnection(host, port, timeout=8)
            conn.request("GET", "/api/events", headers={"Cookie": self.cookie})
            resp = conn.getresponse()
            self.status = resp.status
            if resp.status != 200:
                return
            etype = ""
            for raw in resp:
                line = raw.decode(errors="replace").rstrip("\n")
                if line.startswith("event: "):
                    etype = line[len("event: "):]
                elif line.startswith("data: "):
                    payload = json.loads(line[len("data: "):])
                    self.events.append((etype, payload.get("tenant_id", "")))
                    if etype == "hello":
                        self.hello.set()
                elif line == "":
                    etype = ""
        except Exception:
            return

    def start(self):
        t = threading.Thread(target=self.run, daemon=True)
        t.start()
        return t


# ---------------------------------------------------------------- A. 运营方
# 1. 未登录
code, _ = call("GET", CONSOLE + "/api/state")
say("unauth_state", code)

# 2. 错误 token
code, _ = call("POST", CONSOLE + "/api/login", {"token": "definitely-wrong"})
say("bad_login", code)

# 3. 正确 token
code, raw = call("POST", CONSOLE + "/api/login", {"token": TOKEN})
say("login", code)
if code != 200:
    sys.exit(1)

# 4. 控制台页面
code, raw = call("GET", CONSOLE + "/")
say("page", "SROU 控制台" if code == 200 and "SROU 控制台".encode() in raw else "missing")

# 5. 登录后读状态
code, raw = call("GET", CONSOLE + "/api/state")
st = jstat(code, raw)
say("state_tenants", len(st["tenants"]) if st else "none")

# 6. 建租户（同时得到第一个入网码与租户管理员账号）
code, raw = call("POST", CONSOLE + "/api/tenants", {
    "tenant_id": "tenant-new", "name": "新建租户",
    "account": "tenant-new-admin", "password": "tenant-new-pass-2026",
})
res = jstat(code, raw)
if not res:
    say("tenant", "HTTP %d: %s" % (code, raw.decode(errors="replace")))
    sys.exit(1)
say("tenant", res["tenant"]["tenant_id"])
say("tenant_account", (res.get("account") or {}).get("username", "none"))
claim = res["claim"]["code"]
say("claim", claim)

# 7. 重复建租户
code, _ = call("POST", CONSOLE + "/api/tenants", {"tenant_id": "tenant-new"})
say("dup_tenant", code)

# 8. 非法 tenant_id
code, _ = call("POST", CONSOLE + "/api/tenants", {"tenant_id": "Tenant New"})
say("bad_id", code)

# 9. 非 JSON 的 POST（cookie 不能当 CSRF 杠杆）
code, _ = call("POST", CONSOLE + "/api/claims",
               {"tenant_id": "tenant-new"}, json_ct=False)
say("no_json", code)

# 10. 追加签发入网码
code, raw = call("POST", CONSOLE + "/api/claims", {"tenant_id": "tenant-new", "note": "second"})
res = jstat(code, raw)
second = res["code"] if res else "none"
say("new_claim", second)

# 11. 用第二个码入网
code, res = enroll(second, "vcpe-001")
say("enroll", code)
if isinstance(res, dict):
    say("enroll_tenant", res.get("tenant_id"))
    say("enroll_node", res.get("node_id"))
else:
    say("enroll_tenant", res.decode(errors="replace")[:120])

# 12. 状态里能看到它，且是 pending（入网 ≠ 上线）
code, raw = call("GET", CONSOLE + "/api/state")
st = jstat(code, raw)
node = next((n for n in st["nodes"] if n["serial"] == "vcpe-001"), None) if st else None
say("node_status", node["status"] if node else "missing")
say("node_tenant", node["tenant_id"] if node else "missing")
c2 = next((c for c in st["claims"] if c["code"] == second), None) if st else None
say("claim_serials", len(c2["serials"]) if c2 else "missing")

# 13. 吊销
code, _ = call("POST", CONSOLE + "/api/claims/revoke", {"code": second})
say("revoke", code)
code, raw = call("GET", CONSOLE + "/api/state")
st = jstat(code, raw)
c2 = next((c for c in st["claims"] if c["code"] == second), None) if st else None
say("claim_revoked", str(c2["revoked"]).lower() if c2 else "missing")

# 14. 吊销后同一码不能再入网
code, _ = enroll(second, "vcpe-002")
say("enroll_revoked", code)

# ---------------------------------------------------------------- B. 多角色
# B1. 第二家租户：有账号、没有设备（空集合契约）
code, raw = call("POST", CONSOLE + "/api/tenants", {
    "tenant_id": "tenant-other", "name": "另一家",
    "account": "tenant-other-admin", "password": "other-side-2026",
})
res = jstat(code, raw)
say("other_tenant", res["tenant"]["tenant_id"] if res else "HTTP %d" % code)

# B2. 账号列表：有账号、有角色，但永远没有口令哈希
code, raw = call("GET", CONSOLE + "/api/accounts")
accs = jstat(code, raw) or []
say("accounts_status", code)
say("account_has_password_field",
    str(any("password" in a for a in accs)).lower())
say("account_roles",
    ",".join(sorted(set(a.get("role", "") for a in accs))))

# B3. 运营方建账号：不给口令时服务端生成一次性口令（长度 19，分组）
code, raw = call("POST", CONSOLE + "/api/accounts",
                 {"username": "spare-op", "role": "operator"})
res = jstat(code, raw)
say("account_create", code)
say("account_password_generated", len(res.get("password") or "") if res else 0)
say("account_password_returned_once", str(bool(res and res.get("password"))).lower())

# B4. 口令策略在服务端，不只在页面
code, _ = call("POST", CONSOLE + "/api/accounts",
               {"username": "short-pw", "role": "operator", "password": "short"})
say("short_password", code)

# B5. 租户账号登录（独立 cookie jar）
tjar = http.cookiejar.CookieJar()
topener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(tjar))
code, _ = call("POST", CONSOLE + "/api/login",
               {"username": "tenant-new-admin", "password": "tenant-new-pass-2026"},
               opener_=topener)
say("tenant_login", code)
if code != 200:
    sys.exit(1)
code, raw = call("GET", CONSOLE + "/api/session", opener_=topener)
sess = jstat(code, raw) or {}
say("tenant_role", sess.get("role"))
say("tenant_scope", sess.get("tenant_id"))
say("tenant_username", sess.get("username"))
say("tenant_initial_password", str(sess.get("initial_password")).lower())
tenant_cookie = cookie_header(tjar)

# B6. 租户状态：只有自己的一家租户、自己的设备、自己的入网码
code, raw = call("GET", CONSOLE + "/api/state", opener_=topener)
tst = jstat(code, raw) or {}
ttenants = tst.get("tenants") or []
tnodes = tst.get("nodes") or []
tclaims = tst.get("claims") or []
say("tenant_tenants", len(ttenants))
say("tenant_nodes", len(tnodes))
say("tenant_claims", len(tclaims))
say("tenant_foreign_nodes", len([n for n in tnodes if n.get("tenant_id") != "tenant-new"]))
say("tenant_foreign_claims", len([c for c in tclaims if c.get("tenant_id") != "tenant-new"]))
say("tenant_node_runtime", str(bool(tnodes and tnodes[0].get("runtime"))).lower())
say("tenant_node_configured",
    str((tnodes[0].get("runtime") or {}).get("configured")).lower() if tnodes else "missing")

# B7. 运营方状态：设备行里不该有 runtime / 端口 / 配置
code, raw = call("GET", CONSOLE + "/api/state")
ost = jstat(code, raw) or {}
onodes = ost.get("nodes") or []
say("operator_node_runtime", str(any("runtime" in n for n in onodes)).lower())
say("operator_config_leak",
    str(any(k in raw.decode(errors="replace")
            for k in ("udp_mapping", "vpn_port", "configured"))).lower())

# B8. 租户不能做运营方的任何事（403 是明确告知，不是隐藏 404）
def deny(name, path, body):
    code, _ = call("POST", CONSOLE + path, body, opener_=topener)
    say(name, code)

deny("tenant_deny_tenants", "/api/tenants", {"tenant_id": "evil"})
deny("tenant_deny_claims", "/api/claims", {"tenant_id": "tenant-new"})
deny("tenant_deny_revoke", "/api/claims/revoke", {"code": claim})
deny("tenant_deny_accounts", "/api/accounts",
     {"username": "evil-admin", "role": "tenant", "tenant_id": "tenant-new", "password": "evil-pass-2026"})
deny("tenant_deny_account_reset", "/api/accounts/reset", {"username": "tenant-other-admin"})
deny("tenant_deny_account_disable", "/api/accounts/disable",
     {"username": "tenant-other-admin", "disabled": True})
code, _ = call("GET", CONSOLE + "/api/accounts", opener_=topener)
say("tenant_deny_account_list", code)

# B9. 错误口令与不存在的用户：同一个 401，不做用户名枚举
code, _ = call("POST", CONSOLE + "/api/login",
               {"username": "tenant-new-admin", "password": "wrong-password"})
say("bad_password", code)
code, _ = call("POST", CONSOLE + "/api/login",
               {"username": "nobody-here", "password": "wrong-password"})
say("unknown_user", code)

# B10. 应急令牌会话没有口令可改
code, _ = call("POST", CONSOLE + "/api/password",
               {"old_password": "x", "new_password": "whatever-2026"})
say("token_change_password", code)

# B11. 租户改自己的口令；旧 cookie 与旧口令都必须立刻失效
code, _ = call("POST", CONSOLE + "/api/password",
               {"old_password": "tenant-new-pass-2026", "new_password": "short"},
               opener_=topener)
say("tenant_short_password", code)
code, _ = call("POST", CONSOLE + "/api/password",
               {"old_password": "tenant-new-pass-2026", "new_password": "tenant-new-pass-2"},
               opener_=topener)
say("password_change", code)
code, _ = call("GET", CONSOLE + "/api/state", cookie=tenant_cookie)
say("old_cookie_after_change", code)
code, _ = call("POST", CONSOLE + "/api/login",
               {"username": "tenant-new-admin", "password": "tenant-new-pass-2026"})
say("old_password_login", code)
code, _ = call("POST", CONSOLE + "/api/login",
               {"username": "tenant-new-admin", "password": "tenant-new-pass-2"},
               opener_=topener)
say("new_password_login", code)
code, raw = call("GET", CONSOLE + "/api/session", opener_=topener)
sess = jstat(code, raw) or {}
say("initial_password_cleared", str(sess.get("initial_password")).lower())

# B12. 账号被禁用后不能登录，重新启用后可以
code, _ = call("POST", CONSOLE + "/api/accounts/disable",
               {"username": "tenant-other-admin", "disabled": True})
say("disable_tenant_account", code)
cjar = http.cookiejar.CookieJar()
copener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cjar))
code, _ = call("POST", CONSOLE + "/api/login",
               {"username": "tenant-other-admin", "password": "other-side-2026"},
               opener_=copener)
say("disabled_login", code)
code, _ = call("POST", CONSOLE + "/api/accounts/disable",
               {"username": "tenant-other-admin", "disabled": False})
say("reenable_tenant_account", code)
code, _ = call("POST", CONSOLE + "/api/login",
               {"username": "tenant-other-admin", "password": "other-side-2026"},
               opener_=copener)
say("reenabled_login", code)

# B13. 另一家租户的空集合必须是 []；页面无条件对它们 filter
code, raw = call("GET", CONSOLE + "/api/state", opener_=copener)
ost2 = jstat(code, raw) or {}
say("empty_nodes_is_list", str(isinstance(ost2.get("nodes"), list)).lower())
say("empty_nodes_count", len(ost2.get("nodes") or []))

# B14. SSE：租户只收到自己的事件
sse = SSEWriter(cookie_header(tjar))
sse.start()
if not sse.hello.wait(5):
    say("sse_foreign", "no-stream")
    say("sse_own", "no-stream")
else:
    call("POST", CONSOLE + "/api/claims", {"tenant_id": "tenant-other", "note": "sse-foreign"})
    call("POST", CONSOLE + "/api/claims", {"tenant_id": "tenant-new", "note": "sse-own"})
    time.sleep(1.2)
    evs = list(sse.events)
    say("sse_foreign", len([e for e in evs if e[1] not in ("", "tenant-new")]))
    say("sse_own", len([e for e in evs if e[1] == "tenant-new"]))

# B15. 连续失败锁定（独立的账号，避免影响上面的断言）
call("POST", CONSOLE + "/api/accounts",
     {"username": "lock-probe", "role": "operator", "password": "lock-probe-2026"})
for _ in range(5):
    call("POST", CONSOLE + "/api/login", {"username": "lock-probe", "password": "nope-nope"})
code, _ = call("POST", CONSOLE + "/api/login",
               {"username": "lock-probe", "password": "lock-probe-2026"})
say("login_lock", code)

# B16. 退出登录后会话立即失效
code, _ = call("POST", CONSOLE + "/api/logout", {}, opener_=topener)
say("tenant_logout", code)
code, _ = call("GET", CONSOLE + "/api/state", opener_=topener)
say("tenant_after_logout", code)
