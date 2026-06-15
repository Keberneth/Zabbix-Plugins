//go:build linux
// +build linux

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.zabbix.com/sdk/errs"
	"golang.zabbix.com/sdk/plugin"
	"golang.zabbix.com/sdk/plugin/container"
)

const (
	metricKey            = "btg.account.check.log"
	defaultDedupSeconds  = 300
	maxLineBytes         = 1 << 20 // 1 MiB
	defaultCheckpointDir = "/var/lib/zabbix-agent2/BTGAccountCheck"
)

var impl BTGPlugin

type BTGPlugin struct {
	plugin.Base

	// deduper related
	ded     *deduper
	dedOnce sync.Once

	// checkpoint dir permission check caching (keyed by checkpointDir)
	checkpointCacheMu sync.Mutex
	checkpointOK      map[string]error // nil error == OK, non-nil == last observed error
}

type checkpoint struct {
	Path   string `json:"path"`
	FileID string `json:"file_id"`
	Offset int64  `json:"offset"`
}

// in-memory deduper
type deduper struct {
	mu   sync.Mutex
	last map[string]int64
	ttl  int64
}

func newDeduper(ttl int64) *deduper {
	return &deduper{last: make(map[string]int64), ttl: ttl}
}

func (d *deduper) shouldSend(key string) bool {
	now := time.Now().Unix()
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.last[key]; ok && now-t <= d.ttl {
		return false
	}
	d.last[key] = now
	return true
}

func (d *deduper) startJanitor(ctx context.Context) {
	if d.ttl <= 0 {
		return
	}
	pruneAfter := d.ttl * 2
	ticker := time.NewTicker(time.Duration(d.ttl) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().Unix()
			d.mu.Lock()
			for k, ts := range d.last {
				if now-ts > pruneAfter {
					delete(d.last, k)
				}
			}
			d.mu.Unlock()
		}
	}
}

// actor-focused matchers
var (
	actorRegexes    []*regexp.Regexp
	negativeRegexes []*regexp.Regexp

	// compiled once: extract the pam_unix(...) token
	pamProcRe = regexp.MustCompile(`pam_unix\(([^)]+)\):`)
)

func init() {
	plugin.RegisterMetrics(&impl, "BTGAccountCheck", metricKey, "Report detections of BTG accounts in a secure log file. Params: <logfile> <comma-separated-accounts>.")
	impl.checkpointOK = make(map[string]error)

	// actor-centric regexes (group 1 should be the actor/initiator when present)
	actorRegexes = []*regexp.Regexp{
		// Treat successful SSH auth as actor=subject (captures authenticated user)
		// e.g. "Accepted password for iver from ..."
		regexp.MustCompile(`(?i)Accepted (?:password|publickey|keyboard-interactive) for\s+([^\s(]+)`),

		// pam_unix session opened for user <target> ... by <actor>
		// capture actor in group 1 when `by <actor>` is present
		regexp.MustCompile(`(?i)pam_unix\([^)]+\):\s*session opened for user\s+[^\s(]+(?:.*\bby\s+([^\s(]+))`),

		// LOGIN ... BY <actor>
		regexp.MustCompile(`(?i)\bLOGIN\b.*\bBY\b\s*([^\s(]+)`),

		// sudo/su summary lines where actor appears before ":" followed by "TTY="
		// e.g. "sudo[pid]:    iver : TTY=pts/2 ; PWD=..."
		regexp.MustCompile(`(?i)\b([^\s:]+)\s*:\s*TTY=`),

		// generic pam_unix "session opened ... by <actor>" variant
		regexp.MustCompile(`(?i)session (?:opened|opened for|opened for user).*?\bby\s+([^\s(]+)`),
	}

	// negative patterns: ignore session closed, logout, removed session, disconnects, etc.
	negativeRegexes = []*regexp.Regexp{
		regexp.MustCompile(`(?i)session closed`),
		regexp.MustCompile(`(?i)close session`),
		regexp.MustCompile(`(?i)pam_unix\([^)]+\):\s*session closed for user`),
		regexp.MustCompile(`(?i)\blogged out\b`),
		regexp.MustCompile(`(?i)\bremoved session\b`),
		regexp.MustCompile(`(?i)\blogout\b`),
		regexp.MustCompile(`(?i)\bdisconnect(?:ed)?\b`),
	}
}

func getContextFromProvider(cp plugin.ContextProvider) context.Context {
	if cp == nil {
		return context.Background()
	}
	if cprov, ok := cp.(interface{ Context() context.Context }); ok {
		if c := cprov.Context(); c != nil {
			return c
		}
	}
	return context.Background()
}

func (p *BTGPlugin) Export(key string, params []string, ctx plugin.ContextProvider) (interface{}, error) {
	if key != metricKey {
		return nil, errs.Errorf("unknown key %q", key)
	}

	// Expect at least 2 params: logfile, accounts
	if len(params) < 2 {
		return fmt.Sprintf("usage: %s <logfile> <account1[,account2,...]>", metricKey), nil
	}

	path := params[0]
	accounts := splitAndTrim(params[1], ",")

	// Options from env:
	checkpointDir := os.Getenv("CHECKPOINT_DIR")
	if checkpointDir == "" {
		checkpointDir = defaultCheckpointDir
	}

	// --- Fail fast: ensure checkpoint dir exists and is writable (cached per-dir) ---
	if err := p.checkCheckpointDir(checkpointDir); err != nil {
		log.Printf("ERROR: %v", err)
		return fmt.Sprintf("ERROR: %v", err), nil
	}
	// --- end cached permission checks ---

	dedupSec := defaultDedupSeconds
	if v := os.Getenv("ALERT_DEDUP_SECONDS"); v != "" {
		if i, err := strconv.Atoi(v); err == nil && i > 0 {
			dedupSec = i
		}
	}

	// initialize in-memory deduper once per plugin instance, use plugin context for janitor
	p.dedOnce.Do(func() {
		p.ded = newDeduper(int64(dedupSec))
		go p.ded.startJanitor(getContextFromProvider(ctx))
	})

	found, messages, err := p.processOnce(path, accounts, checkpointDir)
	if err != nil {
		log.Printf("ERROR: %v", err)
		return "ERROR: " + err.Error(), nil
	}
	if !found {
		return "OK: no BTG accounts detected", nil
	}
	return strings.Join(messages, "\n"), nil
}

// checkCheckpointDir ensures the checkpointDir exists and is writable, caches result per-dir
func (p *BTGPlugin) checkCheckpointDir(checkpointDir string) error {
	p.checkpointCacheMu.Lock()
	err, cached := p.checkpointOK[checkpointDir]
	p.checkpointCacheMu.Unlock()
	if cached {
		return err
	}

	// perform check
	var finalErr error
	if err := os.MkdirAll(checkpointDir, 0o750); err != nil {
		finalErr = fmt.Errorf("cannot create checkpoint dir %s: %v", checkpointDir, err)
		p.cacheCheckpointDirResult(checkpointDir, finalErr)
		return finalErr
	}
	permCheck := filepath.Join(checkpointDir, ".permcheck")
	if werr := os.WriteFile(permCheck, []byte("ok"), 0o600); werr != nil {
		finalErr = fmt.Errorf("checkpoint dir %s not writable: %v", checkpointDir, werr)
		p.cacheCheckpointDirResult(checkpointDir, finalErr)
		return finalErr
	}
	_ = os.Remove(permCheck)
	p.cacheCheckpointDirResult(checkpointDir, nil)
	return nil
}

func (p *BTGPlugin) cacheCheckpointDirResult(checkpointDir string, err error) {
	p.checkpointCacheMu.Lock()
	defer p.checkpointCacheMu.Unlock()
	p.checkpointOK[checkpointDir] = err
}

//
// processOnce no longer accepts dedupTTL (deduper is part of the plugin)
//
func (p *BTGPlugin) processOnce(path string, accounts []string, checkpointDir string) (bool, []string, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil, fmt.Errorf("log file %s does not exist", path)
		}
		return false, nil, fmt.Errorf("stat failed for %s: %v", path, err)
	}

	// Normalize accounts once (lowercase + trim) to avoid repeated allocations
	var normAccounts []string
	for _, a := range accounts {
		na := strings.ToLower(strings.TrimSpace(a))
		if na != "" {
			normAccounts = append(normAccounts, na)
		}
	}

	fileID := computeFileID(info)
	off, fid, found := loadCheckpointForPath(checkpointDir, path)
	var offset int64
	if !found {
		// first time: if file small start at 0, otherwise tail to end
		if info.Size() < 1024 {
			offset = 0
		} else {
			offset = info.Size()
		}
	} else {
		if fid == fileID {
			// handle copytruncate (file truncated but same inode): if stored offset > current size, reset to 0
			if off > info.Size() {
				offset = 0
			} else {
				offset = off
			}
		} else {
			// rotated/changed: start from 0
			offset = 0
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return false, nil, fmt.Errorf("open failed: %v", err)
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false, nil, fmt.Errorf("seek failed: %v", err)
	}

	r := bufio.NewReader(f)

	// Track the consumed byte position with an accumulator. bufio reads ahead,
	// so f.Seek(0, io.SeekCurrent) would report a position past what we have
	// actually consumed and corrupt the checkpoint; summing len(line) is exact.
	pos := offset

	var messages []string
	for {
		line, err := r.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return false, nil, fmt.Errorf("read failed: %v", err)
		}

		pos += int64(len(line))

		// if line too big skip (pos has already advanced past it)
		if len(line) > maxLineBytes {
			msg := fmt.Sprintf("oversized line (> %d bytes) in %s (file_id=%s) at offset %d — line skipped", maxLineBytes, path, fileID, pos)
			alertKey := "LONG_LINE|" + path
			if p.ded.shouldSend(alertKey) {
				messages = append(messages, msg)
			}
			if err == io.EOF {
				break
			}
			continue
		}

		if len(line) > 0 {
			lineStr := strings.TrimRight(string(line), "\r\n")
			if ok, account := matchActorInText(lineStr, normAccounts); ok {
				key := account + "|" + path
				if p.ded.shouldSend(key) {
					msg := fmt.Sprintf("btg account '%s' seen in %s: %s", account, path, lineStr)
					messages = append(messages, msg)
				}
			}
		}

		if err == io.EOF {
			break
		}
	}
	offset = pos

	// persist checkpoint (with file locking to avoid races)
	if err := saveCheckpointForPath(checkpointDir, path, fileID, offset); err != nil {
		// not fatal, include warning in returned error
		return len(messages) > 0, messages, fmt.Errorf("failed to save checkpoint: %v", err)
	}

	return len(messages) > 0, messages, nil
}

// computeFileID same as your original helper
func computeFileID(fi fs.FileInfo) string {
	if stat, ok := fi.Sys().(*syscall.Stat_t); ok {
		if stat.Ino != 0 {
			return fmt.Sprintf("ino-%d", stat.Ino)
		}
	}
	return fmt.Sprintf("s%d_m%d", fi.Size(), fi.ModTime().Unix())
}

// checkpoint helpers (with unix.Flock)
func checkpointFilename(checkpointDir, path string) string {
	h := sha256.Sum256([]byte(path))
	return filepath.Join(checkpointDir, hex.EncodeToString(h[:])+".json")
}

func saveCheckpointForPath(checkpointDir, path, fileID string, offset int64) error {
	if checkpointDir == "" {
		return nil
	}
	cp := checkpoint{Path: path, FileID: fileID, Offset: offset}
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}

	fn := checkpointFilename(checkpointDir, path)
	tmp := fn + ".tmp"

	// write tmp file first
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}

	// open (or create) the destination file and acquire exclusive lock before rename
	fd, err := os.OpenFile(fn, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("open checkpoint file for lock failed: %v", err)
	}
	defer func() {
		_ = unix.Flock(int(fd.Fd()), unix.LOCK_UN)
		_ = fd.Close()
	}()

	if err := unix.Flock(int(fd.Fd()), unix.LOCK_EX); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("flock exclusive failed: %v", err)
	}

	// rename tmp -> final
	if err := os.Rename(tmp, fn); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename checkpoint tmp file failed: %v", err)
	}

	// unlock and close in defer
	return nil
}

func loadCheckpointForPath(checkpointDir, path string) (offset int64, fileID string, found bool) {
	if checkpointDir == "" {
		return 0, "", false
	}
	fn := checkpointFilename(checkpointDir, path)

	// open file and acquire shared lock
	fd, err := os.OpenFile(fn, os.O_RDONLY, 0o600)
	if err != nil {
		return 0, "", false
	}
	defer func() {
		_ = unix.Flock(int(fd.Fd()), unix.LOCK_UN)
		_ = fd.Close()
	}()

	if err := unix.Flock(int(fd.Fd()), unix.LOCK_SH); err != nil {
		return 0, "", false
	}

	b, err := io.ReadAll(fd)
	if err != nil {
		return 0, "", false
	}
	var cp checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return 0, "", false
	}
	return cp.Offset, cp.FileID, true
}

// matchActorInText: actor-focused matching.
// - Ignore lines matching negativeRegexes.
// - For each actorRegex, if it captures an actor in group 1, compare actor to monitored accounts.
// - Suppress matches that are clearly post-su/sudo/systemd-user child-session lines where the child reports itself as the actor
//   (examples: pam_unix(su-l:session): session opened for user root(...) by root(...)).
func matchActorInText(line string, accounts []string) (bool, string) {
	// negative checks first
	for _, nr := range negativeRegexes {
		if nr.MatchString(line) {
			return false, ""
		}
	}

	lower := strings.ToLower(line)

	// extract pam_unix(...) token if present to determine the pam service
	proc := ""
	if m := pamProcRe.FindStringSubmatch(lower); m != nil && len(m) >= 2 {
		proc = strings.TrimSpace(m[1]) // e.g. "su-l:session", "sshd:session", "systemd-user:session"
	}

	// consider these pam services to be "child session" services that often report the
	// child process as the actor after a successful sudo/su transition; we suppress
	// actor==target matches for those to avoid false positives.
	childPrefixes := []string{
		"su",          // su, su-l, su:session
		"sudo",        // sudo:session
		"systemd-user", // systemd-user:session
	}

	isPamChild := false
	if proc != "" {
		for _, p := range childPrefixes {
			if strings.HasPrefix(proc, p) {
				isPamChild = true
				break
			}
		}
	}

	// try actor-focused patterns
	for _, re := range actorRegexes {
		if m := re.FindStringSubmatch(line); len(m) >= 2 {
			actor := strings.ToLower(strings.TrimSpace(m[1]))
			if actor == "" {
				continue
			}
			for _, a := range accounts {
				if actor == a {
					// If this line is from a pam child session service (su/sudo/systemd-user)
					// it's typically a follow-up logged by the child after privilege change.
					// Suppress actor==target matches in that case to avoid alerts for
					// post-su lines like "pam_unix(su-l:session): session opened for user root(...) by root(...)"
					// while still alerting on direct authentications (Accepted ... for root, LOGIN BY root).
					if isPamChild {
						return false, ""
					}
					return true, a
				}
			}
		}
	}

	return false, ""
}

func splitAndTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func main() {
	// standalone test mode: detect the flag in any position and pass the
	// remaining (non-flag) args (logfile, accounts) through to Export.
	standalone := false
	var positional []string
	for _, a := range os.Args[1:] {
		switch a {
		case "standalone", "--standalone", "-standalone":
			standalone = true
		default:
			positional = append(positional, a)
		}
	}
	if standalone {
		res, err := impl.Export(metricKey, positional, nil)
		if err != nil {
			fmt.Printf("ERROR: %v\n", err)
			os.Exit(1)
		}
		// res is an interface{} (string) returned by Export
		fmt.Println(res)
		return
	}

	// Agent plugin mode
	h, err := container.NewHandler("BTGAccountCheck")
	if err != nil {
		log.Panic(err)
	}

	impl.Logger = h

	if err := h.Execute(); err != nil {
		log.Panic(err)
	}
}