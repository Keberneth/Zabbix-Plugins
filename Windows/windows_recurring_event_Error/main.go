//go:build windows
// +build windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.zabbix.com/sdk/errs"
	"golang.zabbix.com/sdk/plugin"
	"golang.zabbix.com/sdk/plugin/container"
)

const (
	pluginName = "WinEventRecurring"

	metricKey   = "winevent.recurring"
	metricDescr = "Returns recurring Critical/Error Windows event-log signatures as a JSON array " +
		"(winevent.recurring[logs,period,maxEvents,minCount])."

	defaultLogs      = "System,Application"
	defaultPeriod    = 24 * time.Hour
	defaultMaxEvents = 2000
	defaultMinCount  = 2

	defaultTimeout = 25 * time.Second
	timeoutMargin  = time.Second
	minimumTimeout = 100 * time.Millisecond
	cacheTTL       = 30 * time.Minute
)

// compile-time check
var _ plugin.Exporter = (*winEventRecurringPlugin)(nil)

// validLogName guards the log names that flow (via environment, not string
// interpolation) into the PowerShell collector. Windows channel names look like
// "System", "Application", or "Microsoft-Windows-WindowsUpdateClient/Operational".
var validLogName = regexp.MustCompile(`^[A-Za-z0-9 ._/()-]{1,255}$`).MatchString

type cachedPayload struct {
	key         string
	generatedAt time.Time
	payload     string
}

type winEventRecurringPlugin struct {
	plugin.Base
	mu    sync.Mutex
	cache cachedPayload
}

func main() {
	if runStandalone(os.Args[1:]) {
		return
	}

	if err := run(); err != nil {
		panic(err)
	}
}

func run() error {
	p := &winEventRecurringPlugin{}

	if err := plugin.RegisterMetrics(p, pluginName, metricKey, metricDescr); err != nil {
		return errs.Wrap(err, "failed to register metrics")
	}

	h, err := container.NewHandler(pluginName)
	if err != nil {
		return errs.Wrap(err, "failed to create new handler")
	}
	p.Logger = h

	if err := h.Execute(); err != nil {
		return errs.Wrap(err, "failed to execute plugin handler")
	}
	return nil
}

func (p *winEventRecurringPlugin) Export(key string, params []string, ctx plugin.ContextProvider) (any, error) {
	if key != metricKey {
		return nil, errs.Errorf("unknown item key %q", key)
	}

	logs, period, maxEvents, minCount, ignore, err := parseParams(params)
	if err != nil {
		return nil, err
	}

	sig := cacheSignature(logs, period, maxEvents, minCount, ignore)

	payload, err := p.collect(
		logs,
		period,
		maxEvents,
		minCount,
		ignore,
		effectiveTimeout(contextTimeout(ctx), exportTimeout()),
	)
	if err == nil {
		p.storeCache(sig, payload)
		return payload, nil
	}

	// Serve the last good payload on a transient failure so dependent LLD items
	// keep their values and triggers do not flap; persistent failures (cache
	// expired) surface as the real error so the master item goes unsupported and
	// the template's nodata trigger fires.
	if cached, ok := p.loadCached(sig); ok {
		return cached, nil
	}

	return nil, err
}

func (p *winEventRecurringPlugin) collect(
	logs []string,
	period time.Duration,
	maxEvents, minCount int,
	ignore string,
	collectionTimeout time.Duration,
) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), collectionTimeout)
	defer cancel()

	groups, err := FindRecurringEvents(ctx, logs, period, maxEvents, minCount, ignore)
	if err != nil {
		return "", err
	}

	b, err := json.Marshal(groups)
	if err != nil {
		return "", errs.Wrap(err, "failed to marshal recurring events")
	}
	return string(b), nil
}

// parseParams resolves the optional item-key parameters:
//
//	0: logs       comma-separated event log names (default "System,Application")
//	1: period     lookback window, Go duration ("24h", "90m") or bare hours ("24")
//	2: maxEvents  per-log cap on events scanned (default 2000)
//	3: minCount   minimum occurrences for a signature to be reported (default 2)
//	4: ignore     ignore list, "Source:EventID" rules separated by ';' ('*' wildcard)
func parseParams(params []string) (logs []string, period time.Duration, maxEvents, minCount int, ignore string, err error) {
	logs = splitCSV(param(params, 0, defaultLogs))
	if len(logs) == 0 {
		logs = splitCSV(defaultLogs)
	}
	for _, l := range logs {
		if !validLogName(l) {
			return nil, 0, 0, 0, "", errs.Errorf("invalid event log name %q", l)
		}
	}

	period, err = parsePeriod(param(params, 1, ""))
	if err != nil {
		return nil, 0, 0, 0, "", err
	}

	maxEvents, err = parsePositiveInt(param(params, 2, ""), defaultMaxEvents, "maxEvents")
	if err != nil {
		return nil, 0, 0, 0, "", err
	}

	minCount, err = parsePositiveInt(param(params, 3, ""), defaultMinCount, "minCount")
	if err != nil {
		return nil, 0, 0, 0, "", err
	}

	ignore = param(params, 4, "")

	return logs, period, maxEvents, minCount, ignore, nil
}

func param(params []string, i int, def string) string {
	if i < len(params) {
		if v := strings.TrimSpace(params[i]); v != "" {
			return v
		}
	}
	return def
}

// parsePeriod accepts a Go duration ("24h", "90m") or a bare integer number of
// hours ("24"); an empty value yields the default.
func parsePeriod(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultPeriod, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return 0, errs.Errorf("period must be positive, got %q", s)
		}
		return d, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n <= 0 {
			return 0, errs.Errorf("period must be positive, got %q", s)
		}
		return time.Duration(n) * time.Hour, nil
	}
	return 0, errs.Errorf("invalid period %q (use e.g. 24h, 90m, or a number of hours)", s)
}

func parsePositiveInt(s string, def int, name string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, errs.Errorf("invalid %s %q (expected a positive integer)", name, s)
	}
	if n <= 0 {
		return 0, errs.Errorf("%s must be positive, got %q", name, s)
	}
	return n, nil
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func cacheSignature(logs []string, period time.Duration, maxEvents, minCount int, ignore string) string {
	return fmt.Sprintf("%s|%s|%d|%d|%s", strings.Join(logs, ","), period, maxEvents, minCount, ignore)
}

func (p *winEventRecurringPlugin) storeCache(sig, payload string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cache = cachedPayload{key: sig, generatedAt: time.Now(), payload: payload}
}

func (p *winEventRecurringPlugin) loadCached(sig string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cache.payload == "" || p.cache.key != sig {
		return "", false
	}
	if time.Since(p.cache.generatedAt) > cacheTTL {
		return "", false
	}
	return p.cache.payload, true
}

// exportTimeout is the internal collection deadline. It defaults below the
// template's item timeout so the plugin returns a clean error rather than being
// hard-killed by the agent; ZBX_WINEVENT_TIMEOUT (seconds) overrides it.
func exportTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv("ZBX_WINEVENT_TIMEOUT")); v != "" {
		if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
			return time.Duration(sec) * time.Second
		}
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return time.Duration(f * float64(time.Second))
		}
	}
	return defaultTimeout
}

func contextTimeout(ctx plugin.ContextProvider) int {
	if ctx == nil {
		return 0
	}

	return ctx.Timeout()
}

// effectiveTimeout keeps the configured collector timeout as a hard cap while
// leaving the agent about one second to receive and encode the plugin result.
func effectiveTimeout(agentTimeoutSeconds int, hardCap time.Duration) time.Duration {
	if hardCap <= 0 {
		hardCap = minimumTimeout
	}
	if agentTimeoutSeconds <= 0 {
		return hardCap
	}

	agentTimeout := time.Duration(agentTimeoutSeconds) * time.Second
	available := agentTimeout - timeoutMargin
	if available <= 0 {
		available = minimumTimeout
	}
	if available < hardCap {
		return available
	}

	return hardCap
}

func printStandaloneUsage() {
	fmt.Fprintln(os.Stderr, "Windows Recurring Event Error plugin self-test")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  zabbix-agent2-windows-recurring-event.exe --standalone [--verbose] [logs] [period]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Examples:")
	fmt.Fprintln(os.Stderr, "  zabbix-agent2-windows-recurring-event.exe --standalone")
	fmt.Fprintln(os.Stderr, "  zabbix-agent2-windows-recurring-event.exe --standalone --verbose System,Application 24h")
}

// runStandalone runs the plugin's self-test and reports whether it handled
// execution. It only does so when --standalone (or --selftest) is explicitly
// requested. When Zabbix Agent 2 launches the plugin it passes the IPC socket
// path and a registerStart bool — never these flags — so this returns false and
// main proceeds to the plugin handler.
//
// Crucially it never calls os.Exit on the agent's arguments. The previous
// version exited (code 2, printing usage) when args were empty or began with
// "-"/"/", which would abort the plugin before it registered. A loadable plugin
// that fails to register takes the whole agent down with it, so the parser must
// fall through to the handler for anything that is not an explicit flag.
func runStandalone(args []string) bool {
	standalone := false
	verbose := false
	var positional []string

	for _, arg := range args {
		switch strings.ToLower(arg) {
		case "--standalone", "-standalone", "--selftest", "-selftest":
			standalone = true
		case "--verbose", "-verbose", "-v":
			verbose = true
		case "--help", "-h", "-?", "/?", "/h", "/help":
			printStandaloneUsage()
			return true
		default:
			// Anything that is not an explicit flag (the agent's socket path
			// and registerStart bool included) is ignored unless we are in
			// standalone mode, where bare values are item-key parameters.
			if !strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "/") {
				positional = append(positional, arg)
			}
		}
	}

	if !standalone {
		return false
	}

	impl := &winEventRecurringPlugin{}
	res, err := impl.Export(metricKey, positional, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	payload, _ := res.(string)
	if verbose {
		var normalized any
		if json.Unmarshal([]byte(payload), &normalized) == nil {
			if pretty, perr := json.MarshalIndent(normalized, "", "  "); perr == nil {
				payload = string(pretty)
			}
		}
	}

	fmt.Println(payload)
	return true
}
