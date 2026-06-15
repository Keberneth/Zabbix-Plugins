//go:build windows
// +build windows

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/beevik/ntp"
	"golang.zabbix.com/sdk/errs"
	"golang.zabbix.com/sdk/plugin"
	"golang.zabbix.com/sdk/plugin/container"
)

const (
	pluginName = "WindowsNTPSync"
	metricKey  = "windows.ntp.sync"
	metricDescr = "Return the time difference (seconds) between the local clock and a given NTP server " +
		"as JSON (windows.ntp.sync[server]). Fields: success, ntp_server, local_ts, ntp_ts, " +
		"diff_seconds, stratum, rtt_ms, attempts, error."

	maxRetries   = 3
	queryTimeout = 3 * time.Second
	retryBackoff = 250 * time.Millisecond
	maxServerLen = 255
)

type WindowsNTPSyncPlugin struct {
	plugin.Base
}

var ntpImpl WindowsNTPSyncPlugin

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

func (p *WindowsNTPSyncPlugin) Export(key string, params []string, _ plugin.ContextProvider) (interface{}, error) {
	if key != metricKey {
		return nil, errs.Errorf("unknown item key %q", key)
	}
	if len(params) < 1 {
		return marshalNtp(NtpOutput{Success: false, Error: "no NTP server provided"})
	}

	server := strings.TrimSpace(params[0])
	if err := validateServer(server); err != nil {
		return marshalNtp(NtpOutput{Success: false, NtpServer: server, Error: err.Error()})
	}

	resp, attempts, lastErr := queryNTP(server)
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
func queryNTP(server string) (*ntp.Response, int, error) {
	var lastErr error
	for try := 1; try <= maxRetries; try++ {
		if try > 1 {
			time.Sleep(retryBackoff)
		}

		r, err := ntp.QueryWithOptions(server, ntp.QueryOptions{Timeout: queryTimeout})
		if err != nil {
			lastErr = err
			continue
		}
		if err := r.Validate(); err != nil {
			lastErr = err
			continue
		}
		return r, try, nil
	}
	return nil, maxRetries, lastErr
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
