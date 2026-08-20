//go:build windows
// +build windows

package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
	"golang.zabbix.com/sdk/errs"
	"golang.zabbix.com/sdk/plugin"
	"golang.zabbix.com/sdk/plugin/container"
	"golang.zabbix.com/sdk/zbxerr"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unsafe"
)

const (
	pluginName = "WindowsServiceListeningPort"
	metricKey  = "service.listening.port"
)

type listeningPortEntry struct {
	Port        int    `json:"Port"`
	PID         int    `json:"PID"`
	Process     string `json:"Process"`
	ServiceName string `json:"ServiceName"`
	Description string `json:"Description"`
}

type impl struct {
	plugin.Base
}

var pluginImpl impl

func (p *impl) logf(format string, args ...interface{}) {
	if p.Logger != nil {
		p.Infof(format, args...)
	}
}

func (p *impl) Export(key string, params []string, _ plugin.ContextProvider) (interface{}, error) {
	if key != metricKey {
		return nil, errs.Wrapf(zbxerr.ErrorUnsupportedMetric, "unknown metric %q", key)
	}
	if len(params) != 0 {
		return nil, errs.Wrapf(zbxerr.ErrorTooManyParameters, "metric %q accepts no parameters", key)
	}

	out, err := collectListeningPorts()
	if err != nil {
		// Surface a genuine collection failure instead of an empty "[]" that reads
		// as "no listening ports" and hides the outage from the template trigger.
		p.logf("[%s] %v", pluginName, err)
		return nil, err
	}

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		p.logf("[%s] JSON marshal error: %v", pluginName, err)
		return nil, err
	}
	return string(b), nil
}

func main() {
	for _, a := range os.Args[1:] {
		if a == "--standalone" || a == "-standalone" || a == "standalone" {
			p := &impl{}
			v, err := p.Export(metricKey, nil, nil)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			fmt.Println(v)
			return
		}
	}

	if err := runPlugin(); err != nil {
		panic(err)
	}
}

func runPlugin() error {
	if err := plugin.RegisterMetrics(&pluginImpl, pluginName, metricKey, "Returns TCP listening ports with PID/process/service mapping as JSON."); err != nil {
		return err
	}

	h, err := container.NewHandler(pluginName)
	if err != nil {
		return err
	}

	pluginImpl.Logger = h
	return h.Execute()
}

// ---- TCP table via GetExtendedTcpTable ----

const (
	afInet  = 2
	afInet6 = 23

	tcpTableOwnerPidAll = 5 // TCP_TABLE_OWNER_PID_ALL

	errorInsufficientBuffer = 122 // ERROR_INSUFFICIENT_BUFFER
)

// getExtendedTcpTableBuf calls GetExtendedTcpTable for the given address family,
// retrying when the connection table grows between the size probe and the fetch
// (which then returns ERROR_INSUFFICIENT_BUFFER with an updated size). Without the
// retry a busy host intermittently returned a hard error / empty result.
func getExtendedTcpTableBuf(family uintptr, label string) ([]byte, error) {
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var size uint32
		_, _, _ = procGetExtendedTcpTable.Call(
			0,
			uintptr(unsafe.Pointer(&size)),
			1, // bOrder = TRUE
			family,
			tcpTableOwnerPidAll,
			0,
		)
		if size == 0 {
			return nil, fmt.Errorf("GetExtendedTcpTable returned empty buffer size for %s", label)
		}

		buf := make([]byte, size)
		r2, _, err := procGetExtendedTcpTable.Call(
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(unsafe.Pointer(&size)),
			1,
			family,
			tcpTableOwnerPidAll,
			0,
		)
		if r2 == errorInsufficientBuffer {
			// Table grew between the two calls; retry with the new required size.
			continue
		}
		if r2 != 0 {
			return nil, fmt.Errorf("GetExtendedTcpTable(%s) failed: %v (code=%d)", label, err, r2)
		}
		return buf, nil
	}
	return nil, fmt.Errorf("GetExtendedTcpTable(%s): buffer kept growing after %d attempts", label, maxAttempts)
}

type mibTCPRowOwnerPID struct {
	State      uint32
	LocalAddr  uint32
	LocalPort  uint32
	RemoteAddr uint32
	RemotePort uint32
	OwningPID  uint32
}

type mibTCP6RowOwnerPID struct {
	LocalAddr     [16]byte
	LocalScopeID  uint32
	LocalPort     uint32
	RemoteAddr    [16]byte
	RemoteScopeID uint32
	RemotePort    uint32
	State         uint32
	OwningPID     uint32
}

var (
	iphlpapi                = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable = iphlpapi.NewProc("GetExtendedTcpTable")
)

func getTCPTableOwnerPIDAllIPv4() ([]mibTCPRowOwnerPID, error) {
	buf, err := getExtendedTcpTableBuf(afInet, "IPv4")
	if err != nil {
		return nil, err
	}

	if len(buf) < 4 {
		return nil, fmt.Errorf("unexpected IPv4 tcp table buffer")
	}
	num := binary.LittleEndian.Uint32(buf[0:4])
	rows := make([]mibTCPRowOwnerPID, 0, num)

	rowSize := int(unsafe.Sizeof(mibTCPRowOwnerPID{}))
	offset := 4
	for i := uint32(0); i < num; i++ {
		if offset+rowSize > len(buf) {
			break
		}
		row := *(*mibTCPRowOwnerPID)(unsafe.Pointer(&buf[offset]))
		rows = append(rows, row)
		offset += rowSize
	}
	return rows, nil
}

func getTCPTableOwnerPIDAllIPv6() ([]mibTCP6RowOwnerPID, error) {
	buf, err := getExtendedTcpTableBuf(afInet6, "IPv6")
	if err != nil {
		return nil, err
	}

	if len(buf) < 4 {
		return nil, fmt.Errorf("unexpected IPv6 tcp table buffer")
	}
	num := binary.LittleEndian.Uint32(buf[0:4])
	rows := make([]mibTCP6RowOwnerPID, 0, num)

	rowSize := int(unsafe.Sizeof(mibTCP6RowOwnerPID{}))
	offset := 4
	for i := uint32(0); i < num; i++ {
		if offset+rowSize > len(buf) {
			break
		}
		row := *(*mibTCP6RowOwnerPID)(unsafe.Pointer(&buf[offset]))
		rows = append(rows, row)
		offset += rowSize
	}
	return rows, nil
}

func ntohs(n uint16) uint16 {
	return (n<<8)&0xff00 | (n >> 8)
}

func tcpPort(dwPort uint32) int {
	return int(ntohs(uint16(dwPort)))
}

// ---- Process/service helpers ----

var excludedProcesses = map[string]struct{}{
	"idle":     {},
	"system":   {},
	"lsass":    {},
	"csrss":    {},
	"smss":     {},
	"wininit":  {},
	"services": {},
	"winlogon": {},
	"svchost":  {},
}

type manualPortInfo struct {
	ServiceName string
	Description string
}

// DisplayName is intentionally not included because the output must match the scripts
// (Port, PID, Process, ServiceName, Description).
var manualPortMap = map[int]manualPortInfo{
	// IIS / HTTP.sys
	80:  {ServiceName: "W3SVC", Description: "HTTP (IIS)"},
	443: {ServiceName: "W3SVC", Description: "HTTPS (IIS)"},

	// WinRM
	5985: {ServiceName: "WinRM", Description: "WinRM (HTTP)"},
	5986: {ServiceName: "WinRM", Description: "WinRM (HTTPS)"},

	// WSUS
	8530: {ServiceName: "WsusService", Description: "WSUS (HTTP)"},
	8531: {ServiceName: "WsusService", Description: "WSUS (HTTPS)"},

	// SQL Server
	1433: {ServiceName: "MSSQLSERVER", Description: "SQL Server default instance"},

	// AD DS
	389:  {ServiceName: "NTDS", Description: "LDAP"},
	636:  {ServiceName: "NTDS", Description: "LDAPS (SSL)"},
	3268: {ServiceName: "NTDS", Description: "Global Catalog (unencrypted)"},
	3269: {ServiceName: "NTDS", Description: "Global Catalog (SSL)"},
	88:   {ServiceName: "NTDS", Description: "Kerberos KDC"},
	135:  {ServiceName: "RpcSs", Description: "RPC Endpoint Mapper"},

	// SMB / NetBIOS / File & Print
	445: {ServiceName: "LanmanServer", Description: "SMB/CIFS"},
	137: {ServiceName: "LanmanServer", Description: "NetBIOS Name Service"},
	138: {ServiceName: "LanmanServer", Description: "NetBIOS Datagram Service"},
	139: {ServiceName: "LanmanServer", Description: "NetBIOS Session Service"},

	// DNS
	53: {ServiceName: "DNS", Description: "DNS"},

	// DHCP (UDP 67, included for parity with the script even if not returned by TCP table)
	67: {ServiceName: "DHCPServer", Description: "DHCP (UDP 67)"},

	// RDS
	3389: {ServiceName: "TermService", Description: "RDP"},
}

type portPID struct {
	Port int
	PID  uint32
}

func collectListeningPorts() ([]listeningPortEntry, error) {
	// 1) Collect listening TCP port+pid pairs (IPv4 + IPv6).
	listenSet := make(map[portPID]struct{})

	var hadAnyTable bool
	var errs []error

	if rows, err := getTCPTableOwnerPIDAllIPv4(); err == nil {
		hadAnyTable = true
		for _, r := range rows {
			if r.State != mibTCPStateListen {
				continue
			}
			port := tcpPort(r.LocalPort)
			if port <= 0 || port > 65535 {
				continue
			}
			listenSet[portPID{Port: port, PID: r.OwningPID}] = struct{}{}
		}
	} else {
		errs = append(errs, err)
	}

	if rows, err := getTCPTableOwnerPIDAllIPv6(); err == nil {
		hadAnyTable = true
		for _, r := range rows {
			if r.State != mibTCPStateListen {
				continue
			}
			port := tcpPort(r.LocalPort)
			if port <= 0 || port > 65535 {
				continue
			}
			listenSet[portPID{Port: port, PID: r.OwningPID}] = struct{}{}
		}
	} else {
		errs = append(errs, err)
	}

	if !hadAnyTable {
		if len(errs) == 1 {
			return nil, errs[0]
		}
		return nil, fmt.Errorf("failed to read tcp tables: %v", errs)
	}

	pairs := make([]portPID, 0, len(listenSet))
	for k := range listenSet {
		pairs = append(pairs, k)
	}

	// 2) Build PID -> service name map (best effort).
	serviceByPID := buildServicePIDMap()

	// 3) Build auto and manual records
	auto := make([]listeningPortEntry, 0, len(pairs))
	manual := make([]listeningPortEntry, 0, len(pairs))

	for _, pp := range pairs {
		pid := int(pp.PID)

		// Process name/path (best effort)
		procName, exePath, procErr := getProcessNameAndPath(pid)

		// Auto record: skip if process can't be resolved or is excluded.
		if procErr == nil && procName != "" && !isExcluded(procName) {
			svcName := serviceByPID[pp.PID]
			desc := "N/A"
			if exePath != "" {
				if d, err := fileDescription(exePath); err == nil && strings.TrimSpace(d) != "" {
					desc = d
				}
			}
			auto = append(auto, listeningPortEntry{
				Port:        pp.Port,
				PID:         pid,
				Process:     procName,
				ServiceName: svcName,
				Description: desc,
			})
		}

		// Manual record for known ports (always included when port matches).
		if mp, ok := manualPortMap[pp.Port]; ok {
			manualProc := procName
			if pp.PID == 4 {
				manualProc = "HTTP.sys (kernel)"
			}
			if manualProc == "" {
				manualProc = "unknown"
			}
			manual = append(manual, listeningPortEntry{
				Port:        pp.Port,
				PID:         pid,
				Process:     manualProc,
				ServiceName: mp.ServiceName,
				Description: mp.Description,
			})
		}
	}

	all := append(auto, manual...)

	// 4) Sort by Port,PID,Process (stable) and remove duplicates by Port,PID,Process.
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Port != all[j].Port {
			return all[i].Port < all[j].Port
		}
		if all[i].PID != all[j].PID {
			return all[i].PID < all[j].PID
		}
		return all[i].Process < all[j].Process
	})

	type uniqKey struct {
		Port    int
		PID     int
		Process string
	}

	uniq := make([]listeningPortEntry, 0, len(all))
	var last *uniqKey
	for _, e := range all {
		k := uniqKey{Port: e.Port, PID: e.PID, Process: e.Process}
		if last != nil && k == *last {
			continue
		}
		uniq = append(uniq, e)
		last = &k
	}

	return uniq, nil
}

const (
	mibTCPStateListen = 2
)

func isExcluded(name string) bool {
	_, ok := excludedProcesses[strings.ToLower(name)]
	return ok
}

func getProcessNameAndPath(pid int) (string, string, error) {
	// Special-case PID 4 to match PowerShell behavior (manual mapping uses HTTP.sys (kernel)).
	if pid == 4 {
		return "System", "", nil
	}

	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", "", err
	}
	defer windows.CloseHandle(h)

	// QueryFullProcessImageName needs a buffer; grow if needed.
	size := uint32(260)
	for {
		buf := make([]uint16, size)
		n := size
		err = windows.QueryFullProcessImageName(h, 0, &buf[0], &n)
		if err == nil {
			fullPath := windows.UTF16ToString(buf[:n])
			base := filepath.Base(fullPath)
			name := strings.TrimSuffix(base, filepath.Ext(base))
			return name, fullPath, nil
		}

		if err == windows.ERROR_INSUFFICIENT_BUFFER {
			size *= 2
			if size > 32768 {
				return "", "", err
			}
			continue
		}
		return "", "", err
	}
}

func buildServicePIDMap() map[uint32]string {
	out := make(map[uint32]string)

	m, err := mgr.Connect()
	if err != nil {
		return out
	}
	defer m.Disconnect()

	names, err := m.ListServices()
	if err != nil {
		return out
	}

	for _, name := range names {
		s, err := m.OpenService(name)
		if err != nil {
			continue
		}
		st, err := s.Query()
		_ = s.Close()
		if err != nil {
			continue
		}

		// Only services with a process id are useful here.
		if st.ProcessId == 0 {
			continue
		}
		// Keep first service name for a PID (good enough for non-svchost processes).
		if _, ok := out[st.ProcessId]; ok {
			continue
		}

		// Ignore stopped services.
		if st.State == svc.Stopped {
			continue
		}

		out[st.ProcessId] = name
	}
	return out
}

// ---- FileDescription (Version resource) ----

var (
	versionDLL                 = windows.NewLazySystemDLL("version.dll")
	procGetFileVersionInfoSize = versionDLL.NewProc("GetFileVersionInfoSizeW")
	procGetFileVersionInfo     = versionDLL.NewProc("GetFileVersionInfoW")
	procVerQueryValue          = versionDLL.NewProc("VerQueryValueW")
)

type langAndCodePage struct {
	Language uint16
	CodePage uint16
}

func fileDescription(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}

	var handle uint32
	r1, _, e1 := procGetFileVersionInfoSize.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&handle)),
	)
	if r1 == 0 {
		return "", fmt.Errorf("GetFileVersionInfoSizeW: %v", e1)
	}
	size := uint32(r1)
	if size == 0 {
		return "", fmt.Errorf("GetFileVersionInfoSizeW: size=0")
	}

	buf := make([]byte, size)
	r2, _, e2 := procGetFileVersionInfo.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(handle),
		uintptr(size),
		uintptr(unsafe.Pointer(&buf[0])),
	)
	if r2 == 0 {
		return "", fmt.Errorf("GetFileVersionInfoW: %v", e2)
	}

	// Translation block (language + codepage).
	var transPtr uintptr
	var transLen uint32
	transBlock, _ := windows.UTF16PtrFromString(`\VarFileInfo\Translation`)
	r3, _, _ := procVerQueryValue.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(transBlock)),
		uintptr(unsafe.Pointer(&transPtr)),
		uintptr(unsafe.Pointer(&transLen)),
	)

	lang := uint16(0x0409)
	codepage := uint16(0x04B0)
	if r3 != 0 && transPtr != 0 && transLen >= 4 {
		lc := *(*langAndCodePage)(unsafe.Pointer(transPtr))
		if lc.Language != 0 {
			lang = lc.Language
		}
		if lc.CodePage != 0 {
			codepage = lc.CodePage
		}
	}

	// Query FileDescription in detected language/codepage.
	query := fmt.Sprintf(`\StringFileInfo\%04x%04x\FileDescription`, lang, codepage)
	desc, err := verQueryString(buf, query)
	if err == nil && strings.TrimSpace(desc) != "" {
		return strings.TrimSpace(desc), nil
	}

	// Fallback to en-US Unicode.
	desc, err = verQueryString(buf, `\StringFileInfo\040904b0\FileDescription`)
	if err != nil {
		return "", err
	}
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return "", fmt.Errorf("empty FileDescription")
	}
	return desc, nil
}

func verQueryString(buf []byte, subBlock string) (string, error) {
	sb, err := windows.UTF16PtrFromString(subBlock)
	if err != nil {
		return "", err
	}
	var valuePtr uintptr
	var valueLen uint32
	r, _, _ := procVerQueryValue.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(sb)),
		uintptr(unsafe.Pointer(&valuePtr)),
		uintptr(unsafe.Pointer(&valueLen)),
	)
	if r == 0 || valuePtr == 0 || valueLen == 0 {
		return "", fmt.Errorf("VerQueryValueW failed for %q", subBlock)
	}
	// valueLen is the string length in WCHARs (including the trailing NUL). Bound
	// the read to it: the version resource belongs to the target process's image
	// and may be malformed/attacker-influenced, so reading a NUL-terminated string
	// without a length cap risks an out-of-bounds read past the resource buffer.
	u16 := unsafe.Slice((*uint16)(unsafe.Pointer(valuePtr)), valueLen)
	return windows.UTF16ToString(u16), nil
}
