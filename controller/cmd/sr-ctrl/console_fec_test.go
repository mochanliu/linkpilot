package main

import (
	"strings"
	"testing"
)

// FEC 在页面上是"勾选框 + 档位下拉"，不是"下拉里塞一个「关」"。
//
// 这不是口味问题：勾选框是唯一的开关，所以"这次提交 0"这件事只可能来自它；
// 哪天有人把开关改回下拉，`f.get("fec_on")` 就永远拿不到值，而页面不会报错——
// 隧道会静默地一直关着 FEC，或者更糟：编辑一条已经开着 1:2 的隧道时把它关掉。
//
// 控制台是拼字符串拼出来的，少一个 id 就只是那个框不动，没有任何运行期报错，
// 所以这里把四面接线钉死：两处标记（新建/编辑）、两处联动键、提交取值函数。
func TestConsoleFecToggleIsWired(t *testing.T) {
	html := string(consoleHTML)
	for _, want := range []string{
		`type="checkbox" id="und-tun-fec-on" name="fec_on"`,  // 新建表单的勾选框
		`type="checkbox" id="und-edit-fec-on" name="fec_on"`, // 编辑表单的勾选框
		`data-fec-toggle="und-tun-fec"`,                      // 勾选框 → 下拉的联动键
		`data-fec-toggle="und-edit-fec"`,
		`function undFecOptions(`, // 1..4 四档（不含"关"）
		`function undFecValue(`,   // 勾选框是开关，档位只在勾上时读
		`body.fec_ratio = undFecValue(f);`,
		`input[data-fec-toggle]`, // 每次重建 DOM 后重新挂联动
	} {
		if !strings.Contains(html, want) {
			t.Errorf("console.html 缺少 %s", want)
		}
	}

	// 四档的档位与带宽代价都写在标签里：操作者不该为了算 1+1/n 去翻文档。
	for _, want := range []string{"1:1", "1:2", "1:3", "1:4", "带宽×1.5", "带宽×1.25"} {
		if !strings.Contains(html, want) {
			t.Errorf("FEC 档位标签缺少 %s", want)
		}
	}

	// 编辑表单里"已经开着"的隧道必须回填成勾上 + 选中的档位，否则一保存就把
	// FEC 关掉了（表单总是提交，0 = 关）。
	if !strings.Contains(html, `(t.fec_ratio ? " checked" : "")`) {
		t.Error("编辑表单没有按 fec_ratio 回填勾选框")
	}
	if !strings.Contains(html, `undFecOptions(t.fec_ratio || 2)`) {
		t.Error("编辑表单没有按 fec_ratio 回填档位")
	}
}
