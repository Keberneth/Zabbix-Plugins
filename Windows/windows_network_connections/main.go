//go:build windows
// +build windows

package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"golang.org/x/sys/windows"
	"golang.zabbix.com/sdk/plugin"
	"golang.zabbix.com/sdk/plugin/container"
	"net"
	"os"
	"sort"
	"strconv"
	"time"
	"unsafe"
)

const (
	pluginName = "WindowsNetworkConnections"
	metricKey  = "windows-network-connections"
)

type openPort struct {
	Port string `json:"port"`
}

type incomingConn struct {
	LocalIP   string `json:"localip"`
	LocalPort string `json:"localport"`
	RemoteIP  string `json:"remoteip"`
	Count     string `json:"count"`
}

type outgoingConn struct {
	LocalIP    string `json:"localip"`
	RemoteIP   string `json:"remoteip"`
	RemotePort string `json:"remoteport"`
	Count      string `json:"count"`
}

type payload struct {
	OpenPorts           []openPort     `json:"openports"`
	IncomingConnections []incomingConn `json:"incomingconnections"`
	OutgoingConnections []outgoingConn `json:"outgoingconnections"`
	Timestamp           string         `json:"timestamp"`
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
		return nil, plugin.UnsupportedMetricError
	}

	out, err := buildPayload()
	if err != nil {
		p.logf("[%s] %v", pluginName, err)
		// Script prints JSON even if empty
		return `{"openports":[],"incomingconnections":[],"outgoingconnections":[],"timestamp":"0"}`, nil
	}

	b, err := json.Marshal(out)
	if err != nil {
		p.logf("[%s] JSON marshal error: %v", pluginName, err)
		return `{"openports":[],"incomingconnections":[],"outgoingconnections":[],"timestamp":"0"}`, nil
	}
	return string(b), nil
}

func main() {
	for _, a := range os.Args[1:] {
		if a == "--standalone" || a == "-standalone" || a == "standalone" {
			p := &impl{}
			v, _ := p.Export(metricKey, nil, nil)
			fmt.Println(v)
			os.Exit(0)
		}
	}

	if err := runPlugin(); err != nil {
		panic(err)
	}
}

func runPlugin() error {
	if err := plugin.RegisterMetrics(&pluginImpl, pluginName, metricKey, "Returns Windows TCP listening ports and established connections as JSON."); err != nil {
		return err
	}

	h, err := container.NewHandler(pluginName)
	if err != nil {
		return err
	}

	pluginImpl.Logger = h
	return h.Execute()
}

// ---- TCP table (IPv4) via GetExtendedTcpTable ----

const (
	afInet = 2

	tcpTableOwnerPidAll = 5 // TCP_TABLE_OWNER_PID_ALL
)

type mibTCPRowOwnerPID struct {
	State      uint32
	LocalAddr  uint32
	LocalPort  uint32
	RemoteAddr uint32
	RemotePort uint32
	OwningPID  uint32
}

var (
	iphlpapi                = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable = iphlpapi.NewProc("GetExtendedTcpTable")
)

func getTCPTableOwnerPIDAllIPv4() ([]mibTCPRowOwnerPID, error) {
	var size uint32
	// First call to get required buffer size
	r1, _, _ := procGetExtendedTcpTable.Call(
		0,
		uintptr(unsafe.Pointer(&size)),
		1, // bOrder = TRUE
		afInet,
		tcpTableOwnerPidAll,
		0,
	)
	// ERROR_INSUFFICIENT_BUFFER (122) expected
	_ = r1

	buf := make([]byte, size)
	r2, _, err := procGetExtendedTcpTable.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
		1,
		afInet,
		tcpTableOwnerPidAll,
		0,
	)
	if r2 != 0 {
		return nil, fmt.Errorf("GetExtendedTcpTable failed: %v (code=%d)", err, r2)
	}

	if len(buf) < 4 {
		return nil, fmt.Errorf("unexpected tcp table buffer")
	}
	num := binary.LittleEndian.Uint32(buf[0:4])
	rows := make([]mibTCPRowOwnerPID, 0, num)

	// Rows start immediately after dwNumEntries (DWORD).
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

func ntohs(n uint16) uint16 {
	return (n<<8)&0xff00 | (n>>8)&0x00ff
}

func ipFromDWORD(netOrder uint32) net.IP {
	b := make([]byte, 4)
	// dwLocalAddr/dwRemoteAddr are in network byte order.
	binary.BigEndian.PutUint32(b, netOrder)
	return net.IPv4(b[0], b[1], b[2], b[3])
}

func tcpPort(dwPort uint32) int {
	return int(ntohs(uint16(dwPort)))
}

// ---- Payload generation ----

const (
	mibTCPStateListen      = 2
	mibTCPStateEstablished = 5
)

func buildPayload() (*payload, error) {
	rows, err := getTCPTableOwnerPIDAllIPv4()
	if err != nil {
		return nil, err
	}

	listeningPorts := map[int]struct{}{}
	for _, r := range rows {
		if r.State != mibTCPStateListen {
			continue
		}
		localIP := ipFromDWORD(r.LocalAddr).String()
		if localIP == "127.0.0.1" {
			continue
		}
		port := tcpPort(r.LocalPort)
		if port <= 0 {
			continue
		}
		listeningPorts[port] = struct{}{}
	}

	// Open ports output
	ports := make([]int, 0, len(listeningPorts))
	for p := range listeningPorts {
		ports = append(ports, p)
	}
	sort.Ints(ports)

	openPorts := make([]openPort, 0, len(ports))
	for _, p := range ports {
		openPorts = append(openPorts, openPort{Port: strconv.Itoa(p)})
	}

	type inKey struct {
		localIP   string
		localPort int
		remoteIP  string
	}
	type outKey struct {
		localIP    string
		remoteIP   string
		remotePort int
	}

	inCounts := map[inKey]int{}
	outCounts := map[outKey]int{}

	for _, r := range rows {
		if r.State != mibTCPStateEstablished {
			continue
		}

		localIP := ipFromDWORD(r.LocalAddr).String()
		remoteIP := ipFromDWORD(r.RemoteAddr).String()

		if localIP == "127.0.0.1" || remoteIP == "127.0.0.1" {
			continue
		}

		lp := tcpPort(r.LocalPort)
		rp := tcpPort(r.RemotePort)

		if _, ok := listeningPorts[lp]; ok {
			k := inKey{localIP: localIP, localPort: lp, remoteIP: remoteIP}
			inCounts[k]++
		} else {
			k := outKey{localIP: localIP, remoteIP: remoteIP, remotePort: rp}
			outCounts[k]++
		}
	}

	incoming := make([]incomingConn, 0, len(inCounts))
	for k, c := range inCounts {
		incoming = append(incoming, incomingConn{
			LocalIP:   k.localIP,
			LocalPort: strconv.Itoa(k.localPort),
			RemoteIP:  k.remoteIP,
			Count:     strconv.Itoa(c),
		})
	}
	sort.SliceStable(incoming, func(i, j int) bool {
		if incoming[i].LocalIP != incoming[j].LocalIP {
			return incoming[i].LocalIP < incoming[j].LocalIP
		}
		if incoming[i].LocalPort != incoming[j].LocalPort {
			return incoming[i].LocalPort < incoming[j].LocalPort
		}
		return incoming[i].RemoteIP < incoming[j].RemoteIP
	})

	outgoing := make([]outgoingConn, 0, len(outCounts))
	for k, c := range outCounts {
		outgoing = append(outgoing, outgoingConn{
			LocalIP:    k.localIP,
			RemoteIP:   k.remoteIP,
			RemotePort: strconv.Itoa(k.remotePort),
			Count:      strconv.Itoa(c),
		})
	}
	sort.SliceStable(outgoing, func(i, j int) bool {
		if outgoing[i].LocalIP != outgoing[j].LocalIP {
			return outgoing[i].LocalIP < outgoing[j].LocalIP
		}
		if outgoing[i].RemoteIP != outgoing[j].RemoteIP {
			return outgoing[i].RemoteIP < outgoing[j].RemoteIP
		}
		return outgoing[i].RemotePort < outgoing[j].RemotePort
	})

	ts := strconv.FormatInt(time.Now().UnixNano(), 10)

	return &payload{
		OpenPorts:           openPorts,
		IncomingConnections: incoming,
		OutgoingConnections: outgoing,
		Timestamp:           ts,
	}, nil
}
