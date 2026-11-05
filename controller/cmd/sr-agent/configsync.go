package main

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"sdwan/controller/internal/apply"
	"sdwan/controller/internal/wire"
)

// configSync is the device half of the push channel.
//
// It owns three facts the controller must never guess: which version is
// installed (the store's `current` pointer), whether the last apply worked
// (status.json, written by the applier itself), and whether an apply is in
// flight. The heartbeat reports exactly that, which is why the console can say
// "expected v4 / running v3, last apply failed" instead of showing a green tick
// that means nothing.
type configSync struct {
	nodeID uint16
	store  apply.Store
	fetch  func(context.Context) ([]byte, error)
	apply  func(context.Context, apply.Bundle) error
	logf   func(format string, args ...any)
	now    func() time.Time

	retryMin time.Duration
	retryMax time.Duration

	mu       sync.Mutex
	state    wire.ConfigState
	applying bool
	fails    int
	nextTry  time.Time
	// failedOn is the version that set nextTry. The backoff answers "should I
	// retry THIS bundle again yet?", not "should I touch any bundle at all": an
	// operator who published a newer version already changed what the box is
	// being asked to run, and waiting out a timer for a config that no longer
	// exists is what made a fix look like it had not been received.
	failedOn int
}

func newConfigSync(nodeID uint16, store apply.Store, fetch func(context.Context) ([]byte, error),
	applyFn func(context.Context, apply.Bundle) error, logf func(string, ...any), now func() time.Time) *configSync {
	c := &configSync{
		nodeID: nodeID, store: store, fetch: fetch, apply: applyFn, logf: logf, now: now,
		retryMin: 5 * time.Second, retryMax: 5 * time.Minute,
	}
	c.state = c.readState()
	c.state.Capable = true
	return c
}

// readState turns the store into the report. Note what it does NOT do: claim a
// version was applied because its directory exists. Install() moves `current`
// before the replay runs, so a failed apply still moves the pointer -- the
// status file is the only thing that says whether the box took it.
func (c *configSync) readState() wire.ConfigState {
	st := wire.ConfigState{Capable: true}
	inst, err := c.store.Current()
	switch {
	case err == nil:
		st.Version = inst.Manifest.Version
	case err == apply.ErrNoConfig:
		// Never installed: reporting "nothing" is the truth, not an error.
	default:
		st.Error = err.Error()
	}
	s, err := c.store.ReadStatus()
	if err != nil {
		return st
	}
	if s.Version != st.Version {
		// The status file describes a version the pointer does not name (an
		// install that never finished, or a hand-edited store). Saying so beats
		// picking one of the two numbers and presenting it as fact.
		st.Error = fmt.Sprintf("status.json 记录的是 v%d，磁盘 current 指向 v%d", s.Version, st.Version)
		return st
	}
	st.OK = s.OK
	st.AppliedAt = s.AppliedAt
	st.Source = s.Source
	st.Error = s.Error
	st.Notes = s.Notes
	return st
}

// State is a copy of the last report, safe to attach to a heartbeat.
func (c *configSync) State() wire.ConfigState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Sync fetches and applies one version, then re-reads the store so the report
// is the applier's verdict rather than ours.
func (c *configSync) Sync(ctx context.Context, desired int) error {
	c.mu.Lock()
	c.state.Applying = true
	c.state.ApplyingVersion = desired
	c.mu.Unlock()

	err := c.fetchAndApply(ctx, desired)

	c.mu.Lock()
	st := c.readState()
	st.Applying = false
	if err != nil {
		st.OK = false
		st.Error = err.Error()
	}
	c.state = st
	c.mu.Unlock()
	return err
}

func (c *configSync) fetchAndApply(ctx context.Context, desired int) error {
	raw, err := c.fetch(ctx)
	if err != nil {
		return fmt.Errorf("拉取配置包: %w", err)
	}
	b, err := apply.DecodeBundle(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("配置包无效: %w", err)
	}
	if b.NodeID != 0 && b.NodeID != c.nodeID {
		// A bundle addressed to another node is never installable, even over
		// this device's own certificate. Refusing is the only answer that
		// cannot become a typo in someone's favour.
		return fmt.Errorf("配置包属于 node %d，本机是 node %d", b.NodeID, c.nodeID)
	}
	c.logf("applying config v%d (asked for v%d)", b.Version, desired)
	// Never allowBlocking from the network path: a bundle the controller
	// published should have no errors, and if one slips through the device
	// sides with the applier's refusal instead of with a --force it cannot
	// explain to anyone.
	return c.apply(ctx, b)
}

// Kick starts an apply in the background when the desired version is not what
// is running and that version is not still sitting in its own backoff window.
// It never blocks the heartbeat: an apply restarts FRR and can take tens of
// seconds, and a device that stops heartbeating mid-change looks offline exactly
// when somebody is watching it.
func (c *configSync) Kick(ctx context.Context, desired int) {
	if desired < 1 {
		return
	}
	c.mu.Lock()
	blockedByBackoff := desired == c.failedOn && c.now().Before(c.nextTry)
	if c.applying || (c.state.Version == desired && c.state.OK) || blockedByBackoff {
		c.mu.Unlock()
		return
	}
	c.applying = true
	c.mu.Unlock()

	go func() {
		actx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		err := c.Sync(actx, desired)

		c.mu.Lock()
		c.applying = false
		wait := time.Duration(0)
		fails := c.fails
		if err != nil {
			c.fails++
			fails = c.fails
			wait = c.backoff()
			c.nextTry = c.now().Add(wait)
			c.failedOn = desired
		} else {
			c.fails = 0
			c.nextTry = time.Time{}
			c.failedOn = 0
		}
		c.mu.Unlock()

		if err != nil {
			c.logf("config v%d failed (%d in a row, next try in %s): %v", desired, fails, wait, err)
			return
		}
		c.logf("config v%d applied", desired)
	}()
}

func (c *configSync) backoff() time.Duration {
	d := c.retryMin
	for i := 1; i < c.fails && d < c.retryMax; i++ {
		d *= 2
	}
	if d > c.retryMax {
		d = c.retryMax
	}
	return d
}
