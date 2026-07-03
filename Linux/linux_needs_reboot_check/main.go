//go:build linux
// +build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.zabbix.com/sdk/errs"
	"golang.zabbix.com/sdk/plugin"
	"golang.zabbix.com/sdk/plugin/container"
)

// commandTimeout bounds every external command. Without it a hung package
// manager (a held dnf/rpm/zypper lock, a stuck NFS mount behind /usr/bin) would
// block Export indefinitely and leak one child process per poll cycle. Kept
// generously below a typical Zabbix item timeout so the plugin fails fast with
// an error instead of being killed by the agent.
const commandTimeout = 15 * time.Second

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
	if err != nil && verbose {
		// Secondary-check errors are non-fatal and must never override a confirmed
		// pending=true; just surface them.
		fmt.Fprintln(os.Stderr, "needs_reboot_check: non-fatal check error:", err)
	}
	if err != nil && !pending {
		// We could not confirm a reboot and at least one check errored: best-effort 0.
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
	if err != nil && p.Logger != nil {
		// Secondary-check errors are non-fatal and must never override a confirmed
		// pending=true; just log them.
		p.Infof("%s: non-fatal reboot check error: %v", pluginName, err)
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
//
// Error handling: once an authoritative check has set pending=true, errors from
// later secondary checks must never flip the result back to "0". Non-fatal errors
// from secondary commands are accumulated and returned (joined) so callers can log
// them, but callers must treat a confirmed pending=true as authoritative regardless
// of whether err is non-nil.
func isRebootPendingDetailed() (pending bool, reasons []string, err error) {
	reasons = make([]string, 0, 4)
	var errsList []error

	// Debian / Ubuntu: /run/reboot-required or /var/run/reboot-required exists.
	if fileExists("/run/reboot-required") || fileExists("/var/run/reboot-required") {
		pending = true
		reasons = append(reasons, "reboot-required:file")
	}

	// RHEL / Fedora / Rocky / Alma: needs-restarting -r exit code 1 => reboot recommended.
	if path, ok := lookPath("needs-restarting"); ok {
		code, _, _, runErr := runExitCode(path, "-r")
		if runErr != nil {
			// Non-exit errors are real problems (should be rare since we already
			// looked up the path), but they must not discard an already-confirmed
			// pending=true result.
			errsList = append(errsList, errs.Wrap(runErr, "needs-restarting -r failed"))
		} else if code == 1 {
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
			errsList = append(errsList, errs.Wrap(runErr, "zypper needs-rebooting failed"))
		} else if code == 102 {
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
				errsList = append(errsList, errs.Wrap(runErr, "needs-restarting -r (fallback) failed"))
			} else if code == 1 {
				pending = true
				reasons = append(reasons, "needs-restarting:-r(fallback)")
			}
		}
	}

	// Universal kernel mismatch fallback (RPM-based only):
	// Compare running kernel (uname -r) with latest installed kernel-core package.
	// This is used only if nothing else already indicated reboot, and only flags a
	// reboot when the newest installed kernel is STRICTLY NEWER than the running one.
	if !pending {
		running, e := cmdOutputTrim("uname", "-r")
		if e == nil && running != "" {
			latest, e2 := latestKernelCoreRPM()
			if e2 != nil {
				errsList = append(errsList, errs.Wrap(e2, "latest kernel-core query failed"))
			} else if latest != "" && kernelNewer(running, latest) {
				pending = true
				reasons = append(reasons, fmt.Sprintf("kernel:newer-installed(running=%s, latest=%s)", running, latest))
			}
		}
	}

	if len(errsList) > 0 {
		err = errors.Join(errsList...)
	}
	return pending, reasons, err
}

// kernelNewer reports whether the latest installed kernel string is strictly newer
// than the running kernel string. uname -r and rpm %{VERSION}-%{RELEASE}.%{ARCH}
// are not byte-identical even when they refer to the same kernel (e.g. uname yields
// "5.14.0-503.el9.x86_64" while rpm yields "5.14.0-503.el9.x86_64" only after the
// arch suffix is appended), so a plain string inequality is unsafe. We compare with
// a real version comparison and only return true when latest > running.
func kernelNewer(running, latest string) bool {
	return compareVersions(latest, running) > 0
}

// compareVersions compares two version strings segment by segment.
// It splits each version on non-alphanumeric separators (".", "-", "_", "+") and
// further into runs of digits and runs of non-digits. Numeric runs are compared as
// integers; non-numeric runs are compared lexically. Returns:
//
//	-1 if a < b, 0 if a == b, +1 if a > b.
//
// Missing trailing segments are treated as the empty/zero segment, so "5.14" == "5.14.0".
func compareVersions(a, b string) int {
	ta := tokenizeVersion(a)
	tb := tokenizeVersion(b)

	n := len(ta)
	if len(tb) > n {
		n = len(tb)
	}

	for i := 0; i < n; i++ {
		var sa, sb string
		if i < len(ta) {
			sa = ta[i]
		}
		if i < len(tb) {
			sb = tb[i]
		}
		if sa == sb {
			continue
		}

		aNum, aIsNum := parseUintToken(sa)
		bNum, bIsNum := parseUintToken(sb)

		// Treat a missing (absent) segment as numeric zero so that trailing-zero
		// variants compare equal, e.g. "5.14" == "5.14.0".
		if sa == "" {
			aNum, aIsNum = 0, true
		}
		if sb == "" {
			bNum, bIsNum = 0, true
		}

		switch {
		case aIsNum && bIsNum:
			if aNum != bNum {
				if aNum < bNum {
					return -1
				}
				return 1
			}
		case aIsNum && !bIsNum:
			// Numeric segment outranks an alphabetic/pre-release one
			// (e.g. "1.0" > "1.0a").
			return 1
		case !aIsNum && bIsNum:
			return -1
		default: // both non-numeric, unequal
			if sa < sb {
				return -1
			}
			return 1
		}
	}
	return 0
}

// tokenizeVersion splits a version string into alternating numeric / non-numeric
// tokens, using ".", "-", "_", "+" (and any other non-alphanumeric rune) as
// separators that are dropped.
func tokenizeVersion(v string) []string {
	tokens := make([]string, 0, 8)
	var cur strings.Builder
	curIsDigit := false

	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}

	for _, r := range v {
		isDigit := r >= '0' && r <= '9'
		isAlpha := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')

		switch {
		case !isDigit && !isAlpha:
			// separator
			flush()
			curIsDigit = false
		case cur.Len() == 0:
			cur.WriteRune(r)
			curIsDigit = isDigit
		case isDigit == curIsDigit:
			cur.WriteRune(r)
		default:
			// transition between digit and alpha within a segment
			flush()
			cur.WriteRune(r)
			curIsDigit = isDigit
		}
	}
	flush()
	return tokens
}

// parseUintToken returns the integer value of an all-digit token.
func parseUintToken(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	var n uint64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + uint64(r-'0')
	}
	return n, true
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
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	e := cmd.Run()

	stdout = strings.TrimSpace(outBuf.String())
	stderr = strings.TrimSpace(errBuf.String())

	if ctx.Err() == context.DeadlineExceeded {
		return -1, stdout, stderr, fmt.Errorf("%s timed out after %s", path, commandTimeout)
	}

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
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, p, args...).Output()
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
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, rpmPath, "-q", "--qf", qf, "kernel-core").Output()
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
		ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		sortCmd := exec.CommandContext(ctx, sortPath, "-V")
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

	// In-process fallback: pick the highest version with the same version-aware
	// comparison used elsewhere. A lexical sort.Strings would mis-order kernel
	// versions (e.g. "5.14.0-9" > "5.14.0-10"), silently disabling the
	// kernel-mismatch reboot signal.
	latest := filtered[0]
	for _, v := range filtered[1:] {
		if compareVersions(v, latest) > 0 {
			latest = v
		}
	}
	return latest, nil
}
