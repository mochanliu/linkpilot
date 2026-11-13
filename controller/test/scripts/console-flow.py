#!/usr/bin/env python3
"""console-flow.py -- 控制台管理面的端到端脚本（只用标准库）。

跑在控制器容器里：控制台在 http://127.0.0.1:8088（明文 + 会话 cookie），
设备入网在 https://127.0.0.1:8444（mTLS，但 enroll 这一步没有证书）。

入网是两级模型：运营方只下发「授权」（名称 + 可激活台数，永不出现码），
租户登录后自己点「生成」才拿到一次性码；码默认 5 分钟，明文只在生成响应里
出现一次，落库只有 SHA-256。A 段照这个顺序走一遍，与 GUI 里点的那几步一一
对应。B 段是把"多角色"当成契约来测：权限在服务端收口，页面只是一层皮。

A 段输出：
  unauth_state=401          没有会话读状态必须被拒
  bad_login=401             错误 token 必须被拒
  login=200                 正确 token 换到会话 cookie
  page=LinkPilot 控制台        单页控制台能取到
  state_tenants=<n>         登录后能读到状态
  tenant=tenant-new         建租户
  tenant_account=...        建租户同时得到第一个租户账号
  grant=clm_...             建租户同时下发第一条授权（id，不是码）
  grant_has_code=false      授权响应里没有 code 字段
  grant_no_plaintext=true   响应原文里没有 srk_ 开头的明文
  grant_default_uses=10     没指定台数时的默认名额
  dup_tenant=409            同 ID 再建一次必须冲突
  bad_id=400                非法 tenant_id 被拒（不是"建了再报错"）
  no_json=415               非 JSON 的 POST 被拒（cookie 不是 CSRF 杠杆）
  new_grant=clm_...         给已有租户追加下发授权
  new_grant_name=现场设备    授权带名字（客户在页面里靠它认领）
  new_grant_no_code=true    追加下发同样不产生码
  operator_generate=403     运营方不能替客户生成码
  tenant_login=200          租户账号登录
  tenant_role=tenant / tenant_scope=tenant-new
  tenant_initial_password=true 首次登录仍是一次性口令
  generate_code=200         租户为自己的授权生成码
  claim_code_shape=ok       码是 srk_ + 32 位随机串，不含租户名
  claim_code_hides_tenant=true
  claim_code_ttl=300        默认窗口 5 分钟
  claim_code_name=现场设备    生成响应把授权名带回来，避免复制错
  enroll=200                用刚生成的码入网成功
  enroll_tenant=tenant-new  入网落在新租户下
  enroll_node=1             新租户的第一台设备
  node_status=pending       入网不等于上线：还没有心跳
  node_tenant=tenant-new    状态里的设备归属
  claim_serials=1           授权记录了被谁激活
  claim_code_active=true    状态里只说"码还有效"，不给码
  state_has_secret=false    状态 JSON 里没有明文码
  state_has_hash=false      状态 JSON 里连哈希也不回传
  state_claims_have_code_field=false 授权视图没有 code 字段
  rotate_code=200           重新生成
  rotate_changed=true       新码与旧码不同
  rotate_old_code=403       旧码在重新生成那一刻失效
  rotate_existing_device=200 已入网设备用新码重入仍成功
  revoke=200                按 id 吊销授权
  claim_revoked=true        状态里反映吊销
  revoke_code_active=false  吊销同时清掉当前码
  enroll_revoked=403        吊销后不许再入网
  expiry_code=200           生成 1 秒窗口的码
  expiry_enroll=403         过期后入网被拒（由服务端判定）
  expiry_code_active=false  过期后状态里也不再是"有效"
  expiry_bad_ttl=400        窗口上限在服务端校验（> 24h 被拒）
  expiry_default_ttl=300    不带 ttl 时回到默认 5 分钟
  quota_tenant=tenant-quota
  quota_tenant_claim_uses=2 建租户时指定名额
  quota_generate=200        租户为这条授权生成码
  quota_enroll_1|2=200      名额内正常入网
  quota_enroll_3_blocked=403 名额用满后新设备被拒
  quota_reenroll_existing=200 已入网设备重复入网不占名额
  quota_used=2              状态里 uses 按设备计
  quota_lower=400           名额不能降到已激活数以下
  quota_raise=200           运营方可以调大名额
  quota_enroll_after_raise=200 调大后可以继续入网
  quota_update_unknown=404 / quota_update_too_big=400
  quota_update_zero=200     0 表示默认名额

B 段输出：
  other_tenant=tenant-other / other_grant=clm_...
  other_tenant_generate=200             别家可以生成自己的码
  tenant_foreign_grant_code=404         别家的授权对我是 404（不确认存在）
  tenant_session=200                    会话仍有效
  tenant_tenants / tenant_nodes / tenant_claims    租户只看到自己的
  tenant_foreign_nodes / tenant_foreign_claims     必须是 0
  tenant_node_runtime=true                         租户看得到自己设备的运行时
  metrics_tenant=200 / metrics_nodes=1             监控视图是租户专用的，只列自己的设备
  metrics_status=pending / metrics_uptime=0        没心跳就没在线上时长，不编造
  metrics_window=none / metrics_tunnels_empty=true 没上报遥测时窗口为空数组
  metrics_ttl=30                                   窗口 TTL 与 --metrics-ttl 一致
  metrics_operator=403 / metrics_unauth=401        运营方与未登录都读不到
  operator_node_runtime=false                      运营方状态里没有 runtime
  operator_config_leak=false                       原始 JSON 里没有端口/配置字样
  tenant_deny_tenants|claims|claim_update|revoke|accounts|...   全是 403
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
  tenant_logout=200 / tenant_after_logout=401      退出后会话立即失效

C 段输出（预授权：设备还没到货，先在控制器登记 + 配置）：
  pa_tenant=tenant-pa / pa_grant=clm_... / pa_login=200
  pa_create=200                 租户能预授权一台还没到货的设备
  pa_bound_empty=true           新登记不绑定任何 node
  pa_seat_after_create=1/2      名额在建号这一刻就被占掉（D2）
  pa_reserved_reported=1        授权行把 reserved 一起回传，页面不会停在旧名额
  pa_dup_name=409               同一租户内索引唯一
  pa_state_rows=1 / pa_state_config_version=0      状态里看得见，版本还是 v0
  pa_operator_no_records=true   运营方状态里不含任何一家的登记
  pa_draft=200 / pa_draft_version=1 / pa_draft_node=0 / pa_draft_no_publish=true
                                只写控制器草稿；published_version 这个键压根不存在，
                                因为没有设备可发布
  pa_draft_reread=1:pa-edge-1   重新读回同一份草稿
  pa_row_config_version=1       登记行带版本，设备页才敢写「已配置 · 草稿 v1」
  pa_publish_needs_node=400     对不存在的设备发布是被拒，不是静默成功
  pa_enroll_bad_index=403       现场输错索引 → 拒绝并报错（D3）
  pa_enroll_bad_index_says_why=yes   理由里列出可输入的正确索引
  pa_enroll=200 / pa_enroll_node=1 / pa_enroll_echoes_name=CPE-仓库门口
  pa_bound_node=1               入网即认领，登记绑到这个 node
  pa_seat_after_claim=1+0/2     名额从纸面转成真实设备，总数不变
  pa_moved_version=1 / pa_moved_no_publish=true / pa_moved_hostname=pa-edge-1
                                草稿连着版本号挪到 node 上，但不自动发布（D4）
  pa_draft_after_bind=409       已认领的登记不再接受改写
  pa_second_create=200 / pa_plain_enroll_blocked=403 / pa_blocked_says_paper=yes
  pa_revoke=200 / pa_plain_enroll_after_revoke=200  撤销释放名额

入网用的公钥是 32 字节随机数（控制器只校验长度与 base64）：这个脚本要证明的是
"授权 + 一次性码能不能放设备进来"，真设备证书链由 test-vbond.sh 里 openssl
那几条负责。码本身只在这里出现一次，后面所有断言都在确认它没有落进状态、
日志或账号视图。
"""
import base64
import http.client
import http.cookiejar
import json
import os
import re
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


def enroll(claim, serial, preauth=""):
    # 32 bytes, base64: 控制器只校验长度。见文件头说明。
    pub = base64.b64encode(os.urandom(32)).decode()
    body = json.dumps({
        "serial": serial,
        "claim_code": claim,
        "public_key": pub,
        "protocol_ver": 3,
        "agent_version": "console-flow",
        "preauth": preauth,
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
say("page", "LinkPilot 控制台" if code == 200 and "LinkPilot 控制台".encode() in raw else "missing")

# 5. 登录后读状态
code, raw = call("GET", CONSOLE + "/api/state")
st = jstat(code, raw)
say("state_tenants", len(st["tenants"]) if st else "none")

# 6. 建租户：同时下发第一个入网授权（名称 + 名额，不含码）与租户管理员账号
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
grant = res["claim"]
claim_id = grant["id"]
say("grant", claim_id)
say("grant_has_code", str("code" in grant).lower())
say("grant_no_plaintext", str("srk_" not in raw.decode(errors="replace")).lower())
say("grant_default_uses", grant["max_uses"])

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

# 10. 追加下发一条授权。运营方只看到「名称 + 名额」，看不到也生成不了码。
code, raw = call("POST", CONSOLE + "/api/claims",
                 {"tenant_id": "tenant-new", "name": "现场设备", "max_uses": 5})
second = jstat(code, raw)
if not second:
    say("new_grant", "HTTP %d: %s" % (code, raw.decode(errors="replace")))
    sys.exit(1)
say("new_grant", second["id"])
say("new_grant_name", second["name"])
say("new_grant_no_code", str("code" not in second and "code_hash" not in second).lower())
code, _ = call("POST", CONSOLE + "/api/claims/code", {"id": second["id"], "ttl_sec": 300})
say("operator_generate", code)

# 11. 租户登录，自己为这条授权生成一次性码（默认 5 分钟）
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

code, raw = call("POST", CONSOLE + "/api/claims/code",
                 {"id": second["id"], "ttl_sec": 300}, opener_=topener)
cc = jstat(code, raw)
say("generate_code", code)
code1 = cc["code"] if cc else ""
say("claim_code_shape", "ok" if re.fullmatch(r"srk_[23456789BCDFGHJKLMNPQRSTVWXZ]{32}", code1) else code1)
say("claim_code_hides_tenant", str("tenant-new" not in code1).lower())
say("claim_code_ttl", (cc["expires_at_unix"] - cc["issued_at_unix"]) if cc else "missing")
say("claim_code_name", cc.get("name") if cc else "missing")

# 12. 用码入网
code, res = enroll(code1, "vcpe-001")
say("enroll", code)
if isinstance(res, dict):
    say("enroll_tenant", res.get("tenant_id"))
    say("enroll_node", res.get("node_id"))
else:
    say("enroll_tenant", res.decode(errors="replace")[:120])

# 13. 状态里能看到它，且是 pending（入网 ≠ 上线）；码只在响应里，落库只有哈希
code, raw = call("GET", CONSOLE + "/api/state")
st = jstat(code, raw)
node = next((n for n in st["nodes"] if n["serial"] == "vcpe-001"), None) if st else None
say("node_status", node["status"] if node else "missing")
say("node_tenant", node["tenant_id"] if node else "missing")
c2 = next((c for c in st["claims"] if c["id"] == second["id"]), None) if st else None
say("claim_serials", len(c2["serials"]) if c2 else "missing")
say("claim_code_active", str(c2.get("code_active")).lower() if c2 else "missing")
body = raw.decode(errors="replace")
say("state_has_secret", str("srk_" in body).lower())
say("state_has_hash", str("code_hash" in body).lower())
say("state_claims_have_code_field",
    str(any("code" in c for c in (st["claims"] if st else []))).lower())

# 14. 轮换：旧码立刻失效，新码可用；同一台设备重入不占名额
code, raw = call("POST", CONSOLE + "/api/claims/code",
                 {"id": second["id"], "ttl_sec": 300}, opener_=topener)
code2 = (jstat(code, raw) or {}).get("code", "")
say("rotate_code", code)
say("rotate_changed", str(code1 != code2).lower())
say("rotate_old_code", enroll(code1, "vcpe-001b")[0])
say("rotate_existing_device", enroll(code2, "vcpe-001")[0])

# 15. 吊销按 id，并让当前码立刻作废
code, _ = call("POST", CONSOLE + "/api/claims/revoke", {"id": second["id"]})
say("revoke", code)
code, raw = call("GET", CONSOLE + "/api/state")
st = jstat(code, raw)
c2 = next((c for c in st["claims"] if c["id"] == second["id"]), None) if st else None
say("claim_revoked", str(c2["revoked"]).lower() if c2 else "missing")
say("revoke_code_active", str(c2.get("code_active")).lower() if c2 else "missing")
say("enroll_revoked", enroll(code2, "vcpe-002")[0])

# 16. 到期由服务端判定：1 秒窗口的码，过了就进不了网
code, raw = call("POST", CONSOLE + "/api/claims",
                 {"tenant_id": "tenant-new", "name": "短时测试", "max_uses": 2})
short = jstat(code, raw)
code, raw = call("POST", CONSOLE + "/api/claims/code",
                 {"id": short["id"], "ttl_sec": 1}, opener_=topener)
sc = jstat(code, raw)
say("expiry_code", code)
time.sleep(2.2)
say("expiry_enroll", enroll(sc["code"], "vcpe-short")[0])
code, raw = call("GET", CONSOLE + "/api/state")
st = jstat(code, raw)
cs = next((c for c in st["claims"] if c["id"] == short["id"]), None) if st else None
say("expiry_code_active", str(cs.get("code_active")).lower() if cs else "missing")
code, _ = call("POST", CONSOLE + "/api/claims/code",
               {"id": short["id"], "ttl_sec": 86401}, opener_=topener)
say("expiry_bad_ttl", code)
code, raw = call("POST", CONSOLE + "/api/claims/code",
                 {"id": short["id"]}, opener_=topener)
sdef = jstat(code, raw)
say("expiry_default_ttl", (sdef["expires_at_unix"] - sdef["issued_at_unix"]) if sdef else "missing")

# 17. 名额按设备计：租户账号持有 2 个名额的授权
code, raw = call("POST", CONSOLE + "/api/tenants", {
    "tenant_id": "tenant-quota", "account": "tenant-quota-admin",
    "password": "quota-pass-2026", "claim_max_uses": 2,
})
res = jstat(code, raw)
say("quota_tenant", res["tenant"]["tenant_id"] if res else "HTTP %d" % code)
quota_id = res["claim"]["id"] if res else ""
say("quota_tenant_claim_uses", res["claim"]["max_uses"] if res else "missing")
qjar = http.cookiejar.CookieJar()
qopener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(qjar))
call("POST", CONSOLE + "/api/login",
     {"username": "tenant-quota-admin", "password": "quota-pass-2026"}, opener_=qopener)
code, raw = call("POST", CONSOLE + "/api/claims/code",
                 {"id": quota_id, "ttl_sec": 300}, opener_=qopener)
res = jstat(code, raw)
quota = res["code"] if res else ""
say("quota_generate", code)

say("quota_enroll_1", enroll(quota, "vcpe-q1")[0])
say("quota_enroll_2", enroll(quota, "vcpe-q2")[0])
say("quota_enroll_3_blocked", enroll(quota, "vcpe-q3")[0])
# 同一台设备重入不消耗第二个名额，否则一次重启循环就能把码用光
say("quota_reenroll_existing", enroll(quota, "vcpe-q1")[0])
code, raw = call("GET", CONSOLE + "/api/state")
st = jstat(code, raw)
cq = next((c for c in st["claims"] if c["id"] == quota_id), None) if st else None
say("quota_used", cq["uses"] if cq else "missing")

# 18. 名额可以调整，但不能调到已激活数以下（那台设备的证书已经发出去了）
code, _ = call("POST", CONSOLE + "/api/claims/update", {"id": quota_id, "max_uses": 1})
say("quota_lower", code)
code, raw = call("POST", CONSOLE + "/api/claims/update", {"id": quota_id, "max_uses": 3})
res = jstat(code, raw)
say("quota_raise", code)
say("quota_raise_value", res["max_uses"] if res else "missing")
say("quota_enroll_after_raise", enroll(quota, "vcpe-q3")[0])
code, _ = call("POST", CONSOLE + "/api/claims/update",
               {"id": "clm_ZZZZZZZZZZZZ", "max_uses": 2})
say("quota_update_unknown", code)
code, _ = call("POST", CONSOLE + "/api/claims/update", {"id": quota_id, "max_uses": 10001})
say("quota_update_too_big", code)
code, raw = call("POST", CONSOLE + "/api/claims/update", {"id": quota_id, "max_uses": 0})
res = jstat(code, raw)
say("quota_update_zero", code)
say("quota_update_zero_value", res["max_uses"] if res else "missing")

# ---------------------------------------------------------------- B. 多角色
# B1. 第二家租户：有账号、没有设备（空集合契约）；也能为自己的授权生成码
code, raw = call("POST", CONSOLE + "/api/tenants", {
    "tenant_id": "tenant-other", "name": "另一家",
    "account": "tenant-other-admin", "password": "other-side-2026",
})
res = jstat(code, raw)
say("other_tenant", res["tenant"]["tenant_id"] if res else "HTTP %d" % code)
other_grant = res["claim"]["id"] if res else ""
say("other_grant", other_grant)
ojar = http.cookiejar.CookieJar()
oopener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(ojar))
call("POST", CONSOLE + "/api/login",
     {"username": "tenant-other-admin", "password": "other-side-2026"}, opener_=oopener)
code, _ = call("POST", CONSOLE + "/api/claims/code",
               {"id": other_grant, "ttl_sec": 60}, opener_=oopener)
say("other_tenant_generate", code)
# 别家的授权是 404：不确认它存在，更不给出码
code, _ = call("POST", CONSOLE + "/api/claims/code",
               {"id": other_grant}, opener_=topener)
say("tenant_foreign_grant_code", code)

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

# B5. 租户会话已经建立（A 段登录），这里只确认会话仍有效
code, raw = call("GET", CONSOLE + "/api/session", opener_=topener)
say("tenant_session", code)

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
tclaim = next((c for c in tclaims if c["id"] == claim_id), None)
say("tenant_claim_seats", "%d/%d" % (tclaim["uses"], tclaim["max_uses"]) if tclaim else "missing")
say("tenant_node_runtime", str(bool(tnodes and tnodes[0].get("runtime"))).lower())
# 入网不等于配置：用真实的配置版本口径来断言，而不是页面上一个写死的字样。
# 新入网的设备既没有草稿（version）也没有已发布（published_version omitempty→0）。
tnode_id = tnodes[0].get("node_id") if tnodes else 0
cfg_code, cfg_raw = call("GET", "%s/api/config?node=%s" % (CONSOLE, tnode_id), opener_=topener)
cvt = jstat(cfg_code, cfg_raw) or {}
say("tenant_config_http", cfg_code)
say("tenant_config_draft", cvt.get("version"))
say("tenant_config_published", cvt.get("published_version", 0))

# B6b. 监控：租户专用；没上报遥测时报空集合，而不是编一行
code, raw = call("GET", CONSOLE + "/api/metrics", opener_=topener)
met = jstat(code, raw) or {}
mnodes = met.get("nodes") or []
say("metrics_tenant", code)
say("metrics_nodes", len(mnodes))
say("metrics_foreign_nodes", len([n for n in mnodes if n.get("serial") != "vcpe-001"]))
say("metrics_status", mnodes[0].get("status") if mnodes else "missing")
say("metrics_uptime", mnodes[0].get("uptime_sec") if mnodes else "missing")
say("metrics_window",
    "none" if mnodes and mnodes[0].get("telemetry_seen_at_unix") == 0 else "reported")
say("metrics_tunnels_empty",
    str(bool(mnodes)
        and all(isinstance(n.get("tunnels"), list) for n in mnodes)
        and all(len(n.get("tunnels") or []) == 0 for n in mnodes)).lower())
say("metrics_ttl", met.get("metrics_ttl_sec"))
code, _ = call("GET", CONSOLE + "/api/metrics", cookie=cookie_header(jar))
say("metrics_operator", code)
# 不带任何会话：默认 opener 还挂着运营方的 cookie，必须用无 cookie 的 opener。
code, _ = call("GET", CONSOLE + "/api/metrics", opener_=plain)
say("metrics_unauth", code)

# B7. 运营方状态：设备行里不该有 runtime / 端口 / 配置
code, raw = call("GET", CONSOLE + "/api/state")
ost = jstat(code, raw) or {}
onodes = ost.get("nodes") or []
say("operator_node_runtime", str(any("runtime" in n for n in onodes)).lower())
say("operator_config_leak",
    str(any(k in raw.decode(errors="replace")
            for k in ("udp_mapping", "vpn_port", "site_id"))).lower())

# B8. 租户不能做运营方的任何事（403 是明确告知，不是隐藏 404）
def deny(name, path, body):
    code, _ = call("POST", CONSOLE + path, body, opener_=topener)
    say(name, code)

deny("tenant_deny_tenants", "/api/tenants", {"tenant_id": "evil"})
deny("tenant_deny_claims", "/api/claims", {"tenant_id": "tenant-new"})
deny("tenant_deny_revoke", "/api/claims/revoke", {"id": claim_id})
deny("tenant_deny_claim_update", "/api/claims/update", {"id": claim_id, "max_uses": 999})
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
    call("POST", CONSOLE + "/api/claims", {"tenant_id": "tenant-other", "name": "sse-foreign"})
    call("POST", CONSOLE + "/api/claims", {"tenant_id": "tenant-new", "name": "sse-own"})
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

# ---------------------------------------------------------------- C. 预授权
# 设备还没到货，先在控制器上登记 + 配置；盒子到货时现场多带一个索引参数认领。
# 这一段就是照着 GUI 上「资产 › 设备 › 预授权」那几步走的：建登记（占名额）→
# 写配置（只有草稿）→ 生成码 → 用 --preauth 入网 → 配置跟着 node 走但不自动发布。
code, raw = call("POST", CONSOLE + "/api/tenants", {
    "tenant_id": "tenant-pa", "name": "预授权租户",
    "account": "tenant-pa-admin", "password": "tenant-pa-pass-2026",
    "claim_max_uses": 2,
})
pa = jstat(code, raw)
if not pa:
    say("pa_tenant", "HTTP %d: %s" % (code, raw.decode(errors="replace")))
    sys.exit(1)
pa_claim = pa["claim"]["id"]
say("pa_tenant", pa["tenant"]["tenant_id"])
say("pa_grant", pa_claim)

pjar = http.cookiejar.CookieJar()
popener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(pjar))
code, _ = call("POST", CONSOLE + "/api/login",
               {"username": "tenant-pa-admin", "password": "tenant-pa-pass-2026"},
               opener_=popener)
say("pa_login", code)
if code != 200:
    sys.exit(1)

# C1. 建一条登记：名称是现场要输入的索引，序列号可选（填了就必须对上）
code, raw = call("POST", CONSOLE + "/api/preauth",
                 {"claim_id": pa_claim, "name": "CPE-仓库门口", "serial": "SN-PA-1"},
                 opener_=popener)
pres = jstat(code, raw)
say("pa_create", code)
if not pres or not pres.get("preauth"):
    say("pa_create_body", raw.decode(errors="replace")[:200])
    sys.exit(1)
pa_id = pres["preauth"]["id"]
say("pa_id", pa_id)
say("pa_bound_empty", str(not pres["preauth"].get("bound_node_id")).lower())
# 名额在建登记的这一刻就被占掉（D2），而且响应里要把新的授权行带回来，
# 否则页面上的「已入网 x + 预授权 y」会停在写入前的旧数字。
pc = [c for c in pres.get("claims", []) if c["id"] == pa_claim]
say("pa_seat_after_create",
    "%s/%s" % (pc[0]["uses"] + pc[0]["reserved"], pc[0]["max_uses"]) if pc else "no-claims")
say("pa_reserved_reported", pc[0]["reserved"] if pc else "no-claims")

# C2. 名称在同一租户里唯一：重名是 409，不是"再建一条"
code, _ = call("POST", CONSOLE + "/api/preauth",
               {"claim_id": pa_claim, "name": "CPE-仓库门口"}, opener_=popener)
say("pa_dup_name", code)

# C3. 状态里租户看得到这条登记；运营方看不到任何一家的登记
code, raw = call("GET", CONSOLE + "/api/state", opener_=popener)
pst = jstat(code, raw) or {}
rows = [r for r in (pst.get("preauths") or []) if r["id"] == pa_id]
say("pa_state_rows", len(rows))
say("pa_state_config_version", rows[0].get("config_version") if rows else "none")
code, raw = call("GET", CONSOLE + "/api/state")
ostaraw = raw.decode(errors="replace")
say("pa_operator_no_records", str(pa_id not in ostaraw).lower())

# C4. 给它写配置：只有草稿，没有发布（接收方还不存在）
pa_cfg = {"hostname": "pa-edge-1",
          "vpns": [{"id": 0, "name": "default"}],
          "ifaces": [], "statics": []}
code, raw = call("PUT", CONSOLE + "/api/preauth/config",
                 {"preauth_id": pa_id, "config": pa_cfg}, opener_=popener)
cview = jstat(code, raw) or {}
say("pa_draft", code)
say("pa_draft_version", cview.get("version"))
say("pa_draft_node", cview.get("node_id"))
# published_version 是 omitempty：键根本不存在，才说明这条草稿没有任何发布目标。
say("pa_draft_no_publish", str("published_version" not in cview).lower())
code, raw = call("GET", CONSOLE + "/api/preauth/config?preauth=" + urllib.parse.quote(pa_id),
                 opener_=popener)
gview = jstat(code, raw) or {}
say("pa_draft_reread", "%s:%s" % (gview.get("version"),
                                  (gview.get("config") or {}).get("hostname")))
# 设备页现在能说「已配置 · 草稿 vN」，而不是假称有一台离线设备
code, raw = call("GET", CONSOLE + "/api/preauth", opener_=popener)
prow = [r for r in (jstat(code, raw) or {}).get("preauths", []) if r["id"] == pa_id]
say("pa_row_config_version", prow[0].get("config_version") if prow else "none")

# C5. 没有 node 就没有发布这件事：400 而不是"发了一条不存在的设备"
code, _ = call("POST", CONSOLE + "/api/config/publish",
               {"node_id": 0}, opener_=popener)
say("pa_publish_needs_node", code)

# C6. 认领之前，先用错索引入网：必须拒绝并说清原因（D3），不能静默当普通入网
code, raw = call("POST", CONSOLE + "/api/claims/code", {"id": pa_claim, "ttl_sec": 600},
                 opener_=popener)
pa_code = (jstat(code, raw) or {}).get("code", "")
ec, eb = enroll(pa_code, "SN-WRONG-INDEX", "CPE-不存在的名字")
say("pa_enroll_bad_index", ec)
why = eb.decode(errors="replace") if isinstance(eb, bytes) else str(eb)
say("pa_enroll_bad_index_says_why",
    "yes" if ("没有名为" in why and "CPE-仓库门口" in why) else why[:160])

# C7. 正确索引 + 正确序列号：入网即认领，配置跟着 node 走（D5），但不自动发布（D4）
ec, eb = enroll(pa_code, "SN-PA-1", "CPE-仓库门口")
say("pa_enroll", ec)
pa_node = (eb or {}).get("node_id") if isinstance(eb, dict) else "none"
say("pa_enroll_node", pa_node)
say("pa_enroll_echoes_name", str((eb or {}).get("preauth_name") if isinstance(eb, dict) else ""))

code, raw = call("GET", CONSOLE + "/api/state", opener_=popener)
bst = jstat(code, raw) or {}
brow = [r for r in (bst.get("preauths") or []) if r["id"] == pa_id]
say("pa_bound_node", brow[0].get("bound_node_id") if brow else "none")
bclaim = [c for c in (bst.get("claims") or []) if c["id"] == pa_claim]
say("pa_seat_after_claim",
    "%s+%s/%s" % (bclaim[0]["uses"], bclaim[0]["reserved"], bclaim[0]["max_uses"]) if bclaim else "none")

code, raw = call("GET", CONSOLE + "/api/config?node=%s" % pa_node, opener_=popener)
nview = jstat(code, raw) or {}
say("pa_moved_version", nview.get("version"))
say("pa_moved_no_publish", str("published_version" not in nview).lower())
say("pa_moved_hostname", (nview.get("config") or {}).get("hostname"))
say("pa_moved_serial", nview.get("serial"))
# 草稿只有份：认领之后原来那条不能再被改写，避免页面上出现两份配置
code, _ = call("PUT", CONSOLE + "/api/preauth/config",
               {"preauth_id": pa_id, "config": pa_cfg}, opener_=popener)
say("pa_draft_after_bind", code)

# C8. 纸面设备也占名额（D2）：登记第二条之后已满，普通入网必须被拒并且说清
# 是哪一半被纸占着；撤销那条登记，名额立刻释放，入网就能过。
code, raw = call("POST", CONSOLE + "/api/preauth",
                 {"claim_id": pa_claim, "name": "CPE-第二台"}, opener_=popener)
pa2 = (jstat(code, raw) or {}).get("preauth") or {}
say("pa_second_create", code)
ec3, eb3 = enroll(pa_code, "SN-IMPULSE")
say("pa_plain_enroll_blocked", ec3)
why3 = eb3.decode(errors="replace") if isinstance(eb3, bytes) else str(eb3)
say("pa_blocked_says_paper",
    "yes" if ("预授权未认领 1 个" in why3 and "2/2" in why3 and "撤销一条预授权" in why3) else why3[:160])
code, _ = call("POST", CONSOLE + "/api/preauth/revoke", {"id": pa2.get("id", "")},
               opener_=popener)
say("pa_revoke", code)
ec4, _ = enroll(pa_code, "SN-IMPULSE")
say("pa_plain_enroll_after_revoke", ec4)
