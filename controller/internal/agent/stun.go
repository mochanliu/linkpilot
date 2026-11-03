package agent

import (
	"context"
	"net"
	"strconv"
	"sync"
	"time"

	"sdwan/controller/internal/stun"
	"sdwan/controller/internal/wire"
)

// StunSync is the device's address-discovery loop: it holds ONE probe socket for
// its lifetime, probes whatever reflectors the controller advertises, and keeps
// the freshest verdict for the mapping report to carry.
//
// Three facts decide its shape:
//
//   - The socket is never recreated per probe. The symmetric-NAT verdict is
//     defined as "the same local socket gets a different mapping per
//     destination"; a fresh socket per round would make the verdict a guess. It
//     is also why the verdict is kept here rather than computed at report time.
//   - Reflectors are not configured on the box. The controller advertises them
//     (enroll response, then every heartbeat ack), so an operator moves the
//     observation service without touching a single CPE. An agent that has never
//     been given a list reports no mapping -- 未上报, not 0.0.0.0.
//   - A verdict is dropped when it goes too old. Re-sending a two-minute-old
//     public address as if it were fresh would keep the controller's staleness
//     timer from ever expiring, and the planner would build a tunnel onto a NAT
//     binding that no longer exists. Silence here is the correct signal: the
//     controller ages the observation out on its own.
type StunSync struct {
	mu sync.Mutex

	localBind string
	max       int // how many advertised reflectors this agent will probe

	prober     *stun.Prober
	reflectors []string
	verdict    stun.Verdict
	probedList []string // the list the current verdict was taken against
	probedAt   time.Time
	dirty      bool
	lastErr    string

	// now is a seam for tests; nothing else in here reads a clock twice.
	now  func() time.Time
	logf func(string, ...any)
}

// stunProbeGap is how often a fresh verdict is taken. It must stay well under the
// controller's observation TTL, because that TTL is what makes an observation
// untrustworthy: a device that stops being able to prove its outer address has
// to fall out of the planner, not age in place.
const stunProbeGap = 20 * time.Second

// stunMaxAge is the oldest verdict this agent will still report. One failed
// round (a reflector restart, a transient drop) must not blank the mapping, so
// this is deliberately longer than stunProbeGap -- while remaining shorter than
// the controller's TTL, so the device's own silence wins and the row expires.
const stunMaxAge = 60 * time.Second

// stunTries / stunGap are the per-reflector retry shape. Two tries at 400ms fits
// inside one beat's budget and still survives a single dropped datagram.
const (
	stunTries = 2
	stunGap   = 400 * time.Millisecond
)

func NewStunSync(localBind string, max int, logf func(string, ...any)) *StunSync {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &StunSync{localBind: localBind, max: max, now: time.Now, logf: logf}
}

// SetReflectors records what the controller just advertised. A changed list
// forces a new round: a verdict measured against the old reflectors is not a
// verdict about the new ones.
func (s *StunSync) SetReflectors(list []string) {
	if s == nil {
		return
	}
	clean := make([]string, 0, len(list))
	for _, a := range list {
		if a != "" {
			clean = append(clean, a)
		}
	}
	if s.max > 0 && len(clean) > s.max {
		clean = clean[:s.max]
	}
	s.mu.Lock()
	changed := !equalStrs(s.reflectors, clean)
	if changed {
		s.reflectors = clean
		s.dirty = true
	}
	s.mu.Unlock()
	if changed && len(clean) > 0 {
		s.logf("地址探测：控制器给了 %d 个反射器（%v）", len(clean), clean)
	}
}

// Sync runs one discovery round if one is due. It never blocks a caller's
// heartbeat for longer than the probe's own deadline, and a failure only records
// a reason -- the previous verdict stays usable until stunMaxAge says otherwise.
func (s *StunSync) Sync(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	refs := append([]string{}, s.reflectors...)
	due := len(refs) > 0 && (s.dirty || s.probedList == nil || !equalStrs(s.probedList, refs) ||
		s.now().Sub(s.probedAt) >= stunProbeGap)
	prober := s.prober
	s.mu.Unlock()
	if !due {
		return
	}

	// One socket for the process, redialled only if it died: re-binding per round
	// would change the local port and make every symmetric verdict meaningless.
	if prober == nil {
		p, err := stun.NewProber(s.localBind)
		if err != nil {
			s.fail(err)
			return
		}
		prober = p
		s.mu.Lock()
		if s.prober == nil {
			s.prober = p
		} else {
			// Somebody (another Sync, or an earlier round that redialled) won the
			// race: keep the one socket this process has always had, and close the
			// spare -- a leaked bound port would mean two source ports and a
			// symmetric verdict measured across both.
			_ = p.Close()
			prober = s.prober
		}
		s.mu.Unlock()
	}

	verdict, err := prober.Discover(ctx, refs, stunTries, stunGap)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.lastErr = err.Error()
		// The list is NOT recorded as probed: the next tick retries rather than
		// waiting out the full probe gap on a round that never answered.
		return
	}
	s.verdict = verdict
	s.probedList = refs
	s.probedAt = s.now()
	s.dirty = false
	s.lastErr = ""
}

// Loop refreshes the verdict on its own cadence. Like every other report here, a
// failure is a line in the log, not an exit: a device that stops probing must
// still heartbeat and still show up on the console.
func (s *StunSync) Loop(ctx context.Context, tick time.Duration) {
	if s == nil {
		return
	}
	if tick <= 0 {
		tick = 5 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		// A round costs up to tries*gap per reflector; give it its own budget so a
		// black-holed reflector cannot pile rounds up on top of each other.
		budget := time.Duration(len(s.Reflectors())) * time.Duration(stunTries) * stunGap
		rctx, cancel := context.WithTimeout(ctx, budget+2*time.Second)
		s.Sync(rctx)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *StunSync) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr = err.Error()
}

// Reflectors is what the controller advertised (already capped at this agent's
// own limit), for the status page and for the loop's probe budget.
func (s *StunSync) Reflectors() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.reflectors...)
}

// Apply fills in the observation half of a mapping report from the freshest
// verdict. It carries nothing when there is no verdict, when it was taken
// against a different reflector list, or when it has aged past stunMaxAge -- in
// all three cases the honest wire value is "not measured", and the controller
// then ages the row out instead of refreshing it.
func (s *StunSync) Apply(rep *wire.MappingReport) {
	if s == nil || rep == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Same socket for every round (see Prober), so the source port the peers must
	// be pointed at is this one's -- and the verdict below was measured through
	// it, which is the only reason either number may be reported.
	localPort := s.prober.LocalPort()
	if localPort <= 0 {
		return
	}
	s.applyLocked(rep, localPort)
}

// applyLocked carries the verdict onto a report. It is the only reader of the
// verdict's wire form, and it lives apart from Apply so Apply can take the
// prober's port once under the same lock instead of twice across two calls.
func (s *StunSync) applyLocked(rep *wire.MappingReport, localPort int) {
	if len(s.verdict.Mappings) == 0 || s.probedList == nil ||
		!equalStrs(s.probedList, s.reflectors) || s.now().Sub(s.probedAt) > stunMaxAge {
		return
	}
	m := s.verdict.Mappings[0]
	rep.UDPMapping = net.JoinHostPort(m.Addr, strconv.Itoa(m.Port))
	rep.Reflector = m.Reflector
	rep.ProbeLocal = m.Local
	rep.LocalPort = localPort
	rep.SymmetricNAT = s.verdict.Symmetric
	rep.SymmetricKnown = s.verdict.SymmetricKnown
}

// StunSnapshot is the local status page's view: what was advertised, what was
// measured, how old it is, and why not.
type StunSnapshot struct {
	Reflectors     []string `json:"reflectors,omitempty"`
	Mapping        string   `json:"mapping,omitempty"` // observed outer "ip:port"
	ProbeLocal     string   `json:"probe_local,omitempty"`
	Symmetric      *bool    `json:"symmetric_nat,omitempty"` // nil = 没有判定可给
	SymmetricKnown bool     `json:"symmetric_known"`
	ProbedAt       int64    `json:"probed_at_unix,omitempty"`
	AgeSec         int64    `json:"age_sec,omitempty"`
	Err            string   `json:"err,omitempty"`
}

func (s *StunSync) Snapshot() StunSnapshot {
	if s == nil {
		return StunSnapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v := StunSnapshot{Reflectors: append([]string{}, s.reflectors...), Err: s.lastErr, SymmetricKnown: s.verdict.SymmetricKnown}
	// A copy, not &s.verdict.Symmetric: the snapshot outlives the lock, and a
	// pointer into the guarded struct would let the next round change what this
	// one claims to have measured.
	if s.verdict.SymmetricKnown {
		sym := s.verdict.Symmetric
		v.Symmetric = &sym
	}
	if len(s.verdict.Mappings) > 0 {
		m := s.verdict.Mappings[0]
		v.Mapping = net.JoinHostPort(m.Addr, strconv.Itoa(m.Port))
		v.ProbeLocal = m.Local
		v.ProbedAt = s.probedAt.Unix()
		v.AgeSec = int64(s.now().Sub(s.probedAt).Seconds())
	}
	return v
}

func (s *StunSync) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.prober.Close()
	s.prober = nil
	return err
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
