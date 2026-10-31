// Package apply installs the controller's customer-side artifacts on a CPE and
// replays them at boot.
//
// Why this exists: VPP keeps table/MTU/admin-state/lcp only in memory, so a
// restart wipes exactly the half FRR does not own. A box that needs the
// controller to push again after every reboot is not a box, so the artifacts
// are written to disk *before* they are applied and `sr-apply boot` replays the
// same version after VPP starts.
//
// Ordering is the part that matters:
//
//  1. VPP fragment: tables, interface table/MTU/state, lcp TAPs
//  2. wait for the TAPs to exist in the kernel
//  3. kernel fragment: VRF devices, enslave TAPs, sysctls
//  4. FRR config: persisted to /etc/frr/frr.conf; FRR loads it at start
//  5. overlay segment: merged into /etc/sdwan/route.json, then PUT to the running
//     sr-route (see route.go)
//
// Addresses are deliberately absent: FRR owns them, and lcp-sync mirrors them
// into VPP. Setting them here would fight the mirror (measured on lab-cpe1).
//
// The applier never talks to the controller. The bundle it consumes is the same
// JSON the console shows (frr/vpp/boot + checks); delivering that bundle is a
// separate concern.
package apply

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrNoConfig means the box has never had a version installed.
var ErrNoConfig = errors.New("no installed configuration")

// Check mirrors the controller's ConfigCheck. Only "error" blocks an apply;
// warnings and info are shown but do not stop it.
type Check struct {
	Severity string `json:"severity"`
	Text     string `json:"text"`
}

// Artifacts is the four-way split the controller renders. Route is the overlay
// segment (the tunnels/vrfs keys of the box's route.json) and is empty in a
// bundle published before O6-b -- sr-apply must keep reading those.
type Artifacts struct {
	FRR   string `json:"frr"`
	VPP   string `json:"vpp"`
	Boot  string `json:"boot"`
	Route string `json:"route,omitempty"`
}

// Bundle is what the controller hands out and what the box persists.
type Bundle struct {
	NodeID      uint16    `json:"node_id"`
	Version     int       `json:"version"`
	GeneratedAt int64     `json:"generated_at_unix,omitempty"`
	Checks      []Check   `json:"checks,omitempty"`
	Artifacts   Artifacts `json:"artifacts"`
}

// DecodeBundle reads and minimally validates a bundle. Unknown fields are
// ignored on purpose: the controller may grow fields this binary does not know.
func DecodeBundle(r io.Reader) (Bundle, error) {
	var b Bundle
	dec := json.NewDecoder(r)
	if err := dec.Decode(&b); err != nil {
		return Bundle{}, fmt.Errorf("bundle is not JSON: %w", err)
	}
	if b.Version < 1 {
		return Bundle{}, fmt.Errorf("bundle version must be >= 1 (got %d)", b.Version)
	}
	if strings.TrimSpace(b.Artifacts.Boot) == "" {
		return Bundle{}, errors.New("bundle has no boot fragment")
	}
	return b, nil
}

// BlockingErrors lists the checks that must stop an apply. The page lets a
// draft be saved with errors; this is the gate that keeps it off the device.
func (b Bundle) BlockingErrors() []string {
	var out []string
	for _, c := range b.Checks {
		if c.Severity == "error" {
			out = append(out, c.Text)
		}
	}
	return out
}

// Manifest is written next to the artifacts. The hashes are verified again at
// boot: a truncated or hand-edited file must not be executed as root.
type Manifest struct {
	Version     int               `json:"version"`
	NodeID      uint16            `json:"node_id,omitempty"`
	InstalledAt int64             `json:"installed_at_unix"`
	SHA256      map[string]string `json:"sha256"`
}

// Installed is one readable version on disk.
type Installed struct {
	Dir      string
	Manifest Manifest
	FRR      string
	VPP      string
	Boot     string
	Route    string
}

const (
	frrFile   = "frr.conf"
	vppFile   = "vpp.conf"
	bootFile  = "boot.sh"
	routeFile = "route.json"
)

// artifactFile is one file inside a version directory.
type artifactFile struct {
	name    string
	content string
}

// Store is the on-box versioned store. Layout:
//
//	<root>/versions/<n>/{manifest.json,frr.conf,vpp.conf,boot.sh[,route.json]}
//	<root>/current -> versions/<n>
//	<root>/status.json
//
// The current pointer is switched only after every file is on disk, so a crash
// during install leaves the previous version in charge.
type Store struct {
	Root string
}

func (s Store) versions() string { return filepath.Join(s.Root, "versions") }

func (s Store) versionDir(v int) string {
	return filepath.Join(s.versions(), strconv.Itoa(v))
}

// Install writes the bundle and makes it current. Reinstalling the same version
// with identical content is a no-op; different content under an already
// installed version is refused (versions are the controller's, not ours).
func (s Store) Install(b Bundle) (Installed, error) {
	if b.Version < 1 {
		return Installed{}, fmt.Errorf("version %d is not installable", b.Version)
	}
	if strings.TrimSpace(b.Artifacts.Boot) == "" {
		return Installed{}, errors.New("refusing to install a bundle with no boot fragment")
	}
	if err := os.MkdirAll(s.versions(), 0o755); err != nil {
		return Installed{}, err
	}

	dir := s.versionDir(b.Version)
	if _, err := os.Stat(dir); err == nil {
		cur, err := s.read(b.Version)
		if err != nil {
			return Installed{}, fmt.Errorf("version %d exists but cannot be read: %w", b.Version, err)
		}
		if cur.FRR != b.Artifacts.FRR || cur.VPP != b.Artifacts.VPP ||
			cur.Boot != b.Artifacts.Boot || cur.Route != b.Artifacts.Route {
			return Installed{}, fmt.Errorf("version %d is already installed with different content", b.Version)
		}
		return cur, nil
	}

	tmp, err := os.MkdirTemp(s.Root, ".install-")
	if err != nil {
		return Installed{}, err
	}
	defer os.RemoveAll(tmp)

	files := []artifactFile{
		{name: frrFile, content: b.Artifacts.FRR},
		{name: vppFile, content: b.Artifacts.VPP},
		{name: bootFile, content: b.Artifacts.Boot},
	}
	if b.Artifacts.Route != "" {
		files = append(files, artifactFile{name: routeFile, content: b.Artifacts.Route})
	}
	hashes := make(map[string]string, len(files))
	for _, f := range files {
		sum := sha256.Sum256([]byte(f.content))
		hashes[f.name] = hex.EncodeToString(sum[:])
		if err := writeFileSync(filepath.Join(tmp, f.name), []byte(f.content), 0o644); err != nil {
			return Installed{}, err
		}
	}
	man := Manifest{Version: b.Version, NodeID: b.NodeID, InstalledAt: time.Now().Unix(), SHA256: hashes}
	manJSON, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return Installed{}, err
	}
	if err := writeFileSync(filepath.Join(tmp, "manifest.json"), append(manJSON, '\n'), 0o644); err != nil {
		return Installed{}, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		// Another process may have installed the same version concurrently.
		if _, statErr := os.Stat(dir); statErr == nil {
			return s.read(b.Version)
		}
		return Installed{}, err
	}
	if err := s.pointCurrent(b.Version); err != nil {
		return Installed{}, err
	}
	return s.read(b.Version)
}

func (s Store) pointCurrent(version int) error {
	link := filepath.Join(s.Root, "current")
	tmp := link + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(filepath.Join("versions", strconv.Itoa(version)), tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}

// Current returns the version the boot replayer would use.
func (s Store) Current() (Installed, error) {
	target, err := os.Readlink(filepath.Join(s.Root, "current"))
	if os.IsNotExist(err) {
		return Installed{}, ErrNoConfig
	}
	if err != nil {
		return Installed{}, err
	}
	v, err := strconv.Atoi(filepath.Base(target))
	if err != nil || v < 1 {
		return Installed{}, fmt.Errorf("current points at an unreadable version: %q", target)
	}
	return s.read(v)
}

func (s Store) read(version int) (Installed, error) {
	dir := s.versionDir(version)
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return Installed{}, err
	}
	var man Manifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return Installed{}, fmt.Errorf("manifest: %w", err)
	}
	inst := Installed{Dir: dir, Manifest: man}
	for _, f := range []struct {
		name     string
		dst      *string
		optional bool
	}{
		{frrFile, &inst.FRR, false},
		{vppFile, &inst.VPP, false},
		{bootFile, &inst.Boot, false},
		{routeFile, &inst.Route, true},
	} {
		data, err := os.ReadFile(filepath.Join(dir, f.name))
		if os.IsNotExist(err) && f.optional {
			// A version installed before O6-b has no route.json. Empty means "this
			// bundle had no 4th segment", which is what the box was told.
			continue
		}
		if err != nil {
			return Installed{}, err
		}
		want := man.SHA256[f.name]
		sum := sha256.Sum256(data)
		if want == "" || hex.EncodeToString(sum[:]) != want {
			return Installed{}, fmt.Errorf("%s failed checksum", f.name)
		}
		*f.dst = string(data)
	}
	return inst, nil
}

func writeFileSync(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Status is the result of the last apply/boot, kept next to the versions.
type Status struct {
	Version   int      `json:"version"`
	OK        bool     `json:"ok"`
	Error     string   `json:"error,omitempty"`
	Notes     []string `json:"notes,omitempty"` // e.g. 已落盘、未热更 -- not a failure, never shown as done
	AppliedAt int64    `json:"applied_at_unix"`
	Source    string   `json:"source"` // apply | boot
}

func (s Store) RecordStatus(st Status) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileSync(filepath.Join(s.Root, "status.json"), append(data, '\n'), 0o644)
}

func (s Store) ReadStatus() (Status, error) {
	raw, err := os.ReadFile(filepath.Join(s.Root, "status.json"))
	if err != nil {
		return Status{}, err
	}
	var st Status
	if err := json.Unmarshal(raw, &st); err != nil {
		return Status{}, err
	}
	return st, nil
}

// Executor runs one command and returns its combined output. It exists so the
// applier's ordering can be tested without VPP.
type Executor interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

type execRunner struct{}

// Run captures into a temp file rather than a pipe. frrinit.sh starts watchfrr,
// which inherits stdout and keeps it open; CombinedOutput would then block
// forever waiting for EOF that never comes (measured on the probe box).
func (execRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	f, err := os.CreateTemp("", "sr-apply-exec-*.log")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	defer f.Close()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = f
	cmd.Stderr = f
	err = cmd.Run()
	f.Sync()
	if _, serr := f.Seek(0, 0); serr != nil && err == nil {
		err = serr
	}
	out, rerr := io.ReadAll(f)
	if err == nil {
		err = rerr
	}
	return string(out), err
}

// Applier applies or replays one installed version.
type Applier struct {
	Store     Store
	VPPCtl    string
	FRRConf   string
	FRRReload string // shell command; empty means autodetect
	// RouteConf is the box's own sr-route file. The overlay segment of a bundle is
	// merged into it; it is never created from scratch here.
	RouteConf string
	// NoRoutePush suppresses the hot PUT to sr-route, so a change waits for the
	// next sr-route start. A lab judgement knob, not a normal way to run.
	NoRoutePush bool
	Wait        time.Duration
	DryRun      bool
	Exec        Executor
	Sleep       func(time.Duration)
	TAPExists   func(string) bool
	Logf        func(format string, args ...any)
	Now         func() time.Time
	HTTP        *http.Client
}

// New fills the defaults used on a box.
func New(store Store) *Applier {
	return &Applier{
		Store:     store,
		VPPCtl:    "vppctl",
		FRRConf:   "/etc/frr/frr.conf",
		RouteConf: "/etc/sdwan/route.json",
		Wait:      10 * time.Second,
		Exec:      execRunner{},
		Sleep:     time.Sleep,
		TAPExists: func(name string) bool { _, err := os.Stat("/sys/class/net/" + name); return err == nil },
		Logf:      func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
		Now:       time.Now,
		HTTP:      &http.Client{Timeout: routePushTimeout},
	}
}

func (a *Applier) setDefaults() {
	def := New(a.Store)
	if a.VPPCtl == "" {
		a.VPPCtl = def.VPPCtl
	}
	if a.FRRConf == "" {
		a.FRRConf = def.FRRConf
	}
	if a.RouteConf == "" {
		a.RouteConf = def.RouteConf
	}
	if a.HTTP == nil {
		a.HTTP = def.HTTP
	}
	if a.Wait == 0 {
		a.Wait = def.Wait
	}
	if a.Exec == nil {
		a.Exec = def.Exec
	}
	if a.Sleep == nil {
		a.Sleep = def.Sleep
	}
	if a.TAPExists == nil {
		a.TAPExists = def.TAPExists
	}
	if a.Logf == nil {
		a.Logf = def.Logf
	}
	if a.Now == nil {
		a.Now = def.Now
	}
}

// Apply validates, installs and then applies a bundle. With allowBlocking the
// caller has explicitly overridden the checks (--force); the checks are still
// recorded by the controller and shown on the page.
func (a *Applier) Apply(ctx context.Context, b Bundle, allowBlocking bool) error {
	a.setDefaults()
	if errs := b.BlockingErrors(); len(errs) > 0 && !allowBlocking {
		return fmt.Errorf("refusing to apply version %d: %d blocking error(s), first: %s", b.Version, len(errs), errs[0])
	}
	if a.DryRun {
		a.Logf("would install version %d (frr %d B, vpp %d B, boot %d B, route %d B)", b.Version,
			len(b.Artifacts.FRR), len(b.Artifacts.VPP), len(b.Artifacts.Boot), len(b.Artifacts.Route))
		inst := Installed{
			Dir:      "<dry-run>",
			Manifest: Manifest{Version: b.Version},
			FRR:      b.Artifacts.FRR,
			VPP:      b.Artifacts.VPP,
			Boot:     b.Artifacts.Boot,
			Route:    b.Artifacts.Route,
		}
		if err := a.replay(ctx, inst); err != nil {
			return err
		}
		a.Logf("would write %s from version %d", a.FRRConf, b.Version)
		if _, err := a.applyRoute(ctx, inst, !a.NoRoutePush); err != nil {
			return err
		}
		return nil
	}
	inst, err := a.Store.Install(b)
	if err != nil {
		return err
	}
	if b.Version != inst.Manifest.Version {
		return fmt.Errorf("installed version %d, bundle says %d", inst.Manifest.Version, b.Version)
	}
	a.Logf("installed version %d at %s", inst.Manifest.Version, inst.Dir)

	var notes []string
	err = a.replay(ctx, inst)
	if err == nil {
		if ferr := a.installFRR(inst); ferr != nil {
			err = ferr
		} else {
			err = a.reloadFRR(ctx)
		}
	}
	// Overlay last: replay/installFRR/reloadFRR are what make the VPP tables and
	// the FRR config exist, and a box that failed those must not be left holding a
	// route.json that claims VRFs nothing created.
	if err == nil {
		note, rerr := a.applyRoute(ctx, inst, !a.NoRoutePush)
		if rerr != nil {
			err = rerr
		} else if note != "" {
			notes = append(notes, note)
		}
	}
	a.record(inst.Manifest.Version, "apply", err, notes)
	return err
}

// Boot replays the current version after VPP starts. It never needs the
// controller: the FRR config is already in /etc/frr/frr.conf, and this only
// restores what VPP and the kernel forgot.
func (a *Applier) Boot(ctx context.Context, reloadFRR bool) error {
	a.setDefaults()
	inst, err := a.Store.Current()
	if err != nil {
		return err
	}
	a.Logf("replaying version %d from %s", inst.Manifest.Version, inst.Dir)
	var notes []string
	err = a.replay(ctx, inst)
	if err == nil {
		err = a.installFRR(inst)
	}
	if err == nil && reloadFRR {
		err = a.reloadFRR(ctx)
	}
	// No hot PUT here: at boot the checklist has not started sr-route yet (apply
	// runs before route in start.sh), so the file this writes IS the update.
	if err == nil {
		note, rerr := a.applyRoute(ctx, inst, false)
		if rerr != nil {
			err = rerr
		} else if note != "" {
			notes = append(notes, note)
		}
	}
	a.record(inst.Manifest.Version, "boot", err, notes)
	return err
}

func (a *Applier) record(version int, source string, err error, notes []string) {
	st := Status{Version: version, OK: err == nil, Notes: notes, AppliedAt: a.Now().Unix(), Source: source}
	if err != nil {
		st.Error = err.Error()
	}
	if rerr := a.Store.RecordStatus(st); rerr != nil {
		a.Logf("warning: cannot record status: %v", rerr)
	}
}

// replay is the ordered part shared by apply and boot.
func (a *Applier) replay(ctx context.Context, inst Installed) error {
	if err := a.runVPP(ctx, inst.VPP); err != nil {
		return err
	}
	if a.DryRun {
		if err := a.runBoot(ctx, inst); err != nil {
			return err
		}
		return nil
	}
	if err := a.waitTAPs(inst.VPP); err != nil {
		return err
	}
	return a.runBoot(ctx, inst)
}

var (
	reLCPCreate = regexp.MustCompile(`^lcp\s+create\s+(\S+)\s+host-if\s+(\S+)$`)
	reSetTable  = regexp.MustCompile(`^set\s+interface\s+ip\s+table\s+(\S+)\s+(\d+)$`)
	reTableID   = regexp.MustCompile(`ip4 table-id\s+(\d+)`)
)

func artifactCommands(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// runVPP executes the fragment, skipping work that is already done. VPP's CLI
// exits 0 even on errors, so every output is inspected; the two idempotent
// cases (interface already in the wanted table, lcp pair already present) are
// resolved by asking VPP first instead of by ignoring an error.
func (a *Applier) runVPP(ctx context.Context, text string) error {
	for _, cmd := range artifactCommands(text) {
		if m := reLCPCreate.FindStringSubmatch(cmd); m != nil {
			vppIf, tap := m[1], m[2]
			paired, current, err := a.lcpState(ctx, vppIf)
			if err != nil {
				return err
			}
			if paired {
				if current == tap {
					a.Logf("vpp: %s already paired with %s, skip", vppIf, tap)
					continue
				}
				return fmt.Errorf("vpp: %s is already paired with %s, fragment wants %s", vppIf, current, tap)
			}
		}
		if m := reSetTable.FindStringSubmatch(cmd); m != nil {
			want, _ := strconv.Atoi(m[2])
			table, hasAddr, err := a.ifaceTable(ctx, m[1])
			if err != nil {
				return err
			}
			if table == want {
				a.Logf("vpp: %s already in table %d, skip", m[1], want)
				continue
			}
			if hasAddr {
				return fmt.Errorf("vpp: %s has an address in table %d; cannot move it to table %d without FRR dropping the address first", m[1], table, want)
			}
		}
		if a.DryRun {
			a.Logf("vppctl %s", cmd)
			continue
		}
		out, err := a.Exec.Run(ctx, a.VPPCtl, strings.Fields(cmd)...)
		if err != nil {
			return fmt.Errorf("vppctl %s: %v: %s", cmd, err, strings.TrimSpace(out))
		}
		if msg := vppError(out); msg != "" {
			return fmt.Errorf("vppctl %s: %s", cmd, msg)
		}
	}
	return nil
}

// vppError returns a non-empty message when vppctl printed an error. vppctl
// reports failures in the output and still exits 0.
func vppError(out string) string {
	l := strings.ToLower(out)
	for _, marker := range []string{"unknown input", "unknown interface", "failed", "error"} {
		if strings.Contains(l, marker) {
			return strings.TrimSpace(out)
		}
	}
	return ""
}

func (a *Applier) vppShow(ctx context.Context, cmd string) (string, error) {
	if a.DryRun {
		return "", nil
	}
	out, err := a.Exec.Run(ctx, a.VPPCtl, strings.Fields(cmd)...)
	if err != nil {
		return "", fmt.Errorf("vppctl %s: %v: %s", cmd, err, strings.TrimSpace(out))
	}
	if msg := vppError(out); msg != "" {
		return "", fmt.Errorf("vppctl %s: %s", cmd, msg)
	}
	return out, nil
}

// lcpState reports whether vppIf already has an lcp pair and with which host-if.
func (a *Applier) lcpState(ctx context.Context, vppIf string) (paired bool, hostIf string, err error) {
	out, err := a.vppShow(ctx, "show lcp")
	if err != nil {
		return false, "", err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		// itf-pair: [0] host-eth2 tap4096 lcp-x0 14 type tap
		if len(f) < 5 || f[0] != "itf-pair:" {
			continue
		}
		if f[2] == vppIf {
			return true, f[4], nil
		}
	}
	return false, "", nil
}

// ifaceTable returns the interface's IP table (0 when VPP prints no table-id)
// and whether it currently has an L3 address.
func (a *Applier) ifaceTable(ctx context.Context, vppIf string) (table int, hasAddr bool, err error) {
	out, err := a.vppShow(ctx, "show interface address "+vppIf)
	if err != nil {
		return 0, false, err
	}
	if m := reTableID.FindStringSubmatch(out); m != nil {
		t, _ := strconv.Atoi(m[1])
		return t, true, nil
	}
	return 0, strings.Contains(out, "L3 "), nil
}

// waitTAPs waits for the kernel TAPs the fragment asked VPP to create. Without
// this, the kernel fragment races lcp create and fails with "no such device".
func (a *Applier) waitTAPs(vppText string) error {
	var taps []string
	for _, cmd := range artifactCommands(vppText) {
		if m := reLCPCreate.FindStringSubmatch(cmd); m != nil {
			taps = append(taps, m[2])
		}
	}
	if len(taps) == 0 {
		return nil
	}
	attempts := int(a.Wait/(250*time.Millisecond)) + 1
	for i := 0; i < attempts; i++ {
		var missing []string
		for _, tap := range taps {
			if !a.TAPExists(tap) {
				missing = append(missing, tap)
			}
		}
		if len(missing) == 0 {
			return nil
		}
		if i == attempts-1 {
			return fmt.Errorf("kernel TAP(s) %s did not appear within %s", strings.Join(missing, ", "), a.Wait)
		}
		a.Sleep(250 * time.Millisecond)
	}
	return nil
}

// runBoot executes the kernel fragment. The fragment starts with set -e and is
// intentionally allowed to fail loudly: a box that cannot create its VRFs must
// not pretend the config is in place.
func (a *Applier) runBoot(ctx context.Context, inst Installed) error {
	if a.DryRun {
		a.Logf("sh %s", filepath.Join(inst.Dir, bootFile))
		return nil
	}
	out, err := a.Exec.Run(ctx, "sh", filepath.Join(inst.Dir, bootFile))
	if err != nil {
		return fmt.Errorf("boot fragment failed: %v: %s", err, tail(out, 400))
	}
	return nil
}

// installFRR writes the artifact to /etc/frr/frr.conf atomically, keeping one
// previous copy. FRR loads this file itself at boot, which is why boot replay
// does not need to generate anything.
func (a *Applier) installFRR(inst Installed) error {
	cur, err := os.ReadFile(a.FRRConf)
	if err == nil && string(cur) == inst.FRR {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if a.DryRun {
		a.Logf("write %s from version %d", a.FRRConf, inst.Manifest.Version)
		return nil
	}
	dir := filepath.Dir(a.FRRConf)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err == nil {
		_ = writeFileSync(a.FRRConf+".prev", cur, 0o644)
	}
	tmp := a.FRRConf + ".tmp"
	if err := writeFileSync(tmp, []byte(inst.FRR), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.FRRConf)
}

// reloadFRR restarts FRR so it picks up the installed config. Restart rather
// than merge: a reload leaves removed neighbors and interfaces behind.
func (a *Applier) reloadFRR(ctx context.Context) error {
	cmd := a.FRRReload
	if cmd == "" {
		if _, err := os.Stat("/usr/lib/frr/frrinit.sh"); err == nil {
			cmd = "/usr/lib/frr/frrinit.sh restart"
		} else {
			cmd = "systemctl restart frr"
		}
	}
	if a.DryRun {
		a.Logf("frr reload: %s", cmd)
		return nil
	}
	out, err := a.Exec.Run(ctx, "sh", "-c", cmd)
	if err != nil {
		return fmt.Errorf("frr reload (%s) failed: %v: %s", cmd, err, tail(out, 400))
	}
	a.Logf("frr reloaded (%s)", cmd)
	return nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
