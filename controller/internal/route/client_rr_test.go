package route

// C-c makes the reflector set mutable at runtime. The one property that decides
// whether the design is acceptable is E1's: swapping an RR must not interrupt the
// route plane. So these tests do not ask "did the new session come up" -- they ask
// what happened to the OLD session, and to this box's own announcements, while the
// new one was still coming up.
//
// "Established" means up + synced + ready: the RR sent its EOR and we finished
// writing our snapshot to it. Anything less, and dropping the session it replaces
// leaves a window where this box speaks to nobody.

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// rrFarm routes Dial by address. testRRSrv ignores the address it is handed, so one
// of them cannot express "two reflectors are two sessions" -- which is the whole
// subject of SetRRs. Each address gets its own Reflector; an address the farm does
// not know fails to dial, which is how an RR nobody has opened yet behaves.
type rrFarm struct {
	mu    sync.Mutex
	srvs  map[string]*testRRSrv
	rrs   map[string]*Reflector
	calls map[string]int
}

func newFarm() *rrFarm {
	return &rrFarm{
		srvs:  make(map[string]*testRRSrv),
		rrs:   make(map[string]*Reflector),
		calls: make(map[string]int),
	}
}

func (f *rrFarm) add(t *testing.T, addr, rrid string) *Reflector {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	r := newTestReflector(t, ReflectorConfig{RRID: rrid})
	f.rrs[addr] = r
	f.srvs[addr] = newTestRRSrv(t, r, 10)
	return r
}

// reflector reads an RR back after a late open, so a test can assert on what that
// reflector holds without keeping a pointer across a state change.
func (f *rrFarm) reflector(addr string) *Reflector {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rrs[addr]
}

func (f *rrFarm) dial(ctx context.Context, addr string) (net.Conn, error) {
	f.mu.Lock()
	s := f.srvs[addr]
	f.calls[addr]++
	f.mu.Unlock()
	if s == nil {
		return nil, errors.New("no reflector at " + addr)
	}
	return s.dial(ctx, addr)
}

// kill takes an RR down for good: new dials fail and the live connection closes.
func (f *rrFarm) kill(addr string) {
	f.mu.Lock()
	s := f.srvs[addr]
	f.mu.Unlock()
	if s != nil {
		s.kill()
	}
}

// dialCount is how often an address has been dialled: the difference between "this
// session was reused" and "this session was torn down and rebuilt".
func (f *rrFarm) dialCount(addr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[addr]
}

// open brings an address online late, which is how a switch parked on an
// unreachable RR is seen completing on its own.
func (f *rrFarm) open(t *testing.T, addr, rrid string) {
	t.Helper()
	f.add(t, addr, rrid)
}

func sessionOf(c *Client, addr string) (SessionStats, bool) {
	for _, s := range c.Stats().Sessions {
		if s.Addr == addr {
			return s, true
		}
	}
	return SessionStats{}, false
}

func TestSetRRsValidationLeavesTheSetAlone(t *testing.T) {
	c, err := NewClient(testClientConfig())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"empty set", c.SetRRs(nil), "at least one RR"},
		{"same address twice", c.SetRRs([]RRConfig{{ID: "a", Addr: "x"}, {ID: "b", Addr: "x"}}), "configured twice"},
		{"missing address", c.SetRRs([]RRConfig{{ID: "a"}}), "no address"},
	}
	for _, tc := range cases {
		if tc.err == nil || !strings.Contains(tc.err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want containing %q", tc.name, tc.err, tc.want)
		}
	}
	// A rejected set must not have quietly replaced the live one: an operator
	// reading /status afterwards would believe this box has no reflector at all.
	if got := c.RRs(); len(got) != 1 || got[0] != (RRConfig{ID: "rr-a", Addr: "rr-a"}) {
		t.Fatalf("rrs after rejected sets = %+v", got)
	}
}

// The case E1 was shot for: replacing the only reflector. Until the replacement
// has established, the old session stays in the list and keeps this box's own
// announcements on the wire -- that is the return path other CPEs are using.
func TestSetRRsKeepsTheOldSessionUntilTheNewOneEstablishes(t *testing.T) {
	f := newFarm()
	rrA := f.add(t, "rr-a", "rr-a")
	f.add(t, "rr-b", "rr-b")

	cfg := testClientConfig()
	cfg.RRs = []RRConfig{{ID: "rr1", Addr: "rr-a"}}
	cfg.Dial = f.dial
	c := startClient(t, cfg)
	waitFor(t, "rr-a to serve", func() bool { return c.Stats().RibReady })
	if n := len(rrA.RoutesOf(10)); n != 1 {
		t.Fatalf("rr-a holds %d of our routes before the switch", n)
	}

	if err := c.SetRRs([]RRConfig{{ID: "rr1", Addr: "rr-b"}}); err != nil {
		t.Fatalf("SetRRs: %v", err)
	}

	// In flight: rr-b pending, rr-a retiring, and rr-a is still the active one.
	st := c.Stats()
	if st.RRWait == nil {
		t.Fatal("换 RR 进行中却没有 rr_wait")
	}
	if len(st.RRWait.PendingAddrs) != 1 || st.RRWait.PendingAddrs[0] != "rr-b" {
		t.Fatalf("pending = %v", st.RRWait.PendingAddrs)
	}
	if len(st.RRWait.RetiringAddrs) != 1 || st.RRWait.RetiringAddrs[0] != "rr-a" {
		t.Fatalf("retiring = %v", st.RRWait.RetiringAddrs)
	}
	if s, ok := sessionOf(c, "rr-a"); !ok || !s.Retiring || !s.Synced {
		t.Fatalf("rr-a session = %+v/%v", s, ok)
	}
	// rr-a is still the only session in the Loc-RIB, so the route plane did not
	// blink while the replacement was dialled.
	if s, ok := sessionOf(c, "rr-a"); !ok || !s.Synced || !st.RibReady {
		t.Fatalf("rr-a session = %+v/%v ready=%v, still want it serving while rr-b is coming up", s, ok, st.RibReady)
	}
	// RRs() is the set this box works towards, not the set still connected.
	if got := c.RRs(); len(got) != 1 || got[0].Addr != "rr-b" {
		t.Fatalf("RRs() = %+v", got)
	}

	// Takeover: rr-b has EOR'd and received our snapshot, so rr-a is dropped and
	// the wait disappears. Both RRs holding our prefix is the proof the switch was
	// never a gap.
	waitFor(t, "rr-b to take over", func() bool {
		st := c.Stats()
		return st.RRWait == nil && len(st.Sessions) == 1 && st.Sessions[0].Addr == "rr-b"
	})
	rrB := f.reflector("rr-b")
	if n := len(rrB.RoutesOf(10)); n != 1 {
		t.Fatalf("rr-b holds %d of our routes after takeover", n)
	}
	if n := len(rrA.RoutesOf(10)); n != 0 {
		t.Errorf("旧 RR 摘掉后还留着我们的前缀: %d 条（设备侧计数，见 Sessions()）", n)
	}
	if sn := rrA.Sessions(); len(sn) != 0 {
		t.Fatalf("rr-a still lists a session after the reap: %+v", sn)
	}
}

// The negative half: the new RR is unreachable, so nothing can take over. The old
// session is the only thing between this box and a route plane of nothing.
func TestSetRRsToAnUnreachableRRKeepsTheOldOne(t *testing.T) {
	f := newFarm()
	rrA := f.add(t, "rr-a", "rr-a")

	cfg := testClientConfig()
	cfg.RRs = []RRConfig{{ID: "rr1", Addr: "rr-a"}}
	cfg.Dial = f.dial
	c := startClient(t, cfg)
	waitFor(t, "rr-a to serve", func() bool { return c.Stats().RibReady })

	if err := c.SetRRs([]RRConfig{{ID: "rr1", Addr: "nowhere"}}); err != nil {
		t.Fatalf("SetRRs: %v", err)
	}
	// Dialling happens in the session's own goroutine, so this is a wait, not an
	// assertion on the return of the PUT.
	waitFor(t, "the new RR to be dialled", func() bool { return f.dialCount("nowhere") > 0 })
	// Longer than one reconnect interval: a reap that fired out of turn would show
	// up as rr-a leaving the list.
	waitForWithin(t, 3*time.Second, "rr-a to still be serving", func() bool {
		st := c.Stats()
		if !st.RibReady {
			return false
		}
		return st.RRWait != nil && len(st.RRWait.RetiringAddrs) == 1 &&
			len(st.RRWait.PendingAddrs) == 1 && st.RRWait.PendingAddrs[0] == "nowhere"
	})
	if s, ok := sessionOf(c, "rr-a"); !ok || !s.Connected || !s.Retiring {
		t.Fatalf("rr-a dropped over an unreachable new RR: %+v/%v", s, ok)
	}
	if n := len(rrA.RoutesOf(10)); n != 1 {
		t.Fatalf("our announcements left the old RR: %d", n)
	}
	// The wanted set is still the new one: the box keeps trying, and a link that
	// comes up later reaps rr-a without another PUT.
	if got := c.RRs(); len(got) != 1 || got[0].Addr != "nowhere" {
		t.Fatalf("RRs() = %+v", got)
	}
}

// Rolling a fleet over one RR at a time is what 主+备 is for, so the two changes
// that are not replacements must cost nothing: adding a standby must not touch the
// active session, and reordering must not empty the table.
func TestSetRRsStandbyChangesDoNotDisturbTheActiveSession(t *testing.T) {
	f := newFarm()
	f.add(t, "rr-a", "rr-a")
	f.add(t, "rr-b", "rr-b")

	cfg := testClientConfig()
	cfg.RRs = []RRConfig{{ID: "rr1", Addr: "rr-a"}}
	cfg.Dial = f.dial
	c := startClient(t, cfg)
	waitFor(t, "rr-a serves", func() bool { return c.Stats().RibReady })
	dialed := f.dialCount("rr-a")

	if err := c.SetRRs([]RRConfig{{ID: "rr1", Addr: "rr-a"}, {ID: "rr2", Addr: "rr-b"}}); err != nil {
		t.Fatalf("add standby: %v", err)
	}
	waitFor(t, "both sessions listed", func() bool { return len(c.Stats().Sessions) == 2 })
	if f.dialCount("rr-a") != dialed {
		t.Fatal("the active session was rebuilt over a standby change")
	}
	if st := c.Stats(); st.RRWait != nil {
		t.Fatalf("nothing was replaced, yet rr_wait says otherwise: %+v", st.RRWait)
	}
	// The library label belongs to the session from the moment it is configured,
	// and the hello_ok id arrives with the handshake: /status needs both to let an
	// operator map a session back to the RR library entry it came from.
	if s, ok := sessionOf(c, "rr-b"); !ok || s.ConfigID != "rr2" {
		t.Fatalf("rr-b session = %+v/%v", s, ok)
	}
	waitFor(t, "rr-b to finish its handshake", func() bool {
		s, ok := sessionOf(c, "rr-b")
		return ok && s.Connected && s.RRID == "rr-b"
	})

	// Swap the order. rr-a is still wanted, so it stays connected -- and nothing is
	// retiring, so there is no wait either.
	if err := c.SetRRs([]RRConfig{{ID: "rr1", Addr: "rr-b"}, {ID: "rr2", Addr: "rr-a"}}); err != nil {
		t.Fatalf("swap: %v", err)
	}
	waitFor(t, "rr-b to be listed first", func() bool {
		st := c.Stats()
		return st.Sessions[0].Addr == "rr-b" && st.Sessions[0].Synced &&
			st.Sessions[1].Addr == "rr-a" && st.Sessions[1].Synced
	})
	if st := c.Stats(); st.RRWait != nil {
		t.Fatalf("a reorder retired sessions: %+v", st.RRWait)
	}

	// Drop rr-a for good. Now it is retired AND rr-b has established, so the same
	// call reaps it.
	if err := c.SetRRs([]RRConfig{{ID: "rr1", Addr: "rr-b"}}); err != nil {
		t.Fatalf("drop rr-a: %v", err)
	}
	waitFor(t, "rr-a reaped", func() bool {
		st := c.Stats()
		return len(st.Sessions) == 1 && st.Sessions[0].Addr == "rr-b" && st.RRWait == nil
	})
	if f.dialCount("rr-b") != 1 {
		t.Fatalf("rr-b was dialled %d times by a set that kept asking for it", f.dialCount("rr-b"))
	}
}

// A switch the operator cancels mid-flight: rr-a becomes wanted again while it is
// still retiring. Reusing that session instead of dialling a third one is what
// makes an accidental RR change reversible.
func TestSetRRsRollbackReusesTheRetiringSession(t *testing.T) {
	f := newFarm()
	f.add(t, "rr-a", "rr-a")

	cfg := testClientConfig()
	cfg.RRs = []RRConfig{{ID: "rr1", Addr: "rr-a"}}
	cfg.Dial = f.dial
	c := startClient(t, cfg)
	waitFor(t, "rr-a serves", func() bool { return c.Stats().RibReady })
	dialed := f.dialCount("rr-a")

	if err := c.SetRRs([]RRConfig{{ID: "rr1", Addr: "nowhere"}}); err != nil {
		t.Fatalf("switch away: %v", err)
	}
	waitFor(t, "rr-a to be retiring", func() bool {
		s, ok := sessionOf(c, "rr-a")
		return ok && s.Retiring
	})

	if err := c.SetRRs([]RRConfig{{ID: "rr1", Addr: "rr-a"}}); err != nil {
		t.Fatalf("switch back: %v", err)
	}
	st := c.Stats()
	if st.RRWait != nil {
		t.Fatalf("rollback left a wait behind: %+v", st.RRWait)
	}
	if len(st.Sessions) != 1 {
		t.Fatalf("sessions after rollback = %+v", st.Sessions)
	}
	if f.dialCount("rr-a") != dialed {
		t.Fatal("the retiring session was torn down and redialled instead of reused")
	}
	if s, ok := sessionOf(c, "rr-a"); !ok || s.Retiring || !s.Connected || !st.RibReady {
		t.Fatalf("rr-a after rollback = %+v/%v ready=%v", s, ok, st.RibReady)
	}
}

// The reap is driven by markSynced, not only by the next SetRRs: a switch parked
// on an RR nobody has opened yet completes by itself once the link comes up.
func TestSetRRsReapsWhenTheNewSessionSyncsLater(t *testing.T) {
	f := newFarm()
	f.add(t, "rr-a", "rr-a")

	cfg := testClientConfig()
	cfg.RRs = []RRConfig{{ID: "rr1", Addr: "rr-a"}}
	cfg.Dial = f.dial
	c := startClient(t, cfg)
	waitFor(t, "rr-a serves", func() bool { return c.Stats().RibReady })

	if err := c.SetRRs([]RRConfig{{ID: "rr1", Addr: "rr-b"}}); err != nil {
		t.Fatalf("switch: %v", err)
	}
	waitFor(t, "rr-a to be retiring", func() bool {
		s, ok := sessionOf(c, "rr-a")
		return ok && s.Retiring && s.Connected
	})

	// No second PUT: the session dials on its own backoff, and the first synced
	// wanted session ends the switch.
	f.open(t, "rr-b", "rr-b")
	waitForWithin(t, 20*time.Second, "rr-b to take over and rr-a to go", func() bool {
		st := c.Stats()
		return len(st.Sessions) == 1 && st.Sessions[0].Addr == "rr-b" && st.RRWait == nil
	})
}

// One case where an empty table is the honest answer: the RR is gone. The sessions
// list keeps the address, so /status can still say which reflector this box is
// trying to reach.
func TestAllSessionsDownLeavesNothingReady(t *testing.T) {
	f := newFarm()
	f.add(t, "rr-a", "rr-a")

	cfg := testClientConfig()
	cfg.RRs = []RRConfig{{ID: "rr1", Addr: "rr-a"}}
	cfg.Dial = f.dial
	c := startClient(t, cfg)
	waitFor(t, "rr-a serves", func() bool { return c.Stats().RibReady })

	f.kill("rr-a")
	waitFor(t, "nothing ready", func() bool { return !c.Stats().RibReady })
	st := c.Stats()
	if len(st.Sessions) != 1 || st.Sessions[0].Connected {
		t.Fatalf("sessions = %+v", st.Sessions)
	}
	if st.Sessions[0].Addr != "rr-a" || st.Sessions[0].ConfigID != "rr1" {
		t.Fatalf("session identity lost: %+v", st.Sessions[0])
	}
	// Nothing is retiring, so this must not read as a half-finished RR switch.
	if st.RRWait != nil {
		t.Fatalf("an outage reported as a switch: %+v", st.RRWait)
	}
}
