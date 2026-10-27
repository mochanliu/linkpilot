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
  sr-apply boot   [--reload-frr] [--dry-run] [common]
  sr-apply status [--root DIR]

apply validates the bundle, persists it under <root>/versions/<n>, then applies
VPP -> kernel -> FRR in that order. boot replays the current version after VPP
starts; the FRR config is already installed, so no controller is contacted.

common: --root DIR (default /var/lib/sr-cpe), --vppctl PATH, --frr-conf PATH,
        --frr-reload CMD, --wait DURATION`)
}

func commonFlags(fs *flag.FlagSet) (root, vppctl, frrConf, frrReload *string, wait *time.Duration, dry *bool) {
	root = fs.String("root", "/var/lib/sr-cpe", "state directory on the box")
	vppctl = fs.String("vppctl", "vppctl", "vppctl binary")
	frrConf = fs.String("frr-conf", "/etc/frr/frr.conf", "FRR config path")
	frrReload = fs.String("frr-reload", "", "FRR restart command (default: frrinit.sh or systemctl)")
	wait = fs.Duration("wait", 10*time.Second, "how long to wait for lcp TAPs")
	dry = fs.Bool("dry-run", false, "print the planned commands without changing anything")
	return
}

func applier(root, vppctl, frrConf, frrReload string, wait time.Duration, dry bool) *apply.Applier {
	a := apply.New(apply.Store{Root: root})
	a.VPPCtl = vppctl
	a.FRRConf = frrConf
	a.FRRReload = frrReload
	a.Wait = wait
	a.DryRun = dry
	return a
}

func runApply(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	bundlePath := fs.String("bundle", "", "bundle JSON file, or - for stdin")
	force := fs.Bool("force", false, "apply even when the bundle has blocking errors")
	root, vppctl, frrConf, frrReload, wait, dry := commonFlags(fs)
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
	a := applier(*root, *vppctl, *frrConf, *frrReload, *wait, *dry)
	if err := a.Apply(ctx, b, *force); err != nil {
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
	reloadFRR := fs.Bool("reload-frr", false, "restart FRR after replay (apply already did)")
	root, vppctl, frrConf, frrReload, wait, dry := commonFlags(fs)
	_ = fs.Parse(args)
	a := applier(*root, *vppctl, *frrConf, *frrReload, *wait, *dry)
	inst, err := apply.Store{Root: *root}.Current()
	if err != nil {
		return err
	}
	if err := a.Boot(ctx, *reloadFRR); err != nil {
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
