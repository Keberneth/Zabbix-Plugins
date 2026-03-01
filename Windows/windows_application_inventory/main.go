//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"golang.org/x/sys/windows/registry"

	"golang.zabbix.com/sdk/errs"
	"golang.zabbix.com/sdk/plugin"
	"golang.zabbix.com/sdk/plugin/container"
)

const (
	// IMPORTANT: pluginName must match the name used in the Zabbix Agent2 plugin config section:
	//   Plugins.<pluginName>.System.Path=...
	pluginName = "ApplicationInventory"

	// IMPORTANT: key must NOT change (referenced by your Zabbix items).
	metricKey = "application.inventory"
)

var _ plugin.Exporter = (*ApplicationInventoryPlugin)(nil)

// Embed plugin.Base to satisfy the SDK's plugin.Accessor interface (includes HandleTimeout).
type ApplicationInventoryPlugin struct {
	plugin.Base
}

var impl ApplicationInventoryPlugin

// AppEntry matches the PowerShell output structure.
// Field order is intentional (PowerShell ConvertTo-Json preserves property insertion order).
type AppEntry struct {
	DisplayName     string  `json:"DisplayName"`
	Version         *string `json:"Version"`
	Publisher       *string `json:"Publisher"`
	InstallDate     *string `json:"InstallDate"`
	InstallLocation *string `json:"InstallLocation"`
	RegistryPath    string  `json:"RegistryPath"`
}

func main() {
	standalone := false
	verbose := false

	// IMPORTANT: do NOT use flag.Parse() in Agent2 plugins.
	// Agent2 may pass its own args to the plugin process.
	for _, a := range os.Args[1:] {
		s := strings.TrimSpace(strings.ToLower(a))
		switch s {
		case "--standalone", "-standalone", "standalone":
			standalone = true
		case "--verbose", "-verbose", "verbose":
			verbose = true
		}
	}

	if standalone {
		out, err := runStandalone(verbose)
		fmt.Print(out)
		if err != nil {
			os.Exit(1)
		}
		return
	}

	if err := runPlugin(); err != nil {
		panic(err)
	}
}

func runPlugin() error {
	// Description must end with a dot (SDK validation).
	if err := plugin.RegisterMetrics(
		&impl,
		pluginName,
		metricKey,
		"Returns a JSON array of installed applications (registry uninstall keys).",
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

func runStandalone(verbose bool) (string, error) {
	apps, warns := collectInstalledApps()
	if verbose {
		fmt.Fprintf(os.Stderr, "collected %d application entries\n", len(apps))
		for _, w := range warns {
			fmt.Fprintf(os.Stderr, "warning: %v\n", w)
		}
	}

	out, err := marshalPowerShellJSON(apps)
	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "marshal error: %v\n", err)
		}
		return "[]", err
	}
	return out, nil
}

func (p *ApplicationInventoryPlugin) Export(key string, _ []string, _ plugin.ContextProvider) (interface{}, error) {
	if key != metricKey {
		return nil, errs.Errorf("unknown item key %q", key)
	}

	apps, warns := collectInstalledApps()
	for _, w := range warns {
		if p.Logger != nil {
			p.Infof("%s: %v", pluginName, w)
		}
	}

	out, err := marshalPowerShellJSON(apps)
	if err != nil {
		// Best-effort: if we cannot marshal, return empty array.
		if p.Logger != nil {
			p.Infof("%s: marshal error: %v", pluginName, err)
		}
		return "[]", nil
	}

	return out, nil
}

// --- Registry collection ---------------------------------------------------

type uninstallRoot struct {
	root     registry.Key
	hiveName string
	path     string
}

var uninstallRoots = []uninstallRoot{
	{registry.LOCAL_MACHINE, "HKEY_LOCAL_MACHINE", "Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall"},
	{registry.LOCAL_MACHINE, "HKEY_LOCAL_MACHINE", "Software\\WOW6432Node\\Microsoft\\Windows\\CurrentVersion\\Uninstall"},
	{registry.CURRENT_USER, "HKEY_CURRENT_USER", "Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall"},
}

func collectInstalledApps() ([]AppEntry, []error) {
	var all []AppEntry
	var warns []error

	for _, r := range uninstallRoots {
		apps, w := readUninstallRoot(r)
		all = append(all, apps...)
		warns = append(warns, w...)
	}

	// Sort by DisplayName like: Sort-Object DisplayName
	sort.SliceStable(all, func(i, j int) bool {
		a := strings.ToLower(all[i].DisplayName)
		b := strings.ToLower(all[j].DisplayName)
		if a == b {
			return all[i].DisplayName < all[j].DisplayName
		}
		return a < b
	})

	return all, warns
}

func readUninstallRoot(r uninstallRoot) ([]AppEntry, []error) {
	k, err := registry.OpenKey(r.root, r.path, registry.QUERY_VALUE|registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil, []error{fmt.Errorf("open %s\\%s: %w", r.hiveName, r.path, err)}
	}
	defer k.Close()

	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return nil, []error{fmt.Errorf("enumerate %s\\%s: %w", r.hiveName, r.path, err)}
	}

	apps := make([]AppEntry, 0, len(names))
	var warns []error

	for _, sub := range names {
		fullPath := r.path + "\\" + sub
		sk, err := registry.OpenKey(r.root, fullPath, registry.QUERY_VALUE)
		if err != nil {
			// access denied etc. -> skip
			warns = append(warns, fmt.Errorf("open %s\\%s: %w", r.hiveName, fullPath, err))
			continue
		}

		dn, _ := readTrimmedString(sk, "DisplayName")
		if dn == "" {
			sk.Close()
			continue
		}

		entry := AppEntry{
			DisplayName:     dn,
			Version:         readStringOrIntAsStringPtr(sk, "DisplayVersion"),
			Publisher:       readStringOrIntAsStringPtr(sk, "Publisher"),
			InstallDate:     readStringOrIntAsStringPtr(sk, "InstallDate"),
			InstallLocation: readStringOrIntAsStringPtr(sk, "InstallLocation"),
			RegistryPath:    buildPSRegistryPath(r.hiveName, fullPath),
		}
		apps = append(apps, entry)

		sk.Close()
	}

	return apps, warns
}

func buildPSRegistryPath(hiveName, keyPath string) string {
	// PowerShell's PSPath uses this prefix:
	//   Microsoft.PowerShell.Core\Registry::HKEY_LOCAL_MACHINE\Software\... (single backslashes in the raw string)
	return "Microsoft.PowerShell.Core\\Registry::" + hiveName + "\\" + keyPath
}

func readTrimmedString(k registry.Key, name string) (string, bool) {
	v, _, err := k.GetStringValue(name)
	if err != nil {
		return "", false
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	return v, true
}

func readStringOrIntAsStringPtr(k registry.Key, name string) *string {
	if s, _, err := k.GetStringValue(name); err == nil {
		// Keep empty strings as-is.
		return &s
	}
	if i, _, err := k.GetIntegerValue(name); err == nil {
		s := fmt.Sprintf("%d", i)
		return &s
	}
	return nil
}

// --- JSON formatting -------------------------------------------------------

func marshalPowerShellJSON(apps []AppEntry) (string, error) {
	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "    ")

	if err := enc.Encode(apps); err != nil {
		return "", err
	}

	b := buf.Bytes()
	// json.Encoder.Encode adds a trailing newline.
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}

	// PowerShell ConvertTo-Json uses two spaces after ':' (":  ")
	b = bytes.ReplaceAll(b, []byte("\": "), []byte("\":  "))

	// PowerShell uses CRLF on Windows.
	b = bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))

	return string(b), nil
}
