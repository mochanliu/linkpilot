package main

// cluster_test.go —— 成员表的边界规则。
//
// 这一层最容易出事的地方不是共识，是"人点错了"：同一个地址加两次、把加入码
// 写进 systemd、把已下线的机器再拉起来。共识层对这些错误的反应都是"看起来
// 在跑，其实不对"，所以规则要在这里用断言钉住。

import (
	"strings"
	"testing"
)

func TestEndpointConflict(t *testing.T) {
	members := []ClusterMember{
		{ID: "c-a", Name: "c1", Endpoint: "10.0.0.1:8443", Role: memberVoter, Status: memberActive},
		{ID: "c-b", Name: "c2", Endpoint: "10.0.0.2:8443", Role: memberVoter, Status: memberActive},
		{ID: "c-old", Name: "c3", Endpoint: "10.0.0.3:8443", Role: "", Status: memberDecommissioned},
	}

	cases := []struct {
		name      string
		endpoint  string
		replaceID string
		wantHit   bool // true = 必须被拒
	}{
		{"新地址随便加", "10.0.0.9:8443", "", false},
		{"撞上活着的 voter", "10.0.0.1:8443", "", true},
		{"撞上活着的 learner 也一样", "10.0.0.2:8443", "", true},
		// 现场最常见的动作：旧机器坏了，新机器直接接管它的 IP。
		{"替换目标可以复用地址", "10.0.0.1:8443", "c-a", false},
		// 但"替换 A 却去占 B 的地址"是点错了，照样拦。
		{"替换 A 占 B 的地址要拦", "10.0.0.2:8443", "c-a", true},
		// 已下线的成员不占地址（行留着只做审计）。
		{"已下线的地址可以复用", "10.0.0.3:8443", "", false},
		// 写法差异不该让人踩雷：大小写、尾部斜杠、前后空格。
		{"大小写与斜杠等价", "  10.0.0.1:8443/ ", "", true},
		{"空地址不算冲突", "", "", false},
	}

	for _, c := range cases {
		got := endpointConflict(members, c.endpoint, c.replaceID)
		if c.wantHit && got == "" {
			t.Errorf("%s: 期望被拒，实际放行", c.name)
		}
		if !c.wantHit && got != "" {
			t.Errorf("%s: 期望放行，实际被拒：%s", c.name, got)
		}
		if c.wantHit && !strings.Contains(got, "替换") {
			t.Errorf("%s: 拒绝时要说清怎么绕过去（提到「替换」），实际：%s", c.name, got)
		}
	}
}
