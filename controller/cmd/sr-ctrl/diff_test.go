package main

// diff_test.go —— 统一 diff 的最小回归面：改一行、加一段、删一段、行序互换
// （渲染顺序敏感的内容）。hunk 头的行号算错是最容易混进来的错，逐个对。

import (
	"strings"
	"testing"
)

func dl(s string) []string { return diffLines(s) }

func TestUnifiedDiffChangeOneLine(t *testing.T) {
	a := dl("line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nline10")
	b := dl("line1\nline2\nline3\nline4\nline5X\nline6\nline7\nline8\nline9\nline10")
	d := unifiedDiff(a, b, 3)
	got := strings.Join(d, "\n")
	want := "@@ -2,7 +2,7 @@\n line2\n line3\n line4\n-line5\n+line5X\n line6\n line7\n line8"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedDiffEqualIsEmpty(t *testing.T) {
	a := dl("a\nb\nc")
	if d := unifiedDiff(a, a, 3); len(d) != 0 {
		t.Fatalf("相同输入应无输出: %v", d)
	}
	if d := unifiedDiffStrings("", ""); len(d) != 0 {
		t.Fatalf("两个空串应无输出: %v", d)
	}
}

func TestUnifiedDiffAppendTail(t *testing.T) {
	d := unifiedDiff(dl("a\nb"), dl("a\nb\nc\nd"), 3)
	got := strings.Join(d, "\n")
	want := "@@ -1,2 +1,4 @@\n a\n b\n+c\n+d"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnifiedDiffDropHead(t *testing.T) {
	d := unifiedDiff(dl("a\nb\nc"), dl("c"), 3)
	got := strings.Join(d, "\n")
	want := "@@ -1,3 +1,1 @@\n-a\n-b\n c"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestDiffLinesTrailingNewline(t *testing.T) {
	if got := diffLines("a\nb\n"); len(got) != 2 || got[1] != "b" {
		t.Fatalf("结尾换行不该产生幽灵行: %v", got)
	}
	if got := diffLines(""); len(got) != 0 {
		t.Fatalf("空串应无行: %v", got)
	}
}

func TestUnifiedDiffHunkCounts(t *testing.T) {
	// 两个独立改动 → 两个 hunk；每个 hunk 头的计数要能对上实际行数。
	a := dl("a1\na2\na3\na4\na5\na6\na7\na8\na9\na10\na11\na12\na13\na14\na15")
	b := dl("a1\na2X\na3\na4\na5\na6\na7\na8\na9\na10\na11\na12\na13\na14\na15Z")
	d := unifiedDiff(a, b, 3)
	hunks := 0
	for _, l := range d {
		if strings.HasPrefix(l, "@@") {
			hunks++
		}
	}
	if hunks != 2 {
		t.Fatalf("应有两个 hunk: %v", d)
	}
}
