package main

import (
	"strings"
	"testing"
)

// 拓扑图的四处接线都是拼字符串，断一处没有任何运行期报错：
//
//  1. Dashboard 嵌图：undDashboardMap 要真的被拼进 viewDashboard，还要留一个
//     回完整拓扑页的按钮——嵌了图却没有入口，找站点编辑页要靠翻导航。
//  2. 两页共用同一块面板（undMapPanel）：两块各画各的地图，迟早各自演化，
//     运维就会怀疑两处读的不是同一份数据。
//  3. 同站点聚簇：同一站点的设备坐标完全相同，不散开就是一个点压一个点，
//     看起来像少了几台——owner 实测"图上只有 3 台"就是这个。
//  4. 明细海岸线按缩放切（MAP_GEO50）：适应设备后缩放常落在 2.5~4.5 之间，
//     110m 海岸线在这个档位粗得像错位。
func TestConsoleTopologyWiring(t *testing.T) {
	html := string(consoleHTML)
	for _, want := range []string{
		"function undDashboardMap(",
		"undDashboardMap() +",
		`data-act="goto" data-view="underlay-topology"`,
		"function undMapPanel(",
		"h += undMapPanel(doc, t);",
		"var MAP_GEO50 = 2.5;",
		"cluster[id] = true;",
		`} else if (lv >= 2 || (cluster && cluster[nd.node_id])) {`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("console.html 缺少 %s", want)
		}
	}
	// 聚簇的散开半径必须是屏幕恒定的（inv 换算），否则缩放后两台设备会重新叠住。
	if !strings.Contains(html, "var R = 13 * inv;") {
		t.Error("聚簇散开半径没有用 inv 换算")
	}
}
