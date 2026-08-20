//go:build linux

package main

import (
	"errors"
	"testing"

	"golang.zabbix.com/sdk/zbxerr"
)

func TestExportRejectsParameters(t *testing.T) {
	p := &NeedsRebootCheckPlugin{}

	value, err := p.Export(metricKey, []string{"unexpected"}, nil)
	if value != nil {
		t.Fatalf("Export() value = %v, want nil", value)
	}
	if !errors.Is(err, zbxerr.ErrorTooManyParameters) {
		t.Fatalf("Export() error = %v, want ErrorTooManyParameters", err)
	}
}

func TestExportRejectsUnsupportedMetric(t *testing.T) {
	p := &NeedsRebootCheckPlugin{}

	value, err := p.Export("unknown.key", nil, nil)
	if value != nil {
		t.Fatalf("Export() value = %v, want nil", value)
	}
	if !errors.Is(err, zbxerr.ErrorUnsupportedMetric) {
		t.Fatalf("Export() error = %v, want ErrorUnsupportedMetric", err)
	}
}

func TestRebootMetricValue(t *testing.T) {
	checkErr := errors.New("kernel query failed")

	tests := []struct {
		name      string
		pending   bool
		checkErr  error
		wantValue string
		wantErr   error
	}{
		{
			name:      "confirmed reboot overrides secondary failure",
			pending:   true,
			checkErr:  checkErr,
			wantValue: "1",
		},
		{
			name:     "unconfirmed result propagates check failure",
			checkErr: checkErr,
			wantErr:  zbxerr.ErrorCannotFetchData,
		},
		{
			name:      "successful negative result",
			wantValue: "0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, err := rebootMetricValue(tt.pending, tt.checkErr)
			if value != tt.wantValue {
				t.Fatalf("rebootMetricValue() value = %q, want %q", value, tt.wantValue)
			}
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("rebootMetricValue() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("rebootMetricValue() error = %v, want %v", err, tt.wantErr)
			}
			if !errors.Is(err, checkErr) {
				t.Fatalf("rebootMetricValue() error = %v, want wrapped check error", err)
			}
		})
	}
}
