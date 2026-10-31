// sr-apply installs a controller-rendered customer-side bundle on a CPE and
// replays the installed version at boot.
//
// It is deliberately a separate binary from sr-agent: applying config needs
// root, vppctl and FRR, while the agent only needs its identity directory. It
// also keeps the seam clean for the delivery channel that does not exist yet:
// whoever delivers the bundle (agent or operator) just runs
//
//	sr-apply apply --bundle <file | ->
//
// afterwards the box owns the config and replays it without the controller.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"sdwan/controller/internal/apply"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "apply":
		err = runApply(ctx, os.Args[2:])
	case "boot":
		err = runBoot(ctx, os.Args[2:])
	case "status":
		err = runStatus(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "sr-apply: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  sr-apply apply  --bundle <file|-> [--force] [--dry-run] [common]
  sr-apply boot   [--frr-diff] [--dry-run] [common]
  sr-apply status [--root DIR]

apply validates the bundle, persists it under <root>/versions/<n>, then applies
VPP -> kernel -> FRR -> overlay route tables in that order. boot replays the
current version after VPP starts; the FRR config is already installed, so no
controller is contacted. boot does not push to sr-route: at that point in the
boot checklist sr-route is not running yet, and the route.json this writes is
the update.

common: --root DIR (default /var/lib/sr-cpe), --vppctl PATH, --frr-conf PATH,
        --frr-reload TOOL, --route-conf PATH (default /etc/sdwan/route.json),
        --no-route-push, --wait DURATION`)
}

// commonOpts are the paths and knobs every subcommand needs. They are struct
// fields rather than a tuple of return values because the box's own file layout
// (vppctl, FRR, sr-route, sr-fib) keeps growing a new one.
type commonOpts struct {
	root        string
	vppctl      string
	frrConf     string
	frrReload   string
	routeConf   string
	noRoutePush bool
	wait        time.Duration
	dry         bool
}

func (o *commonOpts) register(fs *flag.FlagSet) {
	fs.StringVar(&o.root, "root", "/var/lib/sr-cpe", "state directory on the box")
	fs.StringVar(&o.vppctl, "vppctl", "vppctl", "vppctl binary")
	fs.StringVar(&o.frrConf, "frr-conf", "/etc/frr/frr.conf", "FRR config path")
	fs.StringVar(&o.frrReload, "frr-reload", "", "frr-reload.py to diff the config through vtysh (default: /usr/lib/frr/frr-reload.py)")
	fs.StringVar(&o.routeConf, "route-conf", "/etc/sdwan/route.json",
		"sr-route config file; the bundle's 4th segment is merged into it (it is never created here)")
	fs.BoolVar(&o.noRoutePush, "no-route-push", false,
		"只改盘上那份，不推给运行中的 sr-route（下次 sr-route 启动才生效）")
	fs.DurationVar(&o.wait, "wait", 10*time.Second, "how long to wait for lcp TAPs")
	fs.BoolVar(&o.dry, "dry-run", false, "print the planned commands without changing anything")
}

func (o *commonOpts) applier() *apply.Applier {
	a := apply.New(apply.Store{Root: o.root})
	a.VPPCtl = o.vppctl
	a.FRRConf = o.frrConf
	a.FRRReload = o.frrReload
	a.RouteConf = o.routeConf
	a.NoRoutePush = o.noRoutePush
	a.Wait = o.wait
	a.DryRun = o.dry
	return a
}

func runApply(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	bundlePath := fs.String("bundle", "", "bundle JSON file, or - for stdin")
	force := fs.Bool("force", false, "apply even when the bundle has blocking errors")
	var o commonOpts
	o.register(fs)
	_ = fs.Parse(args)
	if *bundlePath == "" {
		return fmt.Errorf("--bundle is required")
	}

	var r *os.File
	var err error
	if *bundlePath == "-" {
		r = os.Stdin
	} else {
		r, err = os.Open(*bundlePath)
		if err != nil {
			return err
		}
		defer r.Close()
	}
	b, err := apply.DecodeBundle(r)
	if err != nil {
		return err
	}
	if errs := b.BlockingErrors(); len(errs) > 0 {
		fmt.Printf("校验有 %d 个错误（%s）；%s\n", len(errs), errs[0], forceLine(*force))
	}
	if err := o.applier().Apply(ctx, b, *force); err != nil {
		return err
	}
	fmt.Printf("applied version %d\n", b.Version)
	return nil
}

func forceLine(force bool) string {
	if force {
		return "--force 已指定，仍会下发"
	}
	return "拒绝下发；确认要试可以加 --force"
}

func runBoot(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("boot", flag.ExitOnError)
	frrDiff := fs.Bool("frr-diff", false, "reconcile the running FRR after replay (apply already did; boot normally lets FRR read its own file)")
	var o commonOpts
	o.register(fs)
	_ = fs.Parse(args)
	inst, err := apply.Store{Root: o.root}.Current()
	if err != nil {
		return err
	}
	if err := o.applier().Boot(ctx, *frrDiff); err != nil {
		return err
	}
	fmt.Printf("replayed version %d\n", inst.Manifest.Version)
	return nil
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	root := fs.String("root", "/var/lib/sr-cpe", "state directory on the box")
	_ = fs.Parse(args)
	store := apply.Store{Root: *root}
	out := map[string]any{"root": *root}

	inst, err := store.Current()
	if err != nil {
		out["installed"] = false
		if err != apply.ErrNoConfig {
			out["error"] = err.Error()
		}
	} else {
		out["installed"] = true
		out["version"] = inst.Manifest.Version
		out["installed_at_unix"] = inst.Manifest.InstalledAt
		out["node_id"] = inst.Manifest.NodeID
		out["sha256"] = inst.Manifest.SHA256
	}
	if st, err := store.ReadStatus(); err == nil {
		out["last_run"] = st
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(strings.TrimSpace(string(data)))
	return nil
}
