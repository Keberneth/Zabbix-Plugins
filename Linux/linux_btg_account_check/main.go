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
	maxLineBytes         = 1 << 20  // 1 MiB: longest single line we buffer in memory
	maxScanBytes         = 64 << 20 // 64 MiB: max bytes read per poll (resume next poll)
	defaultCheckpointDir = "/var/lib/zabbix-agent2/BTGAccountCheck"
)

var impl BTGPlugin

type BTGPlugin struct {
	plugin.Base

	// deduper related
	ded     *deduper
	dedOnce sync.Once

	// The checkpoint is a read/scan/write transaction. Serialize it so two
	// concurrent item requests cannot both load the same offset and then let the
	// slower request overwrite a newer checkpoint.
	processMu sync.Mutex

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
	mu        sync.Mutex
	last      map[string]int64
	ttl       int64
	lastPrune int64
}

func newDeduper(ttl int64) *deduper {
	return &deduper{last: make(map[string]int64), ttl: ttl}
}

func (d *deduper) shouldSend(key string) bool {
	now := time.Now().Unix()
	d.mu.Lock()
	defer d.mu.Unlock()

	// Prune opportunistically during normal item execution. This avoids a
	// background goroutine whose lifetime cannot be tied to ContextProvider:
	// that interface exposes a timeout, not a long-lived cancellation context.
	if d.ttl > 0 && (d.lastPrune == 0 || now-d.lastPrune >= d.ttl) {
		pruneAfter := d.ttl * 2
		for k, ts := range d.last {
			if now-ts > pruneAfter {
				delete(d.last, k)
			}
		}
		d.lastPrune = now
	}

	if t, ok := d.last[key]; ok && now-t <= d.ttl {
		return false
	}
	d.last[key] = now
	return true
}

// actor-focused matchers
var (
	actorRegexes    []*regexp.Regexp
	negativeRegexes []*regexp.Regexp

	// compiled once: extract the pam_unix(...) token
	pamProcRe = regexp.MustCompile(`pam_unix\(([^)]+)\):`)
)

func init() {
	if err := plugin.RegisterMetrics(&impl, "BTGAccountCheck", metricKey, "Report detections of BTG accounts in a secure log file. Params: <logfile> <comma-separated-accounts>."); err != nil {
		panic(fmt.Errorf("register BTG metrics: %w", err))
	}
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

func contextFromProvider(cp plugin.ContextProvider) (context.Context, context.CancelFunc) {
	if cp == nil || cp.Timeout() <= 0 {
		return context.Background(), func() {}
	}

	return context.WithTimeout(context.Background(), time.Duration(cp.Timeout())*time.Second)
}

func (p *BTGPlugin) Export(key string, params []string, ctx plugin.ContextProvider) (interface{}, error) {
	if key != metricKey {
		return nil, errs.Errorf("unknown key %q", key)
	}

	// This item intentionally returns operational failures as strings because the
	// supplied template triggers on the "ERROR:" prefix.
	if len(params) != 2 {
		return fmt.Sprintf("ERROR: %s expects exactly 2 parameters (logfile and comma-separated accounts); got %d", metricKey, len(params)), nil
	}

	path := strings.TrimSpace(params[0])
	accounts := splitAndTrim(params[1], ",")
	if path == "" {
		return "ERROR: logfile parameter must not be empty", nil
	}
	if len(accounts) == 0 {
		return "ERROR: accounts parameter must contain at least one account", nil
	}

	itemCtx, cancel := contextFromProvider(ctx)
	defer cancel()

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

	// Initialize the in-memory deduper once per plugin instance. Expired entries
	// are pruned opportunistically by shouldSend.
	p.dedOnce.Do(func() {
		p.ded = newDeduper(int64(dedupSec))
	})

	found, messages, err := p.processOnce(itemCtx, path, accounts, checkpointDir)
	if err != nil {
		log.Printf("ERROR: %v", err)
		// A non-fatal error (e.g. a checkpoint-save failure) must never discard a
		// real break-glass detection — reporting the accounts is the entire point
		// of this plugin. Surface the detections when we have them; only fall back
		// to ERROR when there is nothing to report.
		if found && len(messages) > 0 {
			return strings.Join(messages, "\n"), nil
		}
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
	// Only a cached success short-circuits. A transient failure (dir briefly
	// unavailable, disk full clearing) must be re-checked on the next poll rather
	// than pinning the plugin in ERROR until the agent is restarted.
	if cached && err == nil {
		return nil
	}

	// perform check
	if err := os.MkdirAll(checkpointDir, 0o750); err != nil {
		return fmt.Errorf("cannot create checkpoint dir %s: %v", checkpointDir, err)
	}
	permCheck := filepath.Join(checkpointDir, ".permcheck")
	if werr := os.WriteFile(permCheck, []byte("ok"), 0o600); werr != nil {
		return fmt.Errorf("checkpoint dir %s not writable: %v", checkpointDir, werr)
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

// processOnce no longer accepts dedupTTL (deduper is part of the plugin)
func (p *BTGPlugin) processOnce(ctx context.Context, path string, accounts []string, checkpointDir string) (bool, []string, error) {
	p.processMu.Lock()
	defer p.processMu.Unlock()

	if err := ctx.Err(); err != nil {
		return false, nil, fmt.Errorf("item work canceled before scan: %w", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil, fmt.Errorf("log file %s does not exist", path)
		}
		return false, nil, fmt.Errorf("stat failed for %s: %v", path, err)
	}

	// The log path arrives as an item-key parameter, i.e. from the Zabbix server.
	// Refuse anything that is not a regular file so a malicious/compromised server
	// cannot aim the root agent at a character device (/dev/zero — endless read),
	// a FIFO (blocks forever), or a socket. Symlinks are resolved by os.Stat, so a
	// symlink to such a target is rejected too.
	if !info.Mode().IsRegular() {
		return false, nil, fmt.Errorf("refusing to read non-regular file %s (mode %s)", path, info.Mode())
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
	// actually consumed and corrupt the checkpoint; summing consumed bytes is exact.
	pos := offset

	var messages []string
	var scanned int64
	var workErr error
	for {
		if err := ctx.Err(); err != nil {
			workErr = fmt.Errorf("item work canceled while scanning: %w", err)
			break
		}

		line, truncated, consumed, rerr := readBoundedLine(r, maxLineBytes)
		if rerr != nil && rerr != io.EOF {
			return false, nil, fmt.Errorf("read failed: %v", rerr)
		}

		pos += consumed
		scanned += consumed

		if truncated {
			// Oversized line: only the first maxLineBytes were buffered; the rest
			// was drained without being held in memory. pos has advanced past it.
			msg := fmt.Sprintf("oversized line (> %d bytes) in %s (file_id=%s) at offset %d — line skipped", maxLineBytes, path, fileID, pos)
			alertKey := "LONG_LINE|" + path
			if p.ded.shouldSend(alertKey) {
				messages = append(messages, msg)
			}
		} else if len(line) > 0 {
			lineStr := strings.TrimRight(string(line), "\r\n")
			if ok, account := matchActorInText(lineStr, normAccounts); ok {
				key := account + "|" + path
				if p.ded.shouldSend(key) {
					msg := fmt.Sprintf("btg account '%s' seen in %s: %s", account, path, lineStr)
					messages = append(messages, msg)
				}
			}
		}

		// A regular-file read cannot be interrupted midway, so check again after
		// each complete line. We still save the exact consumed offset below.
		if err := ctx.Err(); err != nil {
			workErr = fmt.Errorf("item work canceled while scanning: %w", err)
			break
		}

		if rerr == io.EOF {
			break
		}
		// Bound the work done in a single poll so a huge backlog cannot pin the
		// agent; the checkpoint below records how far we got and the next poll
		// resumes from there.
		if scanned >= maxScanBytes {
			break
		}
	}
	offset = pos

	// persist checkpoint (with file locking to avoid races)
	if err := saveCheckpointForPath(checkpointDir, path, fileID, offset); err != nil {
		// not fatal, include warning in returned error
		return len(messages) > 0, messages, fmt.Errorf("failed to save checkpoint: %v", err)
	}
	if workErr != nil {
		return len(messages) > 0, messages, workErr
	}

	return len(messages) > 0, messages, nil
}

// readBoundedLine reads a single '\n'-terminated line from r while never buffering
// more than max bytes in memory. If the line is longer than max, the returned data
// is capped at max, truncated is true, and the remainder of the line is discarded
// byte-by-byte without being retained (bounding memory). consumed is the total
// number of bytes read for this line, including any discarded remainder and the
// trailing '\n', so callers can keep an exact byte offset for checkpointing. On the
// final unterminated line it returns whatever was read together with io.EOF.
func readBoundedLine(r *bufio.Reader, max int) (line []byte, truncated bool, consumed int64, err error) {
	for {
		b, e := r.ReadByte()
		if e != nil {
			return line, truncated, consumed, e
		}
		consumed++
		if b == '\n' {
			return line, truncated, consumed, nil
		}
		if len(line) < max {
			line = append(line, b)
		} else {
			truncated = true
		}
	}
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

	// Write to a per-call unique temp file. A fixed "<fn>.tmp" name would be shared
	// by concurrent saves for the same path (the SDK may invoke Export in parallel),
	// letting two writers clobber each other's temp file before the rename.
	tmpf, err := os.CreateTemp(checkpointDir, filepath.Base(fn)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := tmpf.Name()
	if _, err := tmpf.Write(data); err != nil {
		_ = tmpf.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := tmpf.Close(); err != nil {
		_ = os.Remove(tmp)
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
//   - Ignore lines matching negativeRegexes.
//   - For each actorRegex, if it captures an actor in group 1, compare actor to monitored accounts.
//   - Suppress matches that are clearly post-su/sudo/systemd-user child-session lines where the child reports itself as the actor
//     (examples: pam_unix(su-l:session): session opened for user root(...) by root(...)).
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
		"su",           // su, su-l, su:session
		"sudo",         // sudo:session
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
