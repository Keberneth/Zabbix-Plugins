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
	pluginName        = "WindowsLatestUpdate"
	pluginVersion     = "1.0.0"
	metricStatusJSON  = "wlu.update.status"
	metricInstalled   = "wlu.update.installed"
	powerShellTimeout = 60 * time.Second
	cacheTTL          = 30 * time.Minute
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
	if key != metricStatusJSON && key != metricInstalled {
		return nil, errs.Errorf("unknown item key %q", key)
	}

	month, err := parseMonthParam(params)
	if err != nil {
		return nil, err
	}

	payload, err := p.collect(month)
	if err != nil {
		return nil, err
	}

	switch key {
	case metricStatusJSON:
		return payload, nil
	case metricInstalled:
		return extractInstalled(payload)
	}

	return nil, errs.Errorf("unhandled item key %q", key)
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
