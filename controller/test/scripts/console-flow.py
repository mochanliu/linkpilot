#!/usr/bin/env python3
"""console-flow.py -- 控制台管理面的端到端脚本（只用标准库）。

跑在控制器容器里：控制台在 http://127.0.0.1:8088（明文 + token 会话），
设备入网在 https://127.0.0.1:8444（mTLS，但 enroll 这一步没有证书）。

它做的事就是客户在 GUI 里点的那几步，一步一条输出行（name=value），
由 test-vbond.sh 逐条断言：

  unauth_state=401          没有会话读状态必须被拒
  bad_login=401             错误 token 必须被拒
  login=200                 正确 token 换到会话 cookie
  page=SROU 控制台            单页控制台能取到
  state_tenants=<n>         登录后能读到状态
  tenant=tenant-new         建租户
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

入网用的公钥是 32 字节随机数（控制器只校验长度与 base64）：这个脚本要证明的是
"claim code 能不能授权入网"，真设备证书链由 test-vbond.sh 里 openssl 那几条负责。
"""
import base64
import http.cookiejar
import json
import os
import ssl
import sys
import urllib.error
import urllib.request

CONSOLE = os.environ.get("CONSOLE", "http://127.0.0.1:8088")
CTRL = os.environ.get("CTRL", "https://127.0.0.1:8444")
CA = os.environ.get("CA", "/tmp/ctrl-data/ca.pem")
TOKEN = sys.argv[1] if len(sys.argv) > 1 else ""

jar = http.cookiejar.CookieJar()
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
tls = ssl.create_default_context(cafile=CA)


def call(method, url, body=None, json_ct=True, opener_=opener):
    data = None
    req = urllib.request.Request(url, method=method)
    if body is not None:
        data = json.dumps(body).encode()
        if json_ct:
            req.add_header("Content-Type", "application/json")
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
    plain = urllib.request.build_opener(urllib.request.HTTPSHandler(context=tls))
    try:
        with plain.open(req, timeout=10) as r:
            return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except urllib.error.URLError as e:
        return 0, str(e).encode()


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

# 6. 建租户（同时得到第一个入网码）
code, raw = call("POST", CONSOLE + "/api/tenants", {"tenant_id": "tenant-new", "name": "新建租户"})
res = jstat(code, raw)
if not res:
    say("tenant", "HTTP %d: %s" % (code, raw.decode(errors="replace")))
    sys.exit(1)
say("tenant", res["tenant"]["tenant_id"])
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
