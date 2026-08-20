//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExportRejectsInvalidParametersAsErrorValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		params []string
	}{
		{name: "missing", params: nil},
		{name: "one parameter", params: []string{"/var/log/secure"}},
		{name: "extra parameter", params: []string{"/var/log/secure", "breakglass", "extra"}},
		{name: "empty logfile", params: []string{" ", "breakglass"}},
		{name: "empty accounts", params: []string{"/var/log/secure", " , "}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := &BTGPlugin{checkpointOK: make(map[string]error)}
			got, err := p.Export(metricKey, tc.params, nil)
			if err != nil {
				t.Fatalf("Export returned a Go error; the template contract requires an ERROR value: %v", err)
			}

			value, ok := got.(string)
			if !ok {
				t.Fatalf("Export returned %T, want string", got)
			}
			if !strings.HasPrefix(value, "ERROR: ") {
				t.Fatalf("Export returned %q, want ERROR: prefix", value)
			}
		})
	}
}

func TestDeduperPrunesExpiredEntriesOpportunistically(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()
	d := &deduper{
		last: map[string]int64{
			"stale": now - 10,
		},
		ttl: 1,
	}

	if !d.shouldSend("fresh") {
		t.Fatal("first occurrence should be sent")
	}
	if _, ok := d.last["stale"]; ok {
		t.Fatal("expired entry was not pruned")
	}
	if d.shouldSend("fresh") {
		t.Fatal("duplicate inside the TTL should be suppressed")
	}
}

func TestProcessOnceHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p := &BTGPlugin{ded: newDeduper(300)}
	_, _, err := p.processOnce(ctx, filepath.Join(t.TempDir(), "not-read"), []string{"breakglass"}, t.TempDir())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("processOnce error = %v, want context.Canceled", err)
	}
}

func TestConcurrentScansKeepCheckpointAtNewestOffset(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "secure")
	line := "sshd[123]: Accepted password for breakglass from 192.0.2.10 port 22 ssh2\n"
	if err := os.WriteFile(logPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	p := &BTGPlugin{ded: newDeduper(300)}
	type result struct {
		found bool
		err   error
	}
	results := make(chan result, 2)

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			found, _, err := p.processOnce(context.Background(), logPath, []string{"breakglass"}, dir)
			results <- result{found: found, err: err}
		}()
	}
	wg.Wait()
	close(results)

	foundCount := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("processOnce failed: %v", result.err)
		}
		if result.found {
			foundCount++
		}
	}
	if foundCount != 1 {
		t.Fatalf("found count = %d, want exactly one scan to report the line", foundCount)
	}

	offset, _, found := loadCheckpointForPath(dir, logPath)
	if !found {
		t.Fatal("checkpoint was not saved")
	}
	if offset != int64(len(line)) {
		t.Fatalf("checkpoint offset = %d, want %d", offset, len(line))
	}
}
