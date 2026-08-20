//go:build windows

package main

import (
	"errors"
	"testing"

	"golang.zabbix.com/sdk/zbxerr"
)

func TestExportRejectsParameters(t *testing.T) {
	p := &impl{}

	value, err := p.Export(metricKey, []string{"unexpected"}, nil)
	if value != nil {
		t.Fatalf("Export() value = %v, want nil", value)
	}
	if !errors.Is(err, zbxerr.ErrorTooManyParameters) {
		t.Fatalf("Export() error = %v, want ErrorTooManyParameters", err)
	}
}

func TestExportRejectsUnsupportedMetric(t *testing.T) {
	p := &impl{}

	value, err := p.Export("unknown.key", nil, nil)
	if value != nil {
		t.Fatalf("Export() value = %v, want nil", value)
	}
	if !errors.Is(err, zbxerr.ErrorUnsupportedMetric) {
		t.Fatalf("Export() error = %v, want ErrorUnsupportedMetric", err)
	}
}

func TestCollectNetworkPayloadPropagatesCollectionError(t *testing.T) {
	wantErr := errors.New("TCP table unavailable")

	value, err := collectNetworkPayload(
		func() (*payload, error) { return nil, wantErr },
		func(any) ([]byte, error) {
			t.Fatal("marshal called after collection failure")
			return nil, nil
		},
	)

	if value != "" {
		t.Fatalf("collectNetworkPayload() value = %q, want empty", value)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("collectNetworkPayload() error = %v, want wrapped collection error", err)
	}
	if !errors.Is(err, zbxerr.ErrorCannotFetchData) {
		t.Fatalf("collectNetworkPayload() error = %v, want ErrorCannotFetchData", err)
	}
}

func TestCollectNetworkPayloadPropagatesMarshalError(t *testing.T) {
	wantErr := errors.New("JSON encoder failed")

	value, err := collectNetworkPayload(
		func() (*payload, error) { return &payload{}, nil },
		func(any) ([]byte, error) { return nil, wantErr },
	)

	if value != "" {
		t.Fatalf("collectNetworkPayload() value = %q, want empty", value)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("collectNetworkPayload() error = %v, want wrapped marshal error", err)
	}
	if !errors.Is(err, zbxerr.ErrorCannotMarshalJSON) {
		t.Fatalf("collectNetworkPayload() error = %v, want ErrorCannotMarshalJSON", err)
	}
}
