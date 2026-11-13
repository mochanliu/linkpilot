package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// 2026-11-13 owner 报「点 SD-WAN 策略没反应，像卡住了」。根因在服务端：
// DTO 里的候选颜色是 []uint8 —— 而 Go 的 uint8 就是 byte，encoding/json 把
// []uint8 编成 **base64 字符串**（"AgM="），不是数组。页面对它做
// .join/.forEach/.indexOf，render 在 viewPolicy 里抛 TypeError；render 是唯一
// 的重绘入口，异常放出去以后每次轮询都再抛一次，整页就停在上一个画面。
//
// 这两条契约由此钉死：
//  1. colors 在 JSON 里必须是数字数组；
//  2. 控制台渲染一页失败要"看得见"（viewCrash），且 node 不匹配时不能
//     清了重拉形成微任务死环。
func TestPolicyViewColorsAreJSONArrays(t *testing.T) {
	v := policyView{
		Rows:  []policyRowView{{ID: 1, Mode: "dynamic", Peer: 3, Colors: policyColorInts([]uint8{2, 3})}},
		Peers: []policyPeerRow{{NodeID: 3, Colors: policyColorInts([]uint8{2, 3})}},
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"colors":[2,3]`) {
		t.Fatalf("colors 没有被编成数字数组：%s", s)
	}
	// []uint8 的 base64 形状是 "colors":"AgM=" —— 一个带引号的字符串。
	if strings.Contains(s, `"colors":"`) {
		t.Fatalf("colors 被编成了字符串（[]uint8 的 base64 形状）：%s", s)
	}
}

func TestConsolePolicyPageWiring(t *testing.T) {
	html := string(consoleHTML)
	for _, want := range []string{
		// render 的 catch：一页抛异常要落到 viewCrash，而不是把异常放出去
		// 让整个页面（导航 + 轮询 + SSE）一起停摆。
		"c = viewCrash(S.view, err);",
		"function viewCrash(view, err) {",
		// node 不匹配时停止重试：清 doc 再拉是一个只在微任务里打转的环。
		"!o.loading && !o.mismatch &&",
		"o.mismatch = true;",
		"o.mismatch = false; // 换了设备",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("console.html 缺少 %s", want)
		}
	}
}
