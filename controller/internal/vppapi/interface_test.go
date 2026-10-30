package vppapi

import (
	"encoding/binary"
	"strings"
	"testing"
)

// wg1001 的 `show interface features` 原样输出（2026-10-30 取自 lab-cpe1 上 agent
// 建的那条隧道）。它就是那个坑的现场照片：`show mpls interface` 把这张口列成
// "MPLS enabled"，而弧上还挂着 mpls-not-enabled。
//
// 判据只认 mpls-input 段的第一行正文，所以样例必须连段头前的空行、别的弧同名
// 前缀（mpls-output）一起留着 —— 少留一条，解析器"挑错段"这类回归就看不见。
const featuresArmed = `Feature paths configured on wg1001...

nsh-eth-output:
  none configured

arp:
  arp-reply

nsh-output:
  none configured

mpls-output:
  none configured

mpls-input:
  mpls-not-enabled

ip6-drop:
  none configured
`

// 同一条口 disable 之后再 enable 的弧（真机对照：loop0 开 MPLS 后就是这个样子）。
const featuresEnabled = `Feature paths configured on loop0...

mpls-output:
  none configured

mpls-input:
  none configured

ip4-unicast:
  ip4-unicast-lookup
`

func TestMPLSEnabledReadsFeatureArc(t *testing.T) {
	tests := []struct {
		name    string
		cli     string
		want    bool
		wantErr string
	}{
		{"弧上还挂着丢弃路径", featuresArmed, false, ""},
		{"弧已摘掉", featuresEnabled, true, ""},
		// CLI 把错误吞成 retval=0（见 Client.CLI 的注释），所以"没有 mpls-input 段"
		// 只能当读不到，不能当"没开" —— 猜错的方向正好相反。
		{"没有 mpls-input 段", "No such interface: 999\n", false, "没有 mpls-input 段"},
		{"段头是最后一行", "mpls-input:\n", false, "没有正文"},
		{"空输出", "", false, "没有 mpls-input 段"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.cliReply = tc.cli
			c := f.dial(t)
			ctx, cancel := shortCtx()
			defer cancel()

			got, err := c.MPLSEnabled(ctx, 6)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("期望报错含 %q，实际 got=%v err=%v", tc.wantErr, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("MPLSEnabled: %v", err)
			}
			if got != tc.want {
				t.Errorf("MPLSEnabled = %v，期望 %v", got, tc.want)
			}
			// 命令必须按接口号下标去问，不能拿名字猜：agent 手上只有 sw_if_index。
			r := f.lastRequest(t)
			if r.name != "cli_inband" {
				t.Fatalf("请求名 = %s", r.name)
			}
			if want := "show interface features 6"; !strings.Contains(cliBody(t, r.body), want) {
				t.Errorf("命令里没有 %q", want)
			}
		})
	}
}

// cliBody 取出 cli_inband 请求里的那条命令（净体 = u32 长度 + 字符串，不含 NUL）。
func cliBody(t *testing.T, body []byte) string {
	t.Helper()
	if len(body) < 4 {
		t.Fatalf("cli_inband 净体太短 (%d B)", len(body))
	}
	n := int(binary.BigEndian.Uint32(body[:4]))
	if 4+n > len(body) {
		t.Fatalf("cli_inband 声明字符串 %d B，净体只有 %d B", n, len(body)-4)
	}
	return string(body[4 : 4+n])
}
