package frr

import (
	"net/netip"
	"testing"
	"time"
)

// TestImporterLinkWindowSurvivesSessionDrop 钉住链路事件那一拍（实测 2026-11-06，
// lab-cpe3：VPP 重启 → lcp/vpp0 down → 客户 BGP 会话断 → 15s 后才恢复）。
// 会话断掉时 zebra 会把这一批前缀逐条撤销；如果按撤销照直上报，全网会看到
// "本站点没有前缀"，sr-route 从此把本盒自己的前缀当远端路由导出，注入再把
// 客户真路径顶掉 —— 通知永不回来。窗口必须让旧集合活着跨过整段恢复时间。
func TestImporterLinkWindowSurvivesSessionDrop(t *testing.T) {
	im, clock := newClockImporter()
	rules := []ImportRule{allowAll(0)}
	p1 := netip.MustParsePrefix("172.16.3.1/32")
	p2 := netip.MustParsePrefix("10.68.1.0/24")
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p1, Add: true})
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p2, Add: true})

	im.BeginLinkWindow()
	// 会话断：撤销通知到得比窗口关闭早。
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p1, Add: false})
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p2, Add: false})
	im.Tick(clock.now())
	if got := reported(t, im, rules); len(got) != 2 {
		t.Fatalf("链路窗口里撤销通知把集合清空了：%v，want 2 条旧集合", got)
	}

	// 会话 15s 后恢复，回放把两条带回来；安静 500ms 就该关窗，不漏撤。
	clock.advance(15 * time.Second)
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p1, Add: true})
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p2, Add: true})
	clock.advance(resyncQuiet + time.Millisecond)
	im.Tick(clock.now())
	if im.resync {
		t.Fatal("回放已经静默，窗口还开着")
	}
	if got := reported(t, im, rules); len(got) != 2 {
		t.Fatalf("关窗后 prefixes = %v, want 2 条", got)
	}
}

// 会话真的再也没回来：窗口到 linkRecovery 上限按收到的（空）收尾，该撤还是得撤。
func TestImporterLinkWindowClosesAtDeadline(t *testing.T) {
	im, clock := newClockImporter()
	rules := []ImportRule{allowAll(0)}
	p := netip.MustParsePrefix("172.16.3.1/32")
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p, Add: true})

	im.BeginLinkWindow()
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p, Add: false})
	clock.advance(linkRecovery)
	im.Tick(clock.now())
	if im.resync {
		t.Fatal("到 linkRecovery 上限后窗口还开着")
	}
	if got := reported(t, im, rules); len(got) != 0 {
		t.Fatalf("会话没恢复时前缀必须最终撤掉，实际还在报 %v", got)
	}
}

// 读先于写的判据来自 Settled：第一轮对账没落地前它必须是 false。
func TestImporterSettledOnlyAfterFirstWindow(t *testing.T) {
	im, clock := newClockImporter()
	p := netip.MustParsePrefix("10.68.1.0/24")
	if im.Settled() {
		t.Fatal("刚建好就 Settled，导出侧会以为可以加注入")
	}
	im.BeginResync()
	im.Handle(RouteEvent{Type: RouteBGP, Prefix: p, Add: true})
	if im.Settled() {
		t.Fatal("窗口还开着就报 Settled")
	}
	clock.advance(resyncQuiet + time.Millisecond)
	im.Tick(clock.now())
	if !im.Settled() {
		t.Fatal("窗口关完仍不 Settled，注入会被永久冻住")
	}
	// 后面再开的窗口不会把 Settled 收回去：那个闸门只管首轮。
	im.BeginLinkWindow()
	if !im.Settled() {
		t.Fatal("后续窗口不应把 Settled 退回 false")
	}
}
