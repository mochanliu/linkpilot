package main

// 路由事件流：GET sr-route 的 /watch，一条 SSE 帧 = 一批路由变化。
//
// 取代的是"每 poll_sec 拉一次 /status"。轮询把每次真实变化的生效时间都抬高了
// 一个轮询周期（lab 里是 1s），而 #262 量的正是这一项：BFD 150ms 判 down，
// sr-fib 要等到下一轮才摘路。改成事件推之后这一段归零，只剩 sr-route 自己的
// 10ms 合并窗口（照抄 FRR 的 ZEBRA_RIB_PROCESS_HOLD_TIME）。
//
// 三条不变量，逐条对应一种"静默不一致"：
//
//   - 每次挂上都是 INIT（整份视图），没有断点续传。FRR 形状：新连接
//     bgp_zebra_connected -> bgp_zebra_announce_table 重宣告全量，不接半截。
//   - 流一断就立刻把视图作废并全量补一次（不是等下一轮）。留着一份旧视图继续
//     装表就是 AGENTS.md §1.3 禁的那个"乐观窗口"。
//   - 心跳帧（sr-route 每 5s 一条 ": ping"）+ 本侧 15s 静默即判死。没有心跳，
//     "路由确实没变"和"连接不再送字节"在线上是同一个样子。
//
// poll_sec 从"轮询间隔"变成"重连间隔"：读不到流时最多每隔它试一次，每次先做
// 一遍全量。现存的 fib.json 一个字段都不用改。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"sdwan/controller/internal/fib"
	"sdwan/controller/internal/route"
)

// watchEventName is the only SSE event name sr-route emits today. Anything
// else means the two ends disagree about the stream, and the honest reaction
// is to drop the connection and re-attach for a fresh INIT, not to skip a
// frame we did not understand.
const watchEventName = "routes"

// watchIdleTimeout is how long a healthy-looking stream may go without a
// single byte before this side calls it dead. Three keepalive intervals: one
// lost ping is tolerated, two is a wedged connection.
const watchIdleTimeout = 3 * 5 * time.Second

// errWatchBusy is the 409 answer -- someone else holds sr-route's single
// subscription slot. Not fatal: the caller already closed the view gap with a
// full fetch and will retry on the reconnect interval.
var errWatchBusy = errors.New("watch: sr-route 的订阅位被占用（已有另一个消费者）")

// ribKey is one observable RIB row, the same identity the event carries: a
// target table plus the source route that fills it.
type ribKey struct {
	Table    uint8
	DeviceID uint16
	VRFID    uint8
	Prefix   string
}

func keyOf(r fib.RIBRoute) ribKey {
	return ribKey{Table: r.Table, DeviceID: r.Origin, VRFID: r.SourceVRF, Prefix: r.Prefix}
}

// watchURL turns the status URL this process was configured with into the
// stream URL next to it: .../status -> .../watch.
func watchURL(statusURL string) (string, error) {
	u, err := url.Parse(statusURL)
	if err != nil {
		return "", fmt.Errorf("status_url %q 解析不了: %w", statusURL, err)
	}
	u.Path = path.Join(path.Dir(u.Path), "watch")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// watchLoop owns one /watch connection at a time and never returns until ctx
// ends. retryGap is the wait between attempts (poll_sec).
func (f *fiber) watchLoop(ctx context.Context, retryGap time.Duration) {
	for {
		if ctx.Err() != nil {
			return
		}
		err := f.watchOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		// The stream is over, for whatever reason. The view it was feeding is
		// now unprovable, so it goes away before anything else -- converge
		// then falls back to a full GET /status and closes the gap on this
		// pass, not on the next attempt.
		f.dropView(err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryGap):
		}
	}
}

// watchOnce attaches, reads frames until the stream ends, and returns the
// reason it ended. A nil return would mean a clean EOF from sr-route.
func (f *fiber) watchOnce(ctx context.Context) error {
	u, err := watchURL(f.statusURL)
	if err != nil {
		return err
	}
	// A cancellable child is the only way to put a deadline on a read that is
	// already blocked in Body.Read: the watchdog is re-armed by every line the
	// parser sees, and letting it fire makes the in-flight read fail the same
	// way a broken pipe does.
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	wd := time.AfterFunc(watchIdleTimeout, cancel)
	defer wd.Stop()

	req, err := http.NewRequestWithContext(readCtx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := f.stream.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%w：%s", errWatchBusy, strings.TrimSpace(string(body)))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}

	br := bufio.NewReader(resp.Body)
	for {
		batch, err := nextWatchBatch(br, func() { wd.Reset(watchIdleTimeout) })
		if err != nil {
			return err
		}
		f.applyBatch(batch)
	}
}

// nextWatchBatch reads one SSE frame: the data line of the next blank-line
// terminated event. Comments (the keepalive) are skipped, and any line at all
// counts as liveness via touch.
func nextWatchBatch(br *bufio.Reader, touch func()) (route.WatchBatch, error) {
	var (
		data   string
		have   bool
		name   string
		sawAny bool
	)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			// A stream that ends mid-frame is not a batch: report the failure
			// and let the caller re-attach for a full INIT.
			if sawAny && err == io.EOF {
				return route.WatchBatch{}, errors.New("watch: 事件流在帧中间结束")
			}
			return route.WatchBatch{}, err
		}
		if touch != nil {
			touch()
		}
		sawAny = true
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		switch {
		case line == "":
			if !have {
				continue
			}
			if name != "" && name != watchEventName {
				return route.WatchBatch{}, fmt.Errorf("watch: 未知事件 %q（要重连取全量）", name)
			}
			var b route.WatchBatch
			if err := json.Unmarshal([]byte(data), &b); err != nil {
				return route.WatchBatch{}, fmt.Errorf("watch 帧解析: %w", err)
			}
			return b, nil
		case strings.HasPrefix(line, ":"):
			// keepalive / comment
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			have = true
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimPrefix(strings.TrimPrefix(line, "event:"), " ")
		}
	}
}

// applyBatch edits the local view and asks for one converge. It never blocks
// on the data plane: the view has its own lock precisely so a slow apply can
// still let the stream drain (and a drained stream is what keeps sr-route from
// dropping us for backpressure).
func (f *fiber) applyBatch(b route.WatchBatch) {
	f.vmu.Lock()
	if b.Init {
		next := make(map[ribKey]fib.RIBRoute, len(b.Events))
		for _, ev := range b.Events {
			r := eventRoute(ev)
			next[keyOf(r)] = r
		}
		f.view, f.viewOK = next, true
	} else if !f.viewOK {
		// A delta with no snapshot behind it is not a thing we can apply
		// partially; the reconnect that follows brings a full INIT.
		f.vmu.Unlock()
		f.signal()
		return
	} else {
		for _, ev := range b.Events {
			k := ribKey{Table: ev.Table, DeviceID: ev.DeviceID, VRFID: ev.VRFID, Prefix: ev.Prefix}
			switch ev.Op {
			case route.OpUpsert:
				f.view[k] = eventRoute(ev)
			case route.OpWithdraw:
				delete(f.view, k)
			default:
				f.vmu.Unlock()
				f.dropView(fmt.Errorf("未知事件 op=%q，作废视图取全量", ev.Op))
				return
			}
		}
	}
	f.vmu.Unlock()
	f.signal()
}

func eventRoute(ev route.RouteEvent) fib.RIBRoute {
	return fib.RIBRoute{
		Table:     ev.Table,
		SourceVRF: ev.VRFID,
		Origin:    ev.DeviceID,
		Prefix:    ev.Prefix,
		NextHops:  ev.NextHops,
	}
}

// dropView marks the view unprovable and queues the converge that will
// replace it with a full fetch.
func (f *fiber) dropView(reason error) {
	f.vmu.Lock()
	had := f.viewOK
	f.view, f.viewOK = nil, false
	f.vmu.Unlock()
	f.signal()
	if reason != nil {
		f.logWatchIssue(fmt.Sprintf("事件流结束（%v）：改用全量 /status，并按重连间隔再挂", reason))
	} else if had {
		f.logWatchIssue("事件流结束：改用全量 /status，并按重连间隔再挂")
	}
}

// logWatchIssue keeps the reconnect log honest without turning a down sr-route
// into one line per attempt: the same message is only repeated when it changes.
func (f *fiber) logWatchIssue(msg string) {
	f.vmu.Lock()
	defer f.vmu.Unlock()
	if msg == f.watchLog {
		return
	}
	f.watchLog = msg
	log.Printf("sr-fib: %s", msg)
}

// currentRoutes is converge()'s view of the RIB: the event-fed snapshot when it
// is provable, and a full GET /status when it is not (before the first INIT,
// and for every pass while the stream is being re-established).
func (f *fiber) currentRoutes(ctx context.Context) ([]fib.RIBRoute, error) {
	f.vmu.Lock()
	ok := f.viewOK
	out := make([]fib.RIBRoute, 0, len(f.view))
	if ok {
		for _, r := range f.view {
			out = append(out, r)
		}
	}
	f.vmu.Unlock()
	if !ok {
		return fetchStatus(ctx, f.hc, f.statusURL)
	}
	sortRIBRoutes(out)
	return out, nil
}

// convergeLoop is the only place a plan is pushed to the data plane: an event
// batch or a new config sets the flag, and one pass consumes everything that
// arrived while it ran.
//
// Since exp-286 it also runs one pass per healthPoll regardless of events. The
// event stream alone only covers moments when somebody is talking: a VPP
// restart kills the BFD event subscription with the connection, and if the RIB
// is quiet at that moment nothing ever wakes a converge -- the process then sits
// on a dead api handle with an empty FIB and no log line (measured: one page of
// broken-pipe lines 203 s apart from the recovery, FIB empty the whole time).
// A periodic pass is the probe: reading BFD sessions goes over that same
// connection, so a dead one reports ErrClosed here, dropDead drops the handle
// and the following pass redials from scratch. When healthy it is three reads
// and zero writes, and it doubles as drift self-heal.
func (f *fiber) convergeLoop(ctx context.Context) {
	poll := f.healthPoll
	if poll <= 0 {
		poll = defaultHealthPoll
	}
	health := time.NewTicker(poll)
	defer health.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.wake:
		case <-health.C:
		}
		// dp/applier 会在两次收敛之间被换掉（重连），必须和 PUT /config 那条
		// 路径共用一把锁 —— Applier 只允许单线程调用。
		f.mu.Lock()
		err := f.converge(ctx)
		f.mu.Unlock()
		if err != nil {
			// 失败不退出：sr-route 可能还没起来，VPP 也偶尔在重启。已下发成功的
			// 那份计划仍是基线，下一次事件或下一次重连按同一目标再试。
			log.Printf("sr-fib: 本轮收敛失败（保留上次成功的计划）: %v", err)
		}
	}
}

// signal asks for one converge. Capacity 1 collapses an arbitrary burst into
// the single latest state -- the same shape as sr-route's own watch hub and
// sr-frr's importer, and for the same reason: the work is "reach the snapshot",
// not "replay the history".
func (f *fiber) signal() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}
