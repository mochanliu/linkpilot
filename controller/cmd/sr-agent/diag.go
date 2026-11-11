// Remote diagnostics, the box-side half of wire.DiagCommand/DiagResult.
//
// The executor is deliberately narrow: three tools, every argument rebuilt as
// argv (no shell), a hard timeout and an output cap. A diagnostic is something
// the box runs on the operator's behalf, not a general remote shell -- the
// whitelist below is the whole attack surface, and it is checked again here
// even though the controller validated the command on issue, because the two
// processes upgrade on different days.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"sdwan/controller/internal/agent"
	"sdwan/controller/internal/wire"
)

const (
	diagTimeout   = 30 * time.Second
	diagOutCap    = 32 << 10 // the agent truncates; the controller re-caps anyway
	diagMaxPingN  = 10
	diagTracerBin = "traceroute"
)

// diagShowWhitelist is the complete set of vppctl read commands this executor
// will run. Plain strings, exact-prefix matched against the requested command:
// anything else is refused with the whitelist in the error, so the console
// operator learns the boundary instead of guessing.
var diagShowWhitelist = []string{
	"show interface",
	"show ip fib",
	"show ip6 fib",
	"show ip tunnel",
	"show bfd session",
	"show acl-plugin acl",
	"show abf",
}

// diagValidTarget keeps a ping/traceroute destination inside hostname/IP
// charset. argv exec has no injection risk, but a whitespace target would
// confuse the tool's own parser into probing something else; refusing early
// with a clear error beats a confusing tool error.
func diagValidTarget(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_' || r == ':':
		default:
			return false
		}
	}
	return true
}

// diagValidIface follows Linux IFNAMSIZ, same rule as the controller's
// validTAPName -- a lcp TAP is the only thing a diagnostic would source from.
func diagValidIface(s string) bool {
	if len(s) == 0 || len(s) > 15 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == ':':
		default:
			return false
		}
	}
	return true
}

// diagValidate is the agent-side copy of the controller's issue-time check. It
// returns the refusal as an error so the result carries a human reason; the
// controller's console shows it verbatim.
func diagValidate(c *wire.DiagCommand) error {
	switch c.Tool {
	case "ping":
		if !diagValidTarget(c.Target) {
			return fmt.Errorf("ping 目标不合法：%q（只接受主机名/IP 字符）", c.Target)
		}
		if c.Iface != "" && !diagValidIface(c.Iface) {
			return fmt.Errorf("源接口名不合法：%q", c.Iface)
		}
	case "traceroute":
		if !diagValidTarget(c.Target) {
			return fmt.Errorf("traceroute 目标不合法：%q（只接受主机名/IP 字符）", c.Target)
		}
		if c.Iface != "" && !diagValidIface(c.Iface) {
			return fmt.Errorf("源接口名不合法：%q", c.Iface)
		}
	case "vpp-show":
		if !diagShowAllowed(c.VppShow) {
			return fmt.Errorf("vpp-show 只接受这几个命令：%s", strings.Join(diagShowWhitelist, " / "))
		}
	default:
		return fmt.Errorf("不支持的工具：%q", c.Tool)
	}
	return nil
}

// diagShowAllowed is exact-prefix matching against the whitelist: the request
// must equal a whitelisted command up to trailing whitespace. No prefix
// shortcuts ("show ip fib extra-args") sneak through -- equality on the
// normalized string is the whole check.
func diagShowAllowed(cmd string) bool {
	cmd = strings.TrimSpace(strings.Join(strings.Fields(cmd), " "))
	if cmd == "" {
		return false
	}
	for _, w := range diagShowWhitelist {
		if cmd == w {
			return true
		}
	}
	return false
}

// diagArgv turns a validated command into argv. Returned separately from Run
// so tests can assert the exact process shape without executing anything.
func diagArgv(c *wire.DiagCommand) []string {
	switch c.Tool {
	case "ping":
		argv := []string{"ping", "-c", strconv.Itoa(pingCount(c.Count)), "-w", "20"}
		if c.Iface != "" {
			argv = append(argv, "-I", c.Iface)
		}
		return append(argv, c.Target)
	case "traceroute":
		argv := []string{"traceroute", "-n", "-w", "2", "-q", "1", "-m", "15"}
		if c.Iface != "" {
			argv = append(argv, "-i", c.Iface)
		}
		return append(argv, c.Target)
	case "vpp-show":
		// vppctl takes the command as its own argv tail; Fields splits it, and
		// diagShowAllowed already normalized it.
		return append([]string{"vppctl"}, strings.Fields(strings.TrimSpace(c.VppShow))...)
	}
	return nil
}

// pingCount clamps to the same 1..10 window the controller enforces; the copy
// here is cheap insurance against a hand-crafted POST.
func pingCount(n int) int {
	if n < 1 {
		return 4
	}
	if n > diagMaxPingN {
		return diagMaxPingN
	}
	return n
}

// diagRun executes one validated command with the hard timeout and the output
// cap. The process gets its own process group so the timeout kill takes the
// whole tree (ping forks nothing, but the habit costs nothing).
func diagRun(ctx context.Context, argv []string) (out string, err error) {
	cctx, cancel := context.WithTimeout(ctx, diagTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()
	out = buf.String()
	if len(out) > diagOutCap {
		out = out[:diagOutCap] + "\n…（输出超过 32KB，已截断）"
	}
	if cctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("超时（%s 上限）", diagTimeout)
	}
	return out, runErr
}

// diagExecute is the full path for one command: validate → argv → run, with
// every refusal expressed as DiagResult so the console always gets an answer.
func diagExecute(c *wire.DiagCommand) wire.DiagResult {
	res := wire.DiagResult{ID: c.ID, Tool: c.Tool, Finished: time.Now().Unix()}
	if err := diagValidate(c); err != nil {
		res.Err = err.Error()
		return res
	}
	start := time.Now()
	argv := diagArgv(c)
	out, err := diagRun(context.Background(), argv)
	res.DurMS = time.Since(start).Milliseconds()
	res.Output = out
	if err != nil {
		// A non-zero exit from ping/traceroute is the TOOL's verdict ("100%
		// packet loss"), not a failure of the channel -- report it in OK=false
		// with the output attached, which is what an operator needs to read.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.OK = false
			if ee.ExitCode() > 0 && res.Output != "" {
				res.Err = fmt.Sprintf("%s 退出码 %d（输出见下方）", c.Tool, ee.ExitCode())
			} else {
				res.Err = err.Error()
			}
			return res
		}
		res.OK = false
		res.Err = err.Error()
		return res
	}
	res.OK = true
	return res
}

// diagRunner executes commands one at a time. The busy flag is set before a
// command starts and cleared when it finishes (including its result POST); the
// heartbeat loop reads it to set DiagBusy, so a slow ping holds back the next
// command instead of letting results interleave.
type diagRunner struct {
	busy   int32 // 0 idle, 1 executing; CAS-guarded
	client *agent.Client
	id     *agent.Identity
	logf   func(string, ...any)
}

func newDiagRunner(client *agent.Client, id *agent.Identity, logf func(string, ...any)) *diagRunner {
	return &diagRunner{client: client, id: id, logf: logf}
}

// Busy reports whether a command is executing right now -- read at heartbeat
// build time, never written here.
func (d *diagRunner) Busy() bool {
	return atomic.LoadInt32(&d.busy) == 1
}

// Offer runs one command if the executor is idle. It never blocks the
// heartbeat: a busy box refuses the offer and the command stays queued at the
// controller for the next beat.
func (d *diagRunner) Offer(c *wire.DiagCommand) {
	if d == nil || c == nil {
		return
	}
	if !atomic.CompareAndSwapInt32(&d.busy, 0, 1) {
		d.logf("diag: 收到 %s 但上一个命令还在跑，跳过本轮", c.ID)
		return
	}
	go func() {
		defer atomic.StoreInt32(&d.busy, 0)
		res := diagExecute(c)
		res.NodeID = d.id.NodeID()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := d.client.ReportDiag(ctx, res)
		cancel()
		if err != nil {
			d.logf("diag: %s 回执失败: %v", c.ID, err)
			return
		}
		d.logf("diag: %s (%s %s) 完成，ok=%v，%dms", c.ID, c.Tool, c.Target, res.OK, res.DurMS)
	}()
}
