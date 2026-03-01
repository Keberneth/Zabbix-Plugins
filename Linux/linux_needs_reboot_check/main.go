//go:build linux
// +build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"golang.zabbix.com/sdk/errs"
	"golang.zabbix.com/sdk/plugin"
	"golang.zabbix.com/sdk/plugin/container"
)

const (
	// IMPORTANT: pluginName must match the name used in the Zabbix Agent2 plugin config section:
	//   Plugins.<pluginName>.System.Path=...
	pluginName = "NeedsRebootCheck"

	// IMPORTANT: key must NOT change (referenced by your Zabbix items).
	metricKey = "system.needs_reboot"
)

// NeedsRebootCheckPlugin implements Zabbix Agent2 Go plugin exporter.
// The plugin returns "1" if a reboot is recommended, "0" otherwise.
type NeedsRebootCheckPlugin struct {
	plugin.Base
}

var (
	_    plugin.Exporter = (*NeedsRebootCheckPlugin)(nil)
	impl NeedsRebootCheckPlugin
)

func main() {
	standalone, verbose := parseArgs(os.Args[1:])
	if standalone {
		code := runStandalone(verbose)
		fmt.Println(code)
		os.Exit(0)
	}

	if err := runPlugin(); err != nil {
		panic(err)
	}
}

func parseArgs(args []string) (standalone bool, verbose bool) {
	for _, a := range args {
		s := strings.TrimSpace(strings.ToLower(a))
		s = strings.TrimLeft(s, "-")
		s = strings.TrimSpace(s)
		switch s {
		case "standalone":
			standalone = true
		case "verbose", "v":
			verbose = true
		}
	}
	return
}

func runPlugin() error {
	// Description must end with a dot (SDK validation).
	if err := plugin.RegisterMetrics(
		&impl,
		pluginName,
		metricKey,
		"Checks if Linux needs a reboot (reboot-required flags).",
	); err != nil {
		return errs.Wrap(err, "failed to register metrics")
	}

	h, err := container.NewHandler(pluginName)
	if err != nil {
		return errs.Wrap(err, "failed to create new handler")
	}

	impl.Logger = h

	if err := h.Execute(); err != nil {
		return errs.Wrap(err, "failed to execute plugin handler")
	}
	return nil
}

func runStandalone(verbose bool) string {
	pending, reasons, err := isRebootPendingDetailed()
	if err != nil {
		// Best-effort: if we cannot determine, return 0.
		if verbose {
			fmt.Fprintln(os.Stderr, "needs_reboot_check: error:", err)
		}
		return "0"
	}

	if verbose {
		if pending {
			fmt.Fprintln(os.Stderr, "needs_reboot_check: pending reboot detected:")
			for _, r := range reasons {
				fmt.Fprintln(os.Stderr, "-", r)
			}
		} else {
			fmt.Fprintln(os.Stderr, "needs_reboot_check: no reboot-required signals detected")
		}
	}

	if pending {
		return "1"
	}
	return "0"
}

func (p *NeedsRebootCheckPlugin) Export(key string, _ []string, _ plugin.ContextProvider) (interface{}, error) {
	if key != metricKey {
		return nil, plugin.UnsupportedMetricError
	}

	pending, reasons, err := isRebootPendingDetailed()
	if err != nil {
		// Best-effort.
		if p.Logger != nil {
			p.Infof("%s: reboot pending check error: %v", pluginName, err)
		}
		return "0", nil
	}

	if pending {
		if p.Logger != nil && len(reasons) > 0 {
			p.Infof("%s: reboot recommended: %s", pluginName, strings.Join(reasons, ", "))
		}
		return "1", nil
	}

	return "0", nil
}

// isRebootPendingDetailed checks common "reboot required" signals across major distros.
// Returns pending=true if any signal indicates a reboot is recommended.
func isRebootPendingDetailed() (pending bool, reasons []string, err error) {
	reasons = make([]string, 0, 4)

	// Debian / Ubuntu: /run/reboot-required or /var/run/reboot-required exists.
	if fileExists("/run/reboot-required") || fileExists("/var/run/reboot-required") {
		pending = true
		reasons = append(reasons, "reboot-required:file")
	}

	// RHEL / Fedora / Rocky / Alma: needs-restarting -r exit code 1 => reboot recommended.
	if path, ok := lookPath("needs-restarting"); ok {
		code, _, _, runErr := runExitCode(path, "-r")
		if runErr != nil {
			// Non-exit errors are real problems (should be rare since we already looked up the path).
			return pending, reasons, runErr
		}
		if code == 1 {
			pending = true
			reasons = append(reasons, "needs-restarting:-r")
		}
	}

	// SUSE / openSUSE: zypper --quiet needs-rebooting exit code 102 => reboot suggested.
	// If the subcommand is missing or fails, we do not treat it as "reboot required",
	// but we will attempt a fallback to needs-restarting (if available).
	zypperRan := false
	zypperOK := false
	if path, ok := lookPath("zypper"); ok {
		zypperRan = true
		code, _, stderr, runErr := runExitCode(path, "--quiet", "needs-rebooting")
		if runErr != nil {
			return pending, reasons, runErr
		}
		if code == 102 {
			pending = true
			reasons = append(reasons, "zypper:needs-rebooting")
			zypperOK = true
		} else if code == 0 {
			zypperOK = true
		} else {
			// subcommand missing or other error; keep for potential fallback.
			_ = stderr
		}
	}

	// Optional fallback if zypper exists but doesn't support needs-rebooting.
	if zypperRan && !zypperOK {
		if path, ok := lookPath("needs-restarting"); ok {
			code, _, _, runErr := runExitCode(path, "-r")
			if runErr != nil {
				return pending, reasons, runErr
			}
			if code == 1 {
				pending = true
				reasons = append(reasons, "needs-restarting:-r(fallback)")
			}
		}
	}

	// Universal kernel mismatch fallback (RPM-based only):
	// Compare running kernel (uname -r) with latest installed kernel-core package.
	// This is used only if nothing else already indicated reboot.
	if !pending {
		running, e := cmdOutputTrim("uname", "-r")
		if e == nil && running != "" {
			latest, e2 := latestKernelCoreRPM()
			if e2 != nil {
				return pending, reasons, e2
			}
			if latest != "" && running != latest {
				pending = true
				reasons = append(reasons, fmt.Sprintf("kernel:mismatch(running=%s, latest=%s)", running, latest))
			}
		}
	}

	return pending, reasons, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func lookPath(name string) (path string, ok bool) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", false
	}
	return p, true
}

// runExitCode executes a command and returns its exit code.
// It treats "process exited with non-zero code" as non-fatal and returns the exit code.
func runExitCode(path string, args ...string) (code int, stdout string, stderr string, err error) {
	cmd := exec.Command(path, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	e := cmd.Run()

	stdout = strings.TrimSpace(outBuf.String())
	stderr = strings.TrimSpace(errBuf.String())

	if e == nil {
		return 0, stdout, stderr, nil
	}

	var exitErr *exec.ExitError
	if errors.As(e, &exitErr) {
		return exitErr.ExitCode(), stdout, stderr, nil
	}

	return -1, stdout, stderr, e
}

func cmdOutputTrim(name string, args ...string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	out, err := exec.Command(p, args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// latestKernelCoreRPM returns the latest installed kernel-core version string formatted as:
//   %{VERSION}-%{RELEASE}.%{ARCH}
// If rpm is not available or kernel-core is not installed, it returns "" and nil error.
func latestKernelCoreRPM() (string, error) {
	rpmPath, ok := lookPath("rpm")
	if !ok {
		return "", nil
	}

	// Query installed kernel-core packages. rpm returns non-zero if none installed.
	qf := "%{VERSION}-%{RELEASE}.%{ARCH}\n"
	out, err := exec.Command(rpmPath, "-q", "--qf", qf, "kernel-core").Output()
	if err != nil {
		// Not installed / not RPM based.
		return "", nil
	}

	// Parse lines.
	lines := strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n")
	filtered := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			filtered = append(filtered, ln)
		}
	}
	if len(filtered) == 0 {
		return "", nil
	}

	// Prefer GNU sort -V for correct version ordering (mirrors the original shell script).
	if sortPath, ok := lookPath("sort"); ok {
		sortCmd := exec.Command(sortPath, "-V")
		sortCmd.Stdin = bytes.NewReader([]byte(strings.Join(filtered, "\n") + "\n"))
		sortedOut, e := sortCmd.Output()
		if e == nil {
			sortedLines := strings.Split(strings.ReplaceAll(string(sortedOut), "\r\n", "\n"), "\n")
			last := ""
			for _, ln := range sortedLines {
				ln = strings.TrimSpace(ln)
				if ln != "" {
					last = ln
				}
			}
			if last != "" {
				return last, nil
			}
		}
		// Fall through to in-process sort if sort(1) failed unexpectedly.
	}

	sort.Strings(filtered)
	return filtered[len(filtered)-1], nil
}
