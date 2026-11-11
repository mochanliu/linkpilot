// A minimal line-oriented unified diff, written here because the console's
// version-diff view needs exactly one thing -- "what changed between vN and
// vM, line by line" -- and pulling a diff library for that would outsource a
// 60-line algorithm to a dependency. Context is 3 lines, the hunks are
// standard " @@ " format so the page can color + / - / space lines without
// knowing anything else about this file.
package main

import "strings"

// diffLines splits artifact text into lines. Normalize \r\n the renderers
// never emit but a hand-edited bundle might carry.
func diffLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	// A trailing newline produces a final "" that is not a line; drop it so
	// the diff does not end with a phantom deletion.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// unifiedDiff renders a and b (already split into lines) in unified format
// with the given context. Equal inputs return nil, so the page hides a
// segment that did not move instead of printing an empty block.
func unifiedDiff(a, b []string, ctx int) []string {
	var out []string
	type op struct {
		tag   byte // ' ', '-', '+'
		line  string
		aLine int // 1-based, for '-' and ' '
		bLine int // 1-based, for '+' and ' '
	}
	// LCS table -- artifact segments are at most a few hundred lines each,
	// and a quadratic table at that size is noise.
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	ops := make([]op, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{' ', a[i], i + 1, j + 1})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, op{'-', a[i], i + 1, 0})
			i++
		default:
			ops = append(ops, op{'+', b[j], 0, j + 1})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{'-', a[i], i + 1, 0})
	}
	for ; j < m; j++ {
		ops = append(ops, op{'+', b[j], 0, j + 1})
	}

	// Group ops into hunks: a run of changes (or a single changed line) with
	// ctx unchanged lines of context on each side.
	changed := make([]int, len(ops))
	for k := range ops {
		if ops[k].tag != ' ' {
			changed[k] = 1
		}
	}
	k := 0
	var aStart, bStart int
	for k < len(ops) {
		if changed[k] == 0 {
			k++
			continue
		}
		start := k
		end := k
		// Extend while the next change is within 2*ctx unchanged lines.
		gapSinceChange := 0
		for e := k; e < len(ops); e++ {
			if changed[e] == 1 {
				end = e
				gapSinceChange = 0
			} else {
				gapSinceChange++
				if gapSinceChange > 2*ctx {
					break
				}
			}
		}
		lo := start - ctx
		if lo < 0 {
			lo = 0
		}
		hi := end + ctx
		if hi >= len(ops) {
			hi = len(ops) - 1
		}
		aStart, bStart = 0, 0
		for x := lo; x <= hi; x++ {
			if ops[x].aLine > 0 && aStart == 0 {
				aStart = ops[x].aLine
			}
			if ops[x].bLine > 0 && bStart == 0 {
				bStart = ops[x].bLine
			}
		}
		aCount, bCount := 0, 0
		for x := lo; x <= hi; x++ {
			if ops[x].aLine > 0 {
				aCount++
			}
			if ops[x].bLine > 0 {
				bCount++
			}
		}
		out = append(out, hunkHeader(aStart, aCount, bStart, bCount))
		for x := lo; x <= hi; x++ {
			out = append(out, string(ops[x].tag)+ops[x].line)
		}
		k = hi + 1
	}
	return out
}

func hunkHeader(aStart, aCount, bStart, bCount int) string {
	var b strings.Builder
	b.WriteString("@@ -")
	b.WriteString(fmtInt(aStart))
	b.WriteString(",")
	b.WriteString(fmtInt(aCount))
	b.WriteString(" +")
	b.WriteString(fmtInt(bStart))
	b.WriteString(",")
	b.WriteString(fmtInt(bCount))
	b.WriteString(" @@")
	return b.String()
}

func fmtInt(n int) string {
	if n < 0 {
		n = 0
	}
	// Small local itoa: avoids importing strconv for three call sites above.
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// unifiedDiffStrings is the convenience wrapper the API layer uses.
func unifiedDiffStrings(a, b string) []string {
	return unifiedDiff(diffLines(a), diffLines(b), 3)
}
