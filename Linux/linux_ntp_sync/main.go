//go:build linux
// +build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"strings"
	"time"

	"github.com/beevik/ntp"
	"golang.zabbix.com/sdk/errs"
	"golang.zabbix.com/sdk/plugin"
	"golang.zabbix.com/sdk/plugin/container"
	"golang.zabbix.com/sdk/zbxerr"
)

const (
	pluginName  = "LinuxNTPSync"
	metricKey   = "linux.ntp.sync"
	metricDescr = "Return the time difference (seconds) between the local clock and a given NTP server " +
		"as JSON (linux.ntp.sync[server]). Fields: success, ntp_server, local_ts, ntp_ts, " +
		"diff_seconds, stratum, rtt_ms, attempts, error."

	maxRetries   = 3
	queryTimeout = 3 * time.Second
	retryBackoff = 250 * time.Millisecond
	maxServerLen = 255

	standaloneRequestTimeout = 10 * time.Second
	agentTimeoutMargin       = 250 * time.Millisecond
)

type LinuxNTPSyncPlugin struct {
	plugin.Base
}

var ntpImpl LinuxNTPSyncPlugin

// NtpOutput is the JSON contract consumed by the Zabbix template.
//
// DiffSeconds and RttMs are pointers so that a legitimate 0.0 value (a perfectly
// synchronised clock) is still serialised, while a failed query omits them
// entirely instead of reporting a misleading 0.
type NtpOutput struct {
	Success     bool     `json:"success"`
	NtpServer   string   `json:"ntp_server,omitempty"`
	LocalTs     string   `json:"local_ts,omitempty"`
	NtpTs       string   `json:"ntp_ts,omitempty"`
	DiffSeconds *float64 `json:"diff_seconds,omitempty"`
	Stratum     uint8    `json:"stratum,omitempty"`
	RttMs       *float64 `json:"rtt_ms,omitempty"`
	Attempts    int      `json:"attempts,omitempty"`
	Error       string   `json:"error,omitempty"`
}

func (p *LinuxNTPSyncPlugin) Export(key string, params []string, provider plugin.ContextProvider) (interface{}, error) {
	if key != metricKey {
		return nil, errs.Wrapf(zbxerr.ErrorUnsupportedMetric, "unknown metric %q", key)
	}

	switch len(params) {
	case 0:
		return nil, errs.Wrapf(zbxerr.ErrorTooFewParameters, "%s expects exactly one server parameter", metricKey)
	case 1:
		// Expected arity.
	default:
		return nil, errs.Wrapf(zbxerr.ErrorTooManyParameters, "%s expects exactly one server parameter", metricKey)
	}

	server := strings.TrimSpace(params[0])
	if err := validateServer(server); err != nil {
		return nil, errs.WrapConst(err, zbxerr.ErrorInvalidParams)
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout(provider))
	defer cancel()

	resp, attempts, lastErr := queryNTP(ctx, server)
	if lastErr != nil {
		return marshalNtp(NtpOutput{
			Success:   false,
			NtpServer: server,
			Attempts:  attempts,
			Error:     fmt.Sprintf("NTP query failed after %d attempt(s): %v", attempts, lastErr),
		})
	}

	// ClockOffset is positive when the local clock is behind the server.
	diffSec := roundNano(resp.ClockOffset.Seconds())
	rttMs := roundNano(resp.RTT.Seconds() * 1000)

	return marshalNtp(NtpOutput{
		Success:     true,
		NtpServer:   server,
		LocalTs:     time.Now().UTC().Format(time.RFC3339Nano),
		NtpTs:       resp.Time.UTC().Format(time.RFC3339Nano),
		DiffSeconds: &diffSec,
		Stratum:     resp.Stratum,
		RttMs:       &rttMs,
		Attempts:    attempts,
	})
}

// queryNTP attempts the query up to maxRetries times, validating each response
// (Validate rejects Kiss-of-Death packets and an unsynchronised server, i.e.
// stratum 0 or >= 16). Returns the first valid response, the number of attempts
// made, and the last error on total failure.
func queryNTP(ctx context.Context, server string) (*ntp.Response, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	// Bound DNS resolution explicitly. QueryOptions.Timeout covers only the UDP
	// exchange, not name resolution, so a hung resolver would otherwise let a
	// single Export run for the OS resolver's timeout on every attempt and blow
	// past the item timeout. Resolve once up front (DNS is stable across retries)
	// and query the resulting address.
	dnsTimeout, err := operationTimeout(ctx, queryTimeout)
	if err != nil {
		return nil, 0, err
	}
	dnsCtx, cancel := context.WithTimeout(ctx, dnsTimeout)
	addr, err := resolveNTPServer(dnsCtx, server)
	cancel()
	if err != nil {
		return nil, 0, err
	}

	var lastErr error
	attempts := 0
	for try := 1; try <= maxRetries; try++ {
		if try > 1 {
			if err := waitForRetry(ctx, retryBackoff); err != nil {
				return nil, attempts, err
			}
		}

		attemptTimeout, err := operationTimeout(ctx, queryTimeout)
		if err != nil {
			return nil, attempts, err
		}

		attempts = try
		r, err := ntp.QueryWithOptions(addr, ntp.QueryOptions{Timeout: attemptTimeout})
		if err != nil {
			lastErr = err
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, attempts, ctxErr
			}
			continue
		}
		if r.IsKissOfDeath() {
			// RFC 5905: a Kiss-of-Death (RATE/DENY/RSTR) must not be retried —
			// hammering the server can get the agent's IP rate-limited or blocked.
			// Fail immediately instead of looping.
			return nil, try, fmt.Errorf("server sent Kiss-of-Death %q; not retrying", r.KissCode)
		}
		if err := r.Validate(); err != nil {
			lastErr = err
			continue
		}
		return r, try, nil
	}
	return nil, attempts, lastErr
}

// resolveNTPServer resolves the (optionally host:port) server to an address with
// a bounded DNS timeout. Literal IPs are returned unchanged (no lookup). Any
// port supplied by the caller is preserved.
func resolveNTPServer(ctx context.Context, server string) (string, error) {
	host := server
	port := ""
	if h, p, err := net.SplitHostPort(server); err == nil {
		host, port = h, p
	}
	ipHost := host
	if zone := strings.LastIndexByte(ipHost, '%'); zone >= 0 {
		ipHost = ipHost[:zone]
	}
	if net.ParseIP(ipHost) != nil {
		return server, nil // already a literal IP; no DNS needed
	}

	var res net.Resolver
	addrs, err := res.LookupHost(ctx, host)
	if err != nil {
		return "", fmt.Errorf("DNS resolution of %q failed: %v", host, err)
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("DNS resolution of %q returned no addresses", host)
	}
	if port != "" {
		return net.JoinHostPort(addrs[0], port), nil
	}
	return addrs[0], nil
}

// requestTimeout translates Agent 2's whole-second item timeout into a local
// deadline. The small margin lets the loadable-plugin protocol return its
// result before Agent 2 closes the request. Standalone probes have no outer
// Agent 2 deadline, so they use a safe bounded fallback without the margin.
func requestTimeout(provider plugin.ContextProvider) time.Duration {
	if provider == nil || provider.Timeout() <= 0 {
		return standaloneRequestTimeout
	}

	timeout := time.Duration(provider.Timeout()) * time.Second
	if timeout > agentTimeoutMargin {
		timeout -= agentTimeoutMargin
	}

	return timeout
}

// operationTimeout caps one DNS or UDP operation by both its normal limit and
// the time left on the overall request. QueryWithOptions has no Context field,
// but its connection deadline honours this derived duration.
func operationTimeout(ctx context.Context, limit time.Duration) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, context.DeadlineExceeded
		}
		if remaining < limit {
			return remaining, nil
		}
	}

	return limit, nil
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func validateServer(server string) error {
	if server == "" {
		return errs.New("no NTP server provided")
	}
	if len(server) > maxServerLen {
		return errs.Errorf("NTP server name too long (max %d characters)", maxServerLen)
	}
	for _, r := range server {
		if r < 0x20 || r == 0x7f {
			return errs.New("NTP server name contains control characters")
		}
	}
	return nil
}

// roundNano rounds a seconds-based float to nanosecond precision so the JSON
// does not carry meaningless floating-point noise.
func roundNano(v float64) float64 {
	return math.Round(v*1e9) / 1e9
}

func marshalNtp(v NtpOutput) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"success":false,"error":"json marshal failed: %v"}`, err), nil
	}
	return string(b), nil
}

func init() {
	if err := plugin.RegisterMetrics(&ntpImpl, pluginName, metricKey, metricDescr); err != nil {
		panic(err)
	}
}

func main() {
	if server, ok := standaloneServer(os.Args[1:]); ok {
		res, err := ntpImpl.Export(metricKey, []string{server}, nil)
		if err != nil {
			fmt.Printf("{\"success\":false,\"error\":\"%v\"}\n", err)
			os.Exit(1)
		}
		fmt.Println(res)
		return
	}

	h, err := container.NewHandler(pluginName)
	if err != nil {
		panic(err)
	}
	ntpImpl.Logger = h
	if err := h.Execute(); err != nil {
		panic(err)
	}
}

// standaloneServer reports whether --standalone was requested and, if so, which
// server to query (the first non-flag argument, defaulting to pool.ntp.org).
func standaloneServer(args []string) (string, bool) {
	standalone := false
	server := "pool.ntp.org"
	for _, a := range args {
		switch strings.ToLower(a) {
		case "--standalone", "-standalone", "standalone":
			standalone = true
		default:
			if !strings.HasPrefix(a, "-") {
				server = a
			}
		}
	}
	return server, standalone
}
