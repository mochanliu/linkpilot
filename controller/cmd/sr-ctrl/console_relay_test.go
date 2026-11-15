package main

import (
	"strings"
	"testing"
)

// 中转链编辑器标题必须写明**从哪一端出发**。
//
// 「第 1 跳先经过」这句话在有 A/B 两端的隧道上是有歧义的：链只存一个方向
// （A→B），B 端拿到的是镜像，所以站在对端看，"第 1 跳"是链尾那台。页面不改
// 一个字，改链的人就会以为回程还得另配一条，或者把顺序填反了却没有任何提示
// —— 反了照样能"通"（在平面网络里），只是回程绕到了别的盒子上。
//
// 控制台是拼字符串拼出来的，标题拼错不会有任何运行期报错，所以这里把口径
// 钉在参数上：标题取 o.from，隧道表单传「本端」，模板页退回「隧道 A 端」。
func TestConsoleRelayChainSaysWhereItStarts(t *testing.T) {
	html := string(consoleHTML)

	for _, want := range []string{
		`esc(o.from || "隧道 A 端")`,                             // 标题文字只有一个来源
		`{ exclude: exclude, from: "本端" }`,                    // 新建表单：本端 = 隧道的 A 端
		`{ exclude: [t.a.node_id, t.b.node_id], from: "本端" }`, // 编辑表单同理
		`回程不用另配`,                                              // 新建时把"镜像"这件事说出来
	} {
		if !strings.Contains(html, want) {
			t.Errorf("console.html 缺少 %s", want)
		}
	}

	// 旧的、不带方向的标题不许回来：它就是这条歧义的来源。
	if strings.Contains(html, "<h3>中转链 · 第 1 跳先经过</h3>") {
		t.Error("中转链标题退回成不写起点的老文案了")
	}
}
