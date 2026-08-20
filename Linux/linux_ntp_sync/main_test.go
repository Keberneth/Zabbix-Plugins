//go:build linux
// +build linux

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.zabbix.com/sdk/plugin"
	"golang.zabbix.com/sdk/zbxerr"
)

type testContextProvider struct {
	timeout int
}

func (c testContextProvider) ClientID() uint64                   { return 0 }
func (c testContextProvider) ItemID() uint64                     { return 0 }
func (c testContextProvider) Output() plugin.ResultWriter        { return nil }
func (c testContextProvider) Meta() *plugin.Meta                 { return nil }
func (c testContextProvider) GlobalRegexp() plugin.RegexpMatcher { return nil }
func (c testContextProvider) Timeout() int                       { return c.timeout }
func (c testContextProvider) Delay() string                      { return "" }

func TestExportRejectsInvalidParameters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		params []string
		want   error
	}{
		{name: "missing", params: nil, want: zbxerr.ErrorTooFewParameters},
		{name: "extra", params: []string{"time.example.test", "extra"}, want: zbxerr.ErrorTooManyParameters},
		{name: "empty", params: []string{"   "}, want: zbxerr.ErrorInvalidParams},
		{name: "control character", params: []string{"time.\nexample.test"}, want: zbxerr.ErrorInvalidParams},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			value, err := ntpImpl.Export(metricKey, tt.params, testContextProvider{timeout: 10})
			if value != nil {
				t.Fatalf("Export() value = %v, want nil", value)
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("Export() error = %v, want error wrapping %v", err, tt.want)
			}
		})
	}
}

func TestExportRejectsUnknownMetric(t *testing.T) {
	t.Parallel()

	value, err := ntpImpl.Export("linux.ntp.unknown", []string{"time.example.test"}, nil)
	if value != nil {
		t.Fatalf("Export() value = %v, want nil", value)
	}
	if !errors.Is(err, zbxerr.ErrorUnsupportedMetric) {
		t.Fatalf("Export() error = %v, want unsupported metric", err)
	}
}

func TestRequestTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider plugin.ContextProvider
		want     time.Duration
	}{
		{name: "standalone", provider: nil, want: standaloneRequestTimeout},
		{name: "invalid provider timeout uses fallback", provider: testContextProvider{}, want: standaloneRequestTimeout},
		{name: "agent timeout reserves response margin", provider: testContextProvider{timeout: 10}, want: 10*time.Second - agentTimeoutMargin},
		{name: "short agent timeout remains positive", provider: testContextProvider{timeout: 1}, want: time.Second - agentTimeoutMargin},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := requestTimeout(tt.provider); got != tt.want {
				t.Fatalf("requestTimeout() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestOperationTimeoutUsesRemainingBudget(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	got, err := operationTimeout(ctx, queryTimeout)
	if err != nil {
		t.Fatalf("operationTimeout() error = %v", err)
	}
	if got <= 0 || got > time.Second {
		t.Fatalf("operationTimeout() = %s, want within (0s, 1s]", got)
	}
}

func TestCanceledRequestStopsBeforeQuery(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	response, attempts, err := queryNTP(ctx, "127.0.0.1")
	if response != nil {
		t.Fatalf("queryNTP() response = %v, want nil", response)
	}
	if attempts != 0 {
		t.Fatalf("queryNTP() attempts = %d, want 0", attempts)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("queryNTP() error = %v, want context.Canceled", err)
	}
}

func TestRetryWaitHonorsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := waitForRetry(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForRetry() error = %v, want context.Canceled", err)
	}
}
