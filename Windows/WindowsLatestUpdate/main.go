package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.zabbix.com/sdk/errs"
	"golang.zabbix.com/sdk/plugin"
	"golang.zabbix.com/sdk/plugin/container"
)

const (
	pluginName           = "WindowsLatestUpdate"
	pluginVersion        = "1.2.0"
	metricStatusJSON     = "wlu.update.status"
	metricInstalled      = "wlu.update.installed"
	metricStatusJSONPrev = "wlu.update.status.previous"
	metricInstalledPrev  = "wlu.update.installed.previous"
	powerShellTimeout    = 60 * time.Second
	cacheTTL             = 30 * time.Minute
)

var (
	_ plugin.Exporter = (*wluPlugin)(nil)

	monthParamRegex = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])$`)

	powerShellScript = `
$ErrorActionPreference = 'Stop'
$WarningPreference = 'SilentlyContinue'
$VerbosePreference = 'SilentlyContinue'
$InformationPreference = 'SilentlyContinue'
$ProgressPreference = 'SilentlyContinue'

$targetMonth = $env:WLU_TARGET_MONTH
if ([string]::IsNullOrWhiteSpace($targetMonth)) {
    $targetMonth = (Get-Date).ToString('yyyy-MM')
}

$result = [ordered]@{
    Timestamp     = (Get-Date).ToString('o')
    LocalNode     = $env:COMPUTERNAME
    MonthChecked  = $targetMonth
    Installed     = 1
    MatchedTitles = @()
    InstalledOn   = $null
    KBs           = @()
    HistoryCount  = 0
    Source        = 'WindowsUpdateCOM'
    ErrorMessage  = $null
}

try {
    $session  = New-Object -ComObject 'Microsoft.Update.Session'
    $searcher = $session.CreateUpdateSearcher()

    $count = 0
    try {
        $count = [int]$searcher.GetTotalHistoryCount()
    } catch {
        throw "GetTotalHistoryCount failed: $($_.Exception.Message)"
    }

    $result.HistoryCount = $count

    $history = @()
    if ($count -gt 0) {
        $history = $searcher.QueryHistory(0, $count)
    }

    $escapedPrefix = [Regex]::Escape($targetMonth)
    $cuPattern = "(?i)$escapedPrefix.*Cumulative Update.*(Windows\s+Server|Microsoft\s+server\s+operating\s+system|Windows\s+1[01])"

    $matches = @($history | Where-Object {
        $_.Title -match $cuPattern -and
        $_.Title -notmatch '(?i)\.NET Framework' -and
        $_.Title -notmatch '(?i)Servicing Stack' -and
        $_.Operation -eq 1 -and
        ($_.ResultCode -eq 2 -or $_.ResultCode -eq 3)
    })

    if ($matches.Count -gt 0) {
        $latest = $matches | Sort-Object Date -Descending | Select-Object -First 1
        $result.Installed     = 0
        $result.MatchedTitles = @($matches | ForEach-Object { [string]$_.Title })
        $result.InstalledOn   = $latest.Date.ToString('o')

        $kbs = New-Object 'System.Collections.Generic.HashSet[string]'
        foreach ($m in $matches) {
            $title = [string]$m.Title
            $kbHits = [Regex]::Matches($title, '(?i)KB\d{6,7}')
            foreach ($k in $kbHits) {
                [void]$kbs.Add($k.Value.ToUpperInvariant())
            }
        }
        $result.KBs = @($kbs)
    }
}
catch {
    $result.ErrorMessage = $_.Exception.Message
    $result.Source       = 'Error'
    $result.Installed    = 1
}

[pscustomobject]$result | ConvertTo-Json -Depth 5 -Compress
`
)

type cachedPayload struct {
	generatedAt time.Time
	payload     string
	month       string
}

type wluPlugin struct {
	plugin.Base
	mu    sync.Mutex
	cache cachedPayload
}

type updateStatus struct {
	Installed *int `json:"Installed"`
}

func main() {
	if exitCode, handled := maybeRunStandalone(os.Args[1:]); handled {
		os.Exit(exitCode)
	}

	if err := run(); err != nil {
		panic(err)
	}
}

func printStandaloneUsage() {
	fmt.Fprintln(os.Stderr, "Windows Latest Update plugin self-test")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  zabbix-agent2-windows-latest-update.exe --standalone [--verbose] [--month yyyy-MM]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Examples:")
	fmt.Fprintln(os.Stderr, "  & \"C:\\Program Files\\Zabbix Agent 2\\zabbix-agent2-windows-latest-update.exe\" --standalone")
	fmt.Fprintln(os.Stderr, "  & \"C:\\Program Files\\Zabbix Agent 2\\zabbix-agent2-windows-latest-update.exe\" --standalone --verbose --month 2026-04")
}

func maybeRunStandalone(args []string) (int, bool) {
	if len(args) == 0 {
		printStandaloneUsage()
		return 2, true
	}

	standalone := false
	verbose := false
	month := ""

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch strings.ToLower(arg) {
		case "--standalone", "-standalone", "--selftest", "-selftest":
			standalone = true
		case "--verbose", "-verbose", "-v":
			verbose = true
		case "--month", "-month":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "missing value for --month")
				printStandaloneUsage()
				return 2, true
			}
			i++
			month = args[i]
		case "--help", "-h", "-?", "/?", "/h", "/help":
			printStandaloneUsage()
			return 0, true
		default:
			if strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "/") {
				fmt.Fprintf(os.Stderr, "unknown argument: %s\n\n", arg)
				printStandaloneUsage()
				return 2, true
			}

			return 0, false
		}
	}

	if !standalone {
		return 0, false
	}

	if month != "" && !monthParamRegex.MatchString(month) {
		fmt.Fprintf(os.Stderr, "invalid --month value %q (expected yyyy-MM)\n", month)
		return 2, true
	}

	payload, err := collectLive(month)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1, true
	}

	payload, err = applyReleaseSuppression(payload, month, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1, true
	}

	if verbose {
		var normalized any
		if err := json.Unmarshal([]byte(payload), &normalized); err == nil {
			pretty, err := json.MarshalIndent(normalized, "", "  ")
			if err == nil {
				payload = string(pretty)
			}
		}
	}

	fmt.Println(payload)
	return 0, true
}

func run() error {
	p := &wluPlugin{}

	err := plugin.RegisterMetrics(
		p,
		pluginName,
		metricStatusJSON,
		"Returns a JSON snapshot describing whether the current month's Windows Cumulative Update is installed.",
		metricInstalled,
		"Returns 0 if the current month's Windows Cumulative Update is installed, 1 otherwise. Optional parameter: yyyy-MM month override.",
		metricStatusJSONPrev,
		"Returns a JSON snapshot describing whether the previous month's Windows Cumulative Update is installed.",
		metricInstalledPrev,
		"Returns 0 if the previous month's Windows Cumulative Update is installed, 1 otherwise. No parameters.",
	)
	if err != nil {
		return errs.Wrap(err, "failed to register metrics")
	}

	h, err := container.NewHandler(pluginName)
	if err != nil {
		return errs.Wrap(err, "failed to create new handler")
	}

	p.Logger = h

	err = h.Execute()
	if err != nil {
		return errs.Wrap(err, "failed to execute plugin handler")
	}

	return nil
}

func (p *wluPlugin) Export(key string, params []string, _ plugin.ContextProvider) (any, error) {
	now := time.Now()

	var month string

	switch key {
	case metricStatusJSON, metricInstalled:
		parsed, err := parseMonthParam(params)
		if err != nil {
			return nil, err
		}
		month = parsed
	case metricStatusJSONPrev, metricInstalledPrev:
		if len(params) > 0 && strings.TrimSpace(params[0]) != "" {
			return nil, errs.Errorf("item key %q does not accept parameters", key)
		}
		month = previousMonth(now)
	default:
		return nil, errs.Errorf("unknown item key %q", key)
	}

	payload, err := p.collect(month)
	if err != nil {
		return nil, err
	}

	payload, err = applyReleaseSuppression(payload, month, now)
	if err != nil {
		return nil, err
	}

	switch key {
	case metricStatusJSON, metricStatusJSONPrev:
		return payload, nil
	case metricInstalled, metricInstalledPrev:
		return extractInstalled(payload)
	}

	return nil, errs.Errorf("unhandled item key %q", key)
}

func previousMonth(now time.Time) string {
	year, month, _ := now.Date()
	prev := time.Date(year, month-1, 1, 0, 0, 0, 0, now.Location())
	return prev.Format("2006-01")
}

// secondTuesday returns midnight at the start of the second Tuesday (Patch
// Tuesday) of the given month, in loc. Microsoft publishes the monthly
// cumulative update on this day, so a host cannot be expected to have it before
// then.
func secondTuesday(year int, month time.Month, loc *time.Location) time.Time {
	first := time.Date(year, month, 1, 0, 0, 0, 0, loc)
	offsetToFirstTuesday := (int(time.Tuesday) - int(first.Weekday()) + 7) % 7
	firstTuesday := first.AddDate(0, 0, offsetToFirstTuesday)
	return firstTuesday.AddDate(0, 0, 7)
}

// applyReleaseSuppression rewrites the collected payload so the current month's
// CU is not reported as missing before its Patch Tuesday (the second Tuesday of
// the month being checked). Microsoft has not released the update yet, so a
// "missing" verdict would be a false positive from the 1st until Patch Tuesday.
//
// The factual detection from the collector is preserved in RawInstalled, and
// the decision is made transparent via PatchTuesday, ReleaseDue and Suppressed.
// When the release is due (now on/after Patch Tuesday) the payload is left
// untouched apart from the added diagnostic fields, so the previous month and
// any explicit past-month checks are never affected.
//
// The month reasoned about is taken from the payload's MonthChecked, which was
// produced by the same clock read that gathered the data. This avoids a skew at
// the month boundary where a Go-side now and the collector's PowerShell clock
// could land in different months. A collector error payload (Source=="Error")
// is never suppressed, so a genuinely broken Windows Update check keeps
// reporting missing instead of being masked as installed.
func applyReleaseSuppression(payload, targetMonth string, now time.Time) (string, error) {
	var data map[string]any
	if err := json.Unmarshal([]byte(payload), &data); err != nil {
		return "", errs.Wrap(err, "failed to parse payload for release suppression")
	}
	if data == nil {
		data = map[string]any{}
	}

	year, month, ok := monthFromString(payloadString(data["MonthChecked"]), now.Location())
	if !ok {
		year, month, ok = monthFromString(targetMonth, now.Location())
	}
	if !ok {
		year, month = now.Year(), now.Month()
	}

	patchTuesday := secondTuesday(year, month, now.Location())
	releaseDue := !now.Before(patchTuesday)

	rawInstalled, ok := payloadInt(data["Installed"])
	if !ok {
		return "", errs.Errorf("payload missing numeric Installed field for release suppression")
	}

	collectorError := payloadString(data["Source"]) == "Error"

	data["RawInstalled"] = rawInstalled
	data["PatchTuesday"] = patchTuesday.Format("2006-01-02")
	data["ReleaseDue"] = releaseDue

	suppressed := false
	if !releaseDue && rawInstalled != 0 && !collectorError {
		data["Installed"] = 0
		suppressed = true
	}
	data["Suppressed"] = suppressed

	normalized, err := json.Marshal(data)
	if err != nil {
		return "", errs.Wrap(err, "failed to marshal payload after release suppression")
	}

	return string(normalized), nil
}

// monthFromString parses a yyyy-MM string into a year and month in loc. The
// bool is false when the string is empty or malformed.
func monthFromString(s string, loc *time.Location) (int, time.Month, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, false
	}

	parsed, err := time.ParseInLocation("2006-01", s, loc)
	if err != nil {
		return 0, 0, false
	}

	return parsed.Year(), parsed.Month(), true
}

// payloadString returns v as a string, or "" if it is not a string.
func payloadString(v any) string {
	s, _ := v.(string)
	return s
}

// payloadInt coerces a JSON-decoded value into an int. Numbers decoded from
// JSON arrive as float64, but int/json.Number are handled for safety.
func payloadInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	default:
		return 0, false
	}
}

func parseMonthParam(params []string) (string, error) {
	if len(params) == 0 {
		return "", nil
	}

	if len(params) > 1 {
		return "", errs.Errorf("too many parameters (expected at most 1: yyyy-MM)")
	}

	value := strings.TrimSpace(params[0])
	if value == "" {
		return "", nil
	}

	if !monthParamRegex.MatchString(value) {
		return "", errs.Errorf("invalid month parameter %q (expected yyyy-MM)", value)
	}

	return value, nil
}

func (p *wluPlugin) collect(month string) (string, error) {
	payload, err := collectLive(month)
	if err == nil {
		p.storeCache(month, payload)
		return payload, nil
	}

	if cached, ok := p.loadCached(month, err); ok {
		return cached, nil
	}

	return "", err
}

func extractInstalled(payload string) (int, error) {
	var status updateStatus
	if err := json.Unmarshal([]byte(payload), &status); err != nil {
		return 0, errs.Wrap(err, "failed to parse update status payload")
	}

	if status.Installed == nil {
		return 0, errs.Errorf("update status payload missing Installed field")
	}

	switch *status.Installed {
	case 0, 1:
		return *status.Installed, nil
	default:
		return 0, errs.Errorf("unexpected Installed value %d (expected 0 or 1)", *status.Installed)
	}
}

func collectLive(month string) (string, error) {
	commandCtx, cancel := context.WithTimeout(context.Background(), powerShellTimeout)
	defer cancel()

	cmd := exec.CommandContext(
		commandCtx,
		resolvePowerShellPath(),
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy",
		"Bypass",
		"-Command",
		"-",
	)
	cmd.Stdin = strings.NewReader(powerShellScript)

	if month != "" {
		cmd.Env = append(os.Environ(), "WLU_TARGET_MONTH="+month)
	}

	output, err := cmd.CombinedOutput()
	if commandCtx.Err() == context.DeadlineExceeded {
		return "", errs.Errorf("powershell collection timed out after %s", powerShellTimeout)
	}

	if err != nil {
		errorText := strings.TrimSpace(string(output))
		if errorText == "" {
			errorText = err.Error()
		}
		return "", errs.Wrap(err, fmt.Sprintf("powershell collection failed: %s", errorText))
	}

	return enrichPayload(output, "live", "", 0)
}

func resolvePowerShellPath() string {
	systemRoot := strings.TrimSpace(os.Getenv("SystemRoot"))
	if systemRoot == "" {
		return "powershell.exe"
	}

	return filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
}

func (p *wluPlugin) storeCache(month, payload string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.cache.generatedAt = time.Now()
	p.cache.payload = payload
	p.cache.month = month
}

func (p *wluPlugin) loadCached(month string, liveErr error) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cache.payload == "" || p.cache.month != month {
		return "", false
	}

	age := time.Since(p.cache.generatedAt)
	if age > cacheTTL {
		return "", false
	}

	payload, err := enrichPayload([]byte(p.cache.payload), "cached", liveErr.Error(), age)
	if err != nil {
		return p.cache.payload, true
	}

	return payload, true
}

func enrichPayload(raw []byte, mode string, collectionErr string, age time.Duration) (string, error) {
	var payload map[string]any

	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", errs.Errorf("empty payload returned by powershell collector")
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", errs.Wrap(err, "failed to parse powershell JSON output")
	}

	if payload == nil {
		payload = map[string]any{}
	}

	payload["CollectorVersion"] = pluginVersion
	payload["CollectionMode"] = mode
	payload["CollectionAgeSeconds"] = int(age.Seconds())

	if collectionErr == "" {
		delete(payload, "CollectionError")
	} else {
		payload["CollectionError"] = collectionErr
	}

	normalized, err := json.Marshal(payload)
	if err != nil {
		return "", errs.Wrap(err, "failed to marshal normalized payload")
	}

	return string(normalized), nil
}
