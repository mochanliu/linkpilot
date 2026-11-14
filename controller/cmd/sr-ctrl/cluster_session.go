package main

// cluster_session.go —— 会话在节点之间"过手"，而不是共享一张表。
//
// 背景：控制台的会话是**每个节点各自内存**里的一张表（admin.sess）。集群之后
// leaderGuard 会把写请求 307 到 leader，可 leader 从来没见过这个会话 id ——
// 于是"在 follower 上点发布"的结局是 401（2026-11-14 lab 实测）。这不是
// 配置问题，是两条设计撞在一起：写只认 leader、会话只认本机。
//
// 解法选了最轻的一条：**不做共享会话表，也不让会话进共识**（会话是秘密且
// 高频，进共识既贵又扩大泄露面）。follower 在 307 的时候，把"这个请求是谁"
// 用集群密钥签一张 60 秒的一次性断言，放在 X-Cluster-Session 头里；leader
// 收到时先按本地会话查，查不到再验这张断言。
//
// 三条性质：
//  1. 无状态：断言自带身份和过期时间，leader 不需要反查 follower；
//  2. 有时效：60 秒只够一次重定向，过期的断言不能当长期凭证用；
//  3. 边界不变：能伪造断言的前提是拿到集群密钥，而拿到密钥的节点本来就是
//     集群成员，没有新增信任方。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

const clusterSessionHeader = "X-Cluster-Session"

// xClusterForwarded 是本机内部标记（不出网）：proxyToLeader 成功转发后打在
// 请求对象上，auditMiddleware 据此跳过本地台账 —— 这次写的审计行由 leader
// 记录并复制过来，本机再记就是每个控制台上两行重复。
const xClusterForwarded = "X-Cluster-Forwarded"

// sessionAssertionTTL 只覆盖"重定向这一下"。页面的每一次写都会重新 307 一次，
// 所以不需要长；短一点，泄露的窗口就小一点。
const sessionAssertionTTL = 60 * time.Second

type sessionAssertion struct {
	Role   string `json:"r"`
	Tenant string `json:"t,omitempty"`
	User   string `json:"u,omitempty"`
	Via    string `json:"v,omitempty"`
	Node   string `json:"n,omitempty"`
	Exp    int64  `json:"e"`
}

func (s *server) signSession(id consoleIdentity) string {
	sec := s.clusterSecret()
	if sec == "" || id.Role == "" {
		return ""
	}
	payload, err := json.Marshal(sessionAssertion{
		Role: id.Role, Tenant: id.TenantID, User: id.Username, Via: id.Via,
		Node: s.nodeID, Exp: time.Now().Add(sessionAssertionTTL).Unix(),
	})
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(sec))
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verifySession 把断言还原成一个身份。任何一步不对都返回零身份（= 未登录），
// 调用方只有"认"与"不认"两种选择，不存在"半信半疑"。
func (s *server) verifySession(v string) consoleIdentity {
	sec := s.clusterSecret()
	if sec == "" || v == "" {
		return consoleIdentity{}
	}
	enc, sig, ok := strings.Cut(v, ".")
	if !ok {
		return consoleIdentity{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return consoleIdentity{}
	}
	want, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return consoleIdentity{}
	}
	mac := hmac.New(sha256.New, []byte(sec))
	mac.Write(payload)
	if !hmac.Equal(mac.Sum(nil), want) {
		return consoleIdentity{}
	}
	var a sessionAssertion
	if err := json.Unmarshal(payload, &a); err != nil {
		return consoleIdentity{}
	}
	if a.Exp == 0 || time.Now().Unix() > a.Exp {
		return consoleIdentity{}
	}
	return consoleIdentity{
		Role: a.Role, TenantID: a.Tenant, Username: a.User, Via: a.Via,
		expires: time.Unix(a.Exp, 0),
	}
}
