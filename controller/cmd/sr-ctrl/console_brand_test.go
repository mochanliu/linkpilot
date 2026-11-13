package main

import (
	"strings"
	"testing"
)

// 产品名在页面上只出现三处：<title>、登录卡片的品牌行、侧栏 logo。
//
// 三处都是纯字符串，没有运行期校验：改一处漏一处，用户就在同一个页面上看到两个
// 产品名（浏览器标签是新的、侧栏还是旧的）。外部 harness（console-flow.py、
// test-vbond.sh）也只认 <title> 里那一个，漏改侧栏不会有任何测试报错。
//
// 所以这里把三处一起钉住，同时钉住旧名不再回来——产品改叫 LinkPilot 之后，
// 代码标识符（sr-*、vbond）按 docs/architecture.md 的命名约定保留，只有界面文案改。
func TestConsoleBrandIsLinkPilot(t *testing.T) {
	html := string(consoleHTML)
	for _, want := range []string{
		"<title>LinkPilot 控制台</title>",
		`<b>LinkPilot 控制台</b>`,
		`<div class="logo">LinkPilot<small>SD-WAN CONTROLLER</small></div>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("console.html 缺少 %s", want)
		}
	}
	if strings.Contains(html, "SROU") {
		t.Error("console.html 里还有旧产品名 SROU")
	}
}
