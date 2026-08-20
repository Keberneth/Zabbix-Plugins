//go:build windows

package main

import (
	"errors"
	"testing"

	"golang.zabbix.com/sdk/zbxerr"
)

func TestExportRejectsParameters(t *testing.T) {
	p := &ApplicationInventoryPlugin{}

	value, err := p.Export(metricKey, []string{"unexpected"}, nil)
	if value != nil {
		t.Fatalf("Export() value = %v, want nil", value)
	}
	if !errors.Is(err, zbxerr.ErrorTooManyParameters) {
		t.Fatalf("Export() error = %v, want ErrorTooManyParameters", err)
	}
}

func TestExportRejectsUnsupportedMetric(t *testing.T) {
	p := &ApplicationInventoryPlugin{}

	value, err := p.Export("unknown.key", nil, nil)
	if value != nil {
		t.Fatalf("Export() value = %v, want nil", value)
	}
	if !errors.Is(err, zbxerr.ErrorUnsupportedMetric) {
		t.Fatalf("Export() error = %v, want ErrorUnsupportedMetric", err)
	}
}

func TestCollectInstalledAppsAllRootsFail(t *testing.T) {
	firstErr := errors.New("first root unavailable")
	secondErr := errors.New("second root unavailable")
	roots := []uninstallRoot{{path: "first"}, {path: "second"}}

	apps, warns, err := collectInstalledAppsFrom(
		roots,
		func(root uninstallRoot) ([]AppEntry, []error, error) {
			if root.path == "first" {
				return nil, nil, firstErr
			}
			return nil, nil, secondErr
		},
	)

	if apps == nil || len(apps) != 0 {
		t.Fatalf("collectInstalledAppsFrom() apps = %#v, want non-nil empty slice", apps)
	}
	if len(warns) != 2 {
		t.Fatalf("collectInstalledAppsFrom() warnings = %d, want 2", len(warns))
	}
	if !errors.Is(err, firstErr) || !errors.Is(err, secondErr) {
		t.Fatalf("collectInstalledAppsFrom() error = %v, want both root errors", err)
	}
}

func TestCollectInstalledAppsPreservesPartialDataAndWarnings(t *testing.T) {
	rootWarning := errors.New("one subkey was unreadable")
	failedRootErr := errors.New("second root unavailable")
	roots := []uninstallRoot{{path: "readable"}, {path: "failed"}}

	apps, warns, err := collectInstalledAppsFrom(
		roots,
		func(root uninstallRoot) ([]AppEntry, []error, error) {
			if root.path == "readable" {
				return []AppEntry{{DisplayName: "Zabbix Agent 2"}}, []error{rootWarning}, nil
			}
			return nil, nil, failedRootErr
		},
	)

	if err != nil {
		t.Fatalf("collectInstalledAppsFrom() error = %v, want nil for partial success", err)
	}
	if len(apps) != 1 || apps[0].DisplayName != "Zabbix Agent 2" {
		t.Fatalf("collectInstalledAppsFrom() apps = %#v, want partial result", apps)
	}
	if len(warns) != 2 || !errors.Is(warns[0], rootWarning) || !errors.Is(warns[1], failedRootErr) {
		t.Fatalf("collectInstalledAppsFrom() warnings = %#v, want subkey and root warnings", warns)
	}
}

func TestMarshalInventoryJSONPropagatesError(t *testing.T) {
	wantErr := errors.New("JSON encoder failed")

	value, err := marshalInventoryJSON(
		[]AppEntry{{DisplayName: "Zabbix Agent 2"}},
		func([]AppEntry) (string, error) { return "", wantErr },
	)

	if value != "" {
		t.Fatalf("marshalInventoryJSON() value = %q, want empty", value)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("marshalInventoryJSON() error = %v, want wrapped marshal error", err)
	}
	if !errors.Is(err, zbxerr.ErrorCannotMarshalJSON) {
		t.Fatalf("marshalInventoryJSON() error = %v, want ErrorCannotMarshalJSON", err)
	}
}
