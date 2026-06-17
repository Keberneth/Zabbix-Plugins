//go:build windows
// +build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.zabbix.com/sdk/errs"
)

// EventGroup is one recurring event signature aggregated over the lookback
// window. The JSON field names are the contract the Zabbix template's LLD macro
// paths ($.EventID, $.Level, $.Log, $.Source) and JavaScript preprocessing
// depend on — do not rename them without updating the template.
type EventGroup struct {
	Log       string `json:"Log"`
	Level     string `json:"Level"`
	EventID   int    `json:"EventID"`
	Source    string `json:"Source"`
	Count     int    `json:"Count"`
	FirstSeen string `json:"FirstSeen,omitempty"`
	LastSeen  string `json:"LastSeen,omitempty"`
	Message   string `json:"Message,omitempty"`
}

const (
	// Hard caps applied in Go regardless of item parameters, to keep the item
	// value well under Zabbix's value-size limit and to bound LLD growth. The
	// JSON is never byte-truncated (which would corrupt it); instead the number
	// of groups is capped and individual messages are rune-trimmed.
	maxGroups       = 250
	maxMessageRunes = 512
)

// powerShellScript collects Critical/Error events, groups them by
// (Log, Source, EventID, Level), counts the occurrences in the window and emits
// JSON on stdout. Every tunable is read from an environment variable — never
// string-interpolated into the script — so item parameters cannot inject
// PowerShell. The script targets Windows PowerShell 5.1.
//
// It deliberately ends with a single trailing expression (like the other
// PowerShell-backed plugins in this repo): a multi-line if/elseif/else as the
// final statement is not executed reliably when the script is fed to
// "powershell -Command -" over stdin. Empty / single-object / array output is
// all normalised on the Go side by parseGroups.
const powerShellScript = `
$ErrorActionPreference = 'Stop'
$WarningPreference = 'SilentlyContinue'
$VerbosePreference = 'SilentlyContinue'
$InformationPreference = 'SilentlyContinue'
$ProgressPreference = 'SilentlyContinue'

$logs = @()
foreach ($l in ($env:ZBX_WINEVENT_LOGS -split ',')) {
    $t = $l.Trim()
    if ($t) { $logs += $t }
}
if ($logs.Count -eq 0) { $logs = @('System','Application') }

# Lookback is passed as an integer number of minutes. An integer has no decimal
# separator, so [long]::TryParse is locale-independent (a fractional-hours value
# parsed via the current culture would silently fail on comma-decimal locales).
$minutes = 1440
[long]::TryParse($env:ZBX_WINEVENT_LOOKBACK_MINUTES, [ref]$minutes) | Out-Null
if ($minutes -le 0) { $minutes = 1440 }

$max = 2000
[int]::TryParse($env:ZBX_WINEVENT_MAXEVENTS, [ref]$max) | Out-Null
if ($max -le 0) { $max = 2000 }

$start = (Get-Date).AddMinutes(-1 * $minutes)
$levelMap = @{ 1 = 'Critical'; 2 = 'Error' }

$result = New-Object System.Collections.Generic.List[object]

foreach ($log in $logs) {
    $events = $null
    try {
        $events = Get-WinEvent -FilterHashtable @{ LogName = $log; StartTime = $start; Level = @(1,2) } -MaxEvents $max -ErrorAction Stop
    } catch {
        # "No events were found that match the specified selection criteria" is
        # raised as a terminating error, as is an inaccessible log; skip it but
        # report the reason on stderr (kept separate from the JSON on stdout).
        [Console]::Error.WriteLine("winevent: skipped log '$log': $($_.Exception.Message)")
        continue
    }
    if (-not $events) { continue }

    $groups = $events | Group-Object -Property Id, ProviderName, Level
    foreach ($g in $groups) {
        $evt = $g.Group[0]
        $times = @($g.Group | ForEach-Object { $_.TimeCreated } | Sort-Object)
        $first = $times[0]
        $last  = $times[$times.Count - 1]

        $lvl = $levelMap[[int]$evt.Level]
        if (-not $lvl) { $lvl = "Level$([int]$evt.Level)" }

        $msg = ''
        if ($evt.Message) { $msg = [string]$evt.Message }

        $result.Add([ordered]@{
            Log       = [string]$log
            Level     = $lvl
            EventID   = [int]$evt.Id
            Source    = [string]$evt.ProviderName
            Count     = [int]$g.Count
            FirstSeen = $first.ToUniversalTime().ToString('o')
            LastSeen  = $last.ToUniversalTime().ToString('o')
            Message   = $msg
        })
    }
}

$result | ConvertTo-Json -Depth 4 -Compress
`

// ignoreRule drops an event signature by Source and/or EventID. An empty field
// is a wildcard.
type ignoreRule struct {
	source string // lower-cased; "" matches any source
	id     string // decimal EventID string; "" matches any id
}

func (r ignoreRule) matches(g EventGroup) bool {
	if r.source != "" && r.source != strings.ToLower(g.Source) {
		return false
	}
	if r.id != "" && r.id != strconv.Itoa(g.EventID) {
		return false
	}
	return true
}

// parseIgnoreRules parses the ignore specification (the {$WINEVENT.IGNORE}
// macro): rules separated by ';' or newlines, each "Source:EventID" where either
// side may be '*' (wildcard). A bare number is an EventID, a bare name a Source.
// Parsing in Go avoids interpolating a user-edited value into JavaScript.
func parseIgnoreRules(spec string) []ignoreRule {
	var rules []ignoreRule
	for _, part := range strings.FieldsFunc(spec, func(r rune) bool { return r == ';' || r == '\n' }) {
		p := strings.TrimSpace(part)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}

		var source, id string
		if i := strings.LastIndex(p, ":"); i >= 0 {
			source = strings.TrimSpace(p[:i])
			id = strings.TrimSpace(p[i+1:])
		} else if isAllDigits(p) {
			id = p
		} else {
			source = p
		}

		if source == "*" {
			source = ""
		}
		if id == "*" {
			id = ""
		}
		if source == "" && id == "" {
			continue
		}
		rules = append(rules, ignoreRule{source: strings.ToLower(source), id: id})
	}
	return rules
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isIgnored(g EventGroup, rules []ignoreRule) bool {
	for _, r := range rules {
		if r.matches(g) {
			return true
		}
	}
	return false
}

// FindRecurringEvents runs the collector and returns groups that are not ignored
// and whose Count is at least minCount, sorted by Count descending, capped to
// maxGroups and with each message rune-trimmed. The returned slice is never nil.
func FindRecurringEvents(ctx context.Context, logs []string, lookback time.Duration, maxEvents, minCount int, ignoreSpec string) ([]EventGroup, error) {
	out, err := runCollector(ctx, logs, lookback, maxEvents)
	if err != nil {
		return nil, err
	}

	groups, err := parseGroups(out)
	if err != nil {
		return nil, err
	}

	if minCount < 1 {
		minCount = 1
	}
	rules := parseIgnoreRules(ignoreSpec)

	filtered := make([]EventGroup, 0, len(groups))
	for _, g := range groups {
		if g.Count < minCount || isIgnored(g, rules) {
			continue
		}
		g.Message = trimRunes(g.Message, maxMessageRunes)
		filtered = append(filtered, g)
	}

	sort.SliceStable(filtered, func(i, j int) bool {
		return filtered[i].Count > filtered[j].Count
	})

	if len(filtered) > maxGroups {
		filtered = filtered[:maxGroups]
	}

	return filtered, nil
}

func runCollector(ctx context.Context, logs []string, lookback time.Duration, maxEvents int) ([]byte, error) {
	cmd := exec.CommandContext(
		ctx,
		resolvePowerShellPath(),
		"-NoLogo", "-NoProfile", "-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-Command", "-",
	)
	cmd.Stdin = strings.NewReader(powerShellScript)
	cmd.Env = append(os.Environ(),
		"ZBX_WINEVENT_LOGS="+strings.Join(logs, ","),
		fmt.Sprintf("ZBX_WINEVENT_LOOKBACK_MINUTES=%d", int64(lookback.Minutes())),
		fmt.Sprintf("ZBX_WINEVENT_MAXEVENTS=%d", maxEvents),
	)

	// Keep stdout (JSON) and stderr (diagnostics) separate so a skipped-log
	// warning from the collector never corrupts the JSON payload.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, errs.Errorf("event log collection timed out")
	}
	if err != nil {
		errorText := strings.TrimSpace(stderr.String())
		if errorText == "" {
			errorText = err.Error()
		}
		return nil, errs.Wrap(err, "event log collection failed: "+errorText)
	}
	return stdout.Bytes(), nil
}

// parseGroups normalises the collector output, which is empty when no events
// were found, a single JSON object for exactly one signature, or a JSON array
// for many (PowerShell's ConvertTo-Json unwraps single-element arrays).
func parseGroups(raw []byte) ([]EventGroup, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return []EventGroup{}, nil
	}

	var groups []EventGroup
	if err := json.Unmarshal(raw, &groups); err != nil {
		var single EventGroup
		if err2 := json.Unmarshal(raw, &single); err2 != nil {
			return nil, errs.Wrap(err, "failed to parse collector JSON output")
		}
		groups = []EventGroup{single}
	}
	if groups == nil {
		groups = []EventGroup{}
	}
	return groups, nil
}

func resolvePowerShellPath() string {
	systemRoot := strings.TrimSpace(os.Getenv("SystemRoot"))
	if systemRoot == "" {
		return "powershell.exe"
	}
	return filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
}

func trimRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}
