package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/plyrthn/nx-debug-cli/internal/htc"
)

// Installing an nsp is not a daemon operation and never was: the daemon has no
// RPC for it. What the official tool does is launch the target's own
// DevMenuCommand and let it read the package back off the host, which is
// something nxdbg can do just as well, so this works with `nxdbg serve` and no
// daemon at all.
//
// The one thing to know when running it that way is where the file has to be.
// The target asks the host for it by path, and HTCFS answers only for what is
// under the root `nxdbg serve` was started with, so the nsp has to be inside
// that root. `nxdbg install` runs as its own process, separate from `nxdbg
// serve`, so it has no way to ask a serve already running elsewhere what root
// it actually used - the best this can do is warn when the path falls outside
// the current working directory, which is serve's own default root when
// `--root` isn't given. Getting it wrong otherwise shows up minutes into a
// run as the target failing to open a file that is plainly there.

// storageNames are the values DevMenuCommand's -s option takes.
var storageNames = map[string]bool{"sdcard": true, "builtin": true, "auto": true}

// warnIfOutsideRoot flags the one root-containment mistake this process can
// actually detect. wd is meant to be this process's own working directory,
// which only matches serve's real HTCFS root when serve was started without
// --root and from the same place - true for the common case, but not
// something this can confirm, hence a warning rather than a hard error.
func warnIfOutsideRoot(abs, wd string) string {
	rel, err := filepath.Rel(wd, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Sprintf("warning: %s is outside %s - if `nxdbg serve` is rooted there (its default with no --root), the target won't be able to read it back over HTCFS", abs, wd)
	}
	return ""
}

// cmdInstall installs an nsp on the target.
func cmdInstall(ctx context.Context, serial string, rest []string) error {
	opts, err := parseInstallArgs(rest)
	if err != nil {
		return err
	}

	abs, err := filepath.Abs(opts.path)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory; install takes an .nsp", abs)
	}
	if wd, wdErr := os.Getwd(); wdErr == nil {
		if warning := warnIfOutsideRoot(abs, wd); warning != "" {
			fmt.Println(warning)
		}
	}

	fmt.Printf("installing %s (%s) to %s\n", filepath.Base(abs), humanSize(info.Size()), opts.storage)
	fmt.Println("the target reads it back over HTCFS, so this runs at link speed")

	args := installArgs(abs, opts)
	run, err := htc.RunDevMenu(ctx, serial, args, func(line string) {
		if isDevMenuNoise(line) {
			return
		}
		fmt.Printf("  %s\n", formatDevMenuLine(line))
	})
	if err != nil {
		return err
	}
	if !run.Succeeded {
		return fmt.Errorf("install did not report success")
	}
	fmt.Println("✓ installed")
	return nil
}

// installOptions is a parsed install command line.
type installOptions struct {
	path    string
	storage string
	force   bool
}

func parseInstallArgs(rest []string) (installOptions, error) {
	opts := installOptions{storage: "sdcard", force: true}
	var path string
	for i := 0; i < len(rest); i++ {
		switch a := rest[i]; {
		case a == "--builtin":
			opts.storage = "builtin"
		case a == "--sdcard":
			opts.storage = "sdcard"
		case a == "--auto":
			opts.storage = "auto"
		case a == "--no-force":
			opts.force = false
		case strings.HasPrefix(a, "-"):
			return opts, fmt.Errorf("unknown option %s (try `nxdbg help install`)", a)
		default:
			if path != "" {
				return opts, fmt.Errorf("install takes one .nsp, got %q and %q", path, a)
			}
			path = a
		}
	}
	if path == "" {
		return opts, fmt.Errorf("usage: nxdbg install <serial> <file.nsp> [--sdcard|--builtin|--auto] [--no-force]")
	}
	if !storageNames[opts.storage] {
		return opts, fmt.Errorf("unknown storage %q", opts.storage)
	}
	opts.path = path
	return opts, nil
}

// installArgs builds the DevMenuCommand line.
//
// --force is the default here, unlike the official tool. Without it an install
// over the same or a newer version is refused, and since the reason for
// building a package again is nearly always to replace the one already on the
// target, defaulting to refusing would make the common case the one that needs
// a flag. --no-force asks for the official behaviour.
func installArgs(abs string, opts installOptions) string {
	parts := []string{"application", "install", abs}
	if opts.force {
		parts = append(parts, "--force")
	}
	return strings.Join(append(parts, "-s", opts.storage), " ")
}

// isDevMenuNoise reports whether a log line came from something other than the
// command being run. The target log is shared, so anything else running keeps
// writing to it throughout an install.
func isDevMenuNoise(line string) bool {
	t := strings.TrimSpace(line)
	return t == "" || strings.HasPrefix(t, "[20") || strings.Contains(t, "LogCriWare")
}

// progressLine matches DevMenuCommand's own byte-count progress line, e.g.
// "9192849408 / 9192849408" - the one line worth reformatting, since raw
// byte counts read a lot worse than the units the initial "installing ..."
// line already uses.
var progressLine = regexp.MustCompile(`^(\d+) / (\d+)$`)

// formatDevMenuLine rewrites a raw byte-count progress line into human units
// plus a percentage, and returns anything else exactly as sent.
func formatDevMenuLine(line string) string {
	m := progressLine.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return line
	}
	done, err1 := strconv.ParseInt(m[1], 10, 64)
	total, err2 := strconv.ParseInt(m[2], 10, 64)
	if err1 != nil || err2 != nil || total <= 0 {
		return line
	}
	pct := float64(done) / float64(total) * 100
	return fmt.Sprintf("%s / %s (%.0f%%)", humanSize(done), humanSize(total), pct)
}

// cmdUninstall removes an installed application by id.
func cmdUninstall(ctx context.Context, serial string, rest []string) error {
	id := strings.TrimSpace(rest[0])
	if id == "" {
		return fmt.Errorf("usage: nxdbg uninstall <serial> <application-id>")
	}
	run, err := htc.RunDevMenu(ctx, serial, "application uninstall "+id, func(line string) {
		if !isDevMenuNoise(line) {
			fmt.Printf("  %s\n", line)
		}
	})
	if err != nil {
		return err
	}
	if !run.Succeeded {
		return fmt.Errorf("uninstall did not report success")
	}
	fmt.Printf("✓ uninstalled %s\n", id)
	return nil
}

// cmdApps lists what is installed on the target.
func cmdApps(ctx context.Context, serial string, _ []string) error {
	_, err := htc.RunDevMenu(ctx, serial, "application list", func(line string) {
		if !isDevMenuNoise(line) && strings.TrimSpace(line) != "[SUCCESS]" {
			fmt.Println(line)
		}
	})
	return err
}

// humanSize renders a byte count the way a person would say it.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
