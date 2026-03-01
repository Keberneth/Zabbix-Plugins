//go:build windows
// +build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/windows/registry"

	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"

	"golang.zabbix.com/sdk/errs"
	"golang.zabbix.com/sdk/plugin"
	"golang.zabbix.com/sdk/plugin/container"
)

const (
	// IMPORTANT: pluginName must match the name used in the Zabbix Agent2 plugin config section:
	//   Plugins.<pluginName>.System.Path=...
	pluginName = "NeedsRebootCheck"

	// IMPORTANT: key must NOT change (referenced by your Zabbix items).
	metricKey = "system.needs_reboot"
)

// NeedsRebootCheckPlugin implements Zabbix Agent2 Go plugin exporter.
//
// Notes about "1" after a reboot:
// - Not necessarily a false positive.
// - Windows Update / CBS servicing can legitimately require multiple reboots.
// - The most common avoidable false positive is Session Manager's
//   PendingFileRenameOperations existing but being empty. This version
//   treats empty as NOT pending.
//
// Embed plugin.Base to satisfy plugin.Accessor (includes HandleTimeout).
type NeedsRebootCheckPlugin struct {
	plugin.Base
}

var (
	_    plugin.Exporter = (*NeedsRebootCheckPlugin)(nil)
	impl NeedsRebootCheckPlugin
)

func main() {
	standalone, verbose := parseArgs(os.Args[1:])
	if standalone {
		code := runStandalone(verbose)
		fmt.Println(code)
		os.Exit(0)
	}

	if err := runPlugin(); err != nil {
		panic(err)
	}
}

func parseArgs(args []string) (standalone bool, verbose bool) {
	for _, a := range args {
		s := strings.TrimSpace(strings.ToLower(a))
		s = strings.TrimLeft(s, "-")
		s = strings.TrimSpace(s)
		switch s {
		case "standalone":
			standalone = true
		case "verbose", "v":
			verbose = true
		}
	}
	return
}

func runPlugin() error {
	// Description must end with a dot (SDK validation).
	if err := plugin.RegisterMetrics(
		&impl,
		pluginName,
		metricKey,
		"Checks if Windows needs a reboot (pending reboot).",
	); err != nil {
		return errs.Wrap(err, "failed to register metrics")
	}

	h, err := container.NewHandler(pluginName)
	if err != nil {
		return errs.Wrap(err, "failed to create new handler")
	}

	impl.Logger = h

	if err := h.Execute(); err != nil {
		return errs.Wrap(err, "failed to execute plugin handler")
	}
	return nil
}

func runStandalone(verbose bool) string {
	pending, reasons, err := isRebootPendingDetailed()
	if err != nil {
		// Best-effort: if we cannot determine, return 0.
		if verbose {
			fmt.Fprintln(os.Stderr, "needs_reboot_check: error:", err)
		}
		return "0"
	}

	if verbose {
		if pending {
			fmt.Fprintln(os.Stderr, "needs_reboot_check: pending reboot detected:")
			for _, r := range reasons {
				fmt.Fprintln(os.Stderr, "-", r)
			}
		} else {
			fmt.Fprintln(os.Stderr, "needs_reboot_check: no pending reboot flags detected")
		}
	}

	if pending {
		return "1"
	}
	return "0"
}

func (p *NeedsRebootCheckPlugin) Export(key string, _ []string, _ plugin.ContextProvider) (interface{}, error) {
	if key != metricKey {
		return nil, errs.Errorf("unknown item key %q", key)
	}

	pending, reasons, err := isRebootPendingDetailed()
	if err != nil {
		// Keep old PowerShell behavior: best-effort.
		if p.Logger != nil {
			p.Infof("%s: reboot pending check error: %v", pluginName, err)
		}
		return "0", nil
	}

	if pending {
		if p.Logger != nil && len(reasons) > 0 {
			p.Infof("%s: pending reboot detected: %s", pluginName, strings.Join(reasons, ", "))
		}
		return "1", nil
	}

	return "0", nil
}

func isRebootPendingDetailed() (pending bool, reasons []string, err error) {
	reasons = make([]string, 0, 4)

	// 1) Component Based Servicing (CBS)
	{
		const path = `SOFTWARE\\Microsoft\\Windows\\CurrentVersion\\Component Based Servicing\\RebootPending`
		ok, e := registryKeyExists(registry.LOCAL_MACHINE, path)
		if e != nil {
			return false, nil, e
		}
		if ok {
			reasons = append(reasons, "CBS:RebootPending")
		}
	}

	// 2) Windows Update
	{
		const path = `SOFTWARE\\Microsoft\\Windows\\CurrentVersion\\WindowsUpdate\\Auto Update\\RebootRequired`
		ok, e := registryKeyExists(registry.LOCAL_MACHINE, path)
		if e != nil {
			return false, nil, e
		}
		if ok {
			reasons = append(reasons, "WindowsUpdate:RebootRequired")
		}
	}

	// 3) Session Manager file rename operations (value)
	// Treat as pending ONLY if non-empty (avoid common false positives).
	{
		ok, reason, e := pendingFileRenameOperationsReason()
		if e != nil {
			return false, nil, e
		}
		if ok {
			reasons = append(reasons, reason)
		}
	}

	// If we already have clear registry evidence, no need to hit SCCM/WMI (slower).
	if len(reasons) > 0 {
		return true, reasons, nil
	}

	// 4) SCCM/ConfigMgr Client SDK (best-effort)
	if sccmPending, e := sccmRebootPending(); e == nil && sccmPending {
		reasons = append(reasons, "SCCM:DetermineIfRebootPending")
		return true, reasons, nil
	}

	return false, nil, nil
}

func isNotExistErr(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, registry.ErrNotExist) ||
		errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, syscall.ERROR_PATH_NOT_FOUND)
}

func registryKeyExists(root registry.Key, path string) (bool, error) {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		if isNotExistErr(err) {
			return false, nil
		}
		return false, err
	}
	_ = k.Close()
	return true, nil
}

func pendingFileRenameOperationsReason() (pending bool, reason string, err error) {
	const path = `SYSTEM\\CurrentControlSet\\Control\\Session Manager`

	// Most common is PendingFileRenameOperations.
	pending, unreadable, err := registryValueNonEmptyOrUnreadable(registry.LOCAL_MACHINE, path, "PendingFileRenameOperations")
	if err != nil {
		return false, "", err
	}
	if pending {
		if unreadable {
			return true, "SessionManager:PendingFileRenameOperations (unreadable)", nil
		}
		return true, "SessionManager:PendingFileRenameOperations", nil
	}

	// Some systems use the "2" variant.
	pending, unreadable, err = registryValueNonEmptyOrUnreadable(registry.LOCAL_MACHINE, path, "PendingFileRenameOperations2")
	if err != nil {
		return false, "", err
	}
	if pending {
		if unreadable {
			return true, "SessionManager:PendingFileRenameOperations2 (unreadable)", nil
		}
		return true, "SessionManager:PendingFileRenameOperations2", nil
	}

	return false, "", nil
}

// registryValueNonEmptyOrUnreadable checks if a registry value exists and has non-empty content.
//
// Returns:
// - pending=true,  unreadable=false : value exists and is non-empty
// - pending=false, unreadable=false : value missing OR exists but empty
// - pending=true,  unreadable=true  : value appears to exist but could not be decoded (assume pending; matches Test-Path behavior)
func registryValueNonEmptyOrUnreadable(root registry.Key, path, valueName string) (pending bool, unreadable bool, err error) {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		if isNotExistErr(err) {
			return false, false, nil
		}
		return false, false, err
	}
	defer k.Close()

	// REG_MULTI_SZ (most common)
	if vals, _, e := k.GetStringsValue(valueName); e == nil {
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return true, false, nil
			}
		}
		return false, false, nil
	} else if isNotExistErr(e) {
		return false, false, nil
	}

	// REG_SZ (rare)
	if s, _, e := k.GetStringValue(valueName); e == nil {
		if strings.TrimSpace(s) != "" {
			return true, false, nil
		}
		return false, false, nil
	} else if isNotExistErr(e) {
		return false, false, nil
	}

	// REG_BINARY
	if b, _, e := k.GetBinaryValue(valueName); e == nil {
		if len(b) > 0 {
			return true, false, nil
		}
		return false, false, nil
	} else if isNotExistErr(e) {
		return false, false, nil
	}

	// REG_DWORD/QWORD
	if i, _, e := k.GetIntegerValue(valueName); e == nil {
		if i != 0 {
			return true, false, nil
		}
		return false, false, nil
	} else if isNotExistErr(e) {
		return false, false, nil
	}

	// If we get here, the value is likely present but has a type we didn't decode,
	// or WMI/registry returned an unexpected error. To avoid false negatives, assume pending.
	return true, true, nil
}

// sccmRebootPending calls:
//   ROOT\\CCM\\ClientSDK : CCM_ClientUtilities.DetermineIfRebootPending()
// Returns (false, error) if SCCM client/WMI is not available.
func sccmRebootPending() (bool, error) {
	if err := ole.CoInitialize(0); err != nil {
		return false, err
	}
	defer ole.CoUninitialize()

	locObj, err := oleutil.CreateObject("WbemScripting.SWbemLocator")
	if err != nil {
		return false, err
	}
	defer locObj.Release()

	loc, err := locObj.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return false, err
	}
	defer loc.Release()

	// Connect to local WMI namespace.
	svcRaw, err := oleutil.CallMethod(loc, "ConnectServer", nil, `ROOT\\CCM\\ClientSDK`)
	if err != nil {
		return false, err
	}
	svc := svcRaw.ToIDispatch()
	defer svc.Release()

	// ExecMethod_ on the class (static method).
	resRaw, err := oleutil.CallMethod(svc, "ExecMethod_", "CCM_ClientUtilities", "DetermineIfRebootPending")
	if err != nil {
		return false, err
	}
	res := resRaw.ToIDispatch()
	defer res.Release()

	// Prefer RebootPending; if missing/false, also check IsHardRebootPending when available.
	if b, ok, e := getBoolProperty(res, "RebootPending"); e == nil && ok {
		if b {
			return true, nil
		}
	}
	if b, ok, e := getBoolProperty(res, "IsHardRebootPending"); e == nil && ok {
		if b {
			return true, nil
		}
	}

	return false, nil
}

func getBoolProperty(disp *ole.IDispatch, name string) (value bool, present bool, err error) {
	v, err := oleutil.GetProperty(disp, name)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = v.Clear() }()

	val := v.Value()
	switch t := val.(type) {
	case bool:
		return t, true, nil
	case int8:
		return t != 0, true, nil
	case int16:
		return t != 0, true, nil
	case int32:
		return t != 0, true, nil
	case int64:
		return t != 0, true, nil
	case int:
		return t != 0, true, nil
	case uint8:
		return t != 0, true, nil
	case uint16:
		return t != 0, true, nil
	case uint32:
		return t != 0, true, nil
	case uint64:
		return t != 0, true, nil
	case uint:
		return t != 0, true, nil
	case string:
		s := strings.TrimSpace(strings.ToLower(t))
		return s == "true" || s == "1", true, nil
	default:
		return false, true, errors.New("unexpected type")
	}
}
