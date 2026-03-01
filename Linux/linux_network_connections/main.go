//go:build linux
// +build linux

package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.zabbix.com/sdk/errs"
	"golang.zabbix.com/sdk/plugin"
	"golang.zabbix.com/sdk/plugin/container"
)

const (
	pluginName = "LinuxNetworkConnections"
	itemKey    = "linux-network-connections"
)

type LinuxNetworkConnectionsPlugin struct {
	plugin.Base
}

var impl LinuxNetworkConnectionsPlugin

var _ plugin.Exporter = (*LinuxNetworkConnectionsPlugin)(nil)

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

type output struct {
	OpenPorts           []openPort    `json:"openports"`
	IncomingConnections []incomingConn `json:"incomingconnections"`
	OutgoingConnections []outgoingConn `json:"outgoingconnections"`
	Timestamp           string        `json:"timestamp"`
}

func main() {
	// Standalone mode for debugging:
	//   ./zabbix-agent2-linux-network-connections --standalone
	for _, a := range os.Args[1:] {
		if a == "--standalone" || a == "-standalone" || a == "standalone" {
			out, err := collect()
			if err != nil {
				fmt.Printf("ERROR: %v\n", err)
				os.Exit(1)
			}
			b, _ := json.Marshal(out)
			fmt.Println(string(b))
			return
		}
	}

	if err := runPlugin(); err != nil {
		panic(err)
	}
}

func runPlugin() error {
	// description must end with '.' per SDK validation
	if err := plugin.RegisterMetrics(
		&impl,
		pluginName,
		itemKey,
		"Returns open ports and TCP connection statistics in JSON.",
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

func (p *LinuxNetworkConnectionsPlugin) logf(format string, args ...interface{}) {
	if p.Logger != nil {
		p.Infof(format, args...)
	}
}

func (p *LinuxNetworkConnectionsPlugin) Export(key string, params []string, ctx plugin.ContextProvider) (interface{}, error) {
	if key != itemKey {
		return nil, plugin.UnsupportedMetricError
	}

	out, err := collect()
	if err != nil {
		p.logf("[%s] %v", pluginName, err)
		return `{"openports":[],"incomingconnections":[],"outgoingconnections":[],"timestamp":"0"}`, nil
	}

	// Return JSON string to match the original script output style.
	b, err := json.Marshal(out)
	if err != nil {
		p.logf("[%s] JSON marshal error: %v", pluginName, err)
		return `{"openports":[],"incomingconnections":[],"outgoingconnections":[],"timestamp":"0"}`, nil
	}
	return string(b), nil
}

// -------------------- collection logic --------------------

func collect() (*output, error) {
	internalNets := parseInternalNets(os.Getenv("LNC_INTERNAL_NETS"))

	// Open ports: replicate "ss -4 --listen" behavior broadly using /proc
	listenPorts, err := collectListeningPortsV4()
	if err != nil {
		return nil, err
	}

	// Connected TCP flows: replicate "ss ... state CONNECTED" using /proc
	inAgg, outAgg, err := collectTcpConnectedAggV4(listenPorts, internalNets)
	if err != nil {
		return nil, err
	}

	// Build output arrays
	openPorts := make([]openPort, 0, len(listenPorts))
	portNums := make([]int, 0, len(listenPorts))
	for p := range listenPorts {
		portNums = append(portNums, p)
	}
	sort.Ints(portNums)
	for _, pn := range portNums {
		openPorts = append(openPorts, openPort{Port: strconv.Itoa(pn)})
	}

	incoming := make([]incomingConn, 0, len(inAgg))
	for _, k := range sortedIncomingKeys(inAgg) {
		v := inAgg[k]
		parts := strings.Split(k, "|")
		incoming = append(incoming, incomingConn{
			Count:     strconv.Itoa(v),
			LocalIP:   parts[0],
			LocalPort: parts[1],
			RemoteIP:  parts[2],
		})
	}

	outgoing := make([]outgoingConn, 0, len(outAgg))
	for _, k := range sortedOutgoingKeys(outAgg) {
		v := outAgg[k]
		parts := strings.Split(k, "|")
		outgoing = append(outgoing, outgoingConn{
			Count:      strconv.Itoa(v),
			LocalIP:    parts[0],
			RemoteIP:   parts[1],
			RemotePort: parts[2],
		})
	}

	return &output{
		OpenPorts:           openPorts,
		IncomingConnections: incoming,
		OutgoingConnections: outgoing,
		Timestamp:           strconv.FormatInt(time.Now().UnixNano(), 10),
	}, nil
}

// parseInternalNets parses env var LNC_INTERNAL_NETS like:
//   "0.0.0.0/0" (default)
//   "10.0.0.0/8 192.168.0.0/16"
//
// NOTE: This matches the original script's behavior where the ss filter used "src = <net>"
// which refers to the local address. We apply the CIDR check to the local IP.
func parseInternalNets(s string) []*net.IPNet {
	s = strings.TrimSpace(s)
	if s == "" {
		s = "0.0.0.0/0"
	}
	parts := strings.Fields(s)

	var nets []*net.IPNet
	for _, p := range parts {
		if strings.Contains(p, "/") {
			_, n, err := net.ParseCIDR(p)
			if err == nil && n != nil {
				nets = append(nets, n)
				continue
			}
		}
		// allow plain IP (treated as /32)
		ip := net.ParseIP(p)
		if ip == nil {
			continue
		}
		if ip4 := ip.To4(); ip4 != nil {
			_, n, _ := net.ParseCIDR(ip4.String() + "/32")
			nets = append(nets, n)
		}
	}
	if len(nets) == 0 {
		_, n, _ := net.ParseCIDR("0.0.0.0/0")
		nets = []*net.IPNet{n}
	}
	return nets
}

func ipAllowed(ipStr string, nets []*net.IPNet) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	ip = ip.To4()
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func collectListeningPortsV4() (map[int]struct{}, error) {
	ports := map[int]struct{}{}

	// TCP LISTEN
	if err := parseProcNetV4("/proc/net/tcp", func(localIP string, localPort int, remoteIP string, remotePort int, state string) {
		if state == "0A" { // LISTEN
			ports[localPort] = struct{}{}
		}
	}); err != nil {
		return nil, err
	}

	// UDP "listening" (no LISTEN state; include any local port present)
	_ = parseProcNetV4("/proc/net/udp", func(localIP string, localPort int, remoteIP string, remotePort int, state string) {
		// keep consistent with "ss --listen": UDP sockets shown under LISTEN
		ports[localPort] = struct{}{}
	})

	return ports, nil
}

// collectTcpConnectedAggV4 reads /proc/net/tcp and groups "CONNECTED" TCP sockets into incoming/outgoing
// based on whether the local port is a listening port.
func collectTcpConnectedAggV4(listenPorts map[int]struct{}, internalNets []*net.IPNet) (map[string]int, map[string]int, error) {
	incoming := map[string]int{}
	outgoing := map[string]int{}

	connectedStates := map[string]struct{}{
		"01": {}, // ESTABLISHED
		"02": {}, // SYN_SENT
		"03": {}, // SYN_RECV
		"04": {}, // FIN_WAIT1
		"05": {}, // FIN_WAIT2
		"06": {}, // TIME_WAIT
		"08": {}, // CLOSE_WAIT
		"09": {}, // LAST_ACK
		"0B": {}, // CLOSING
	}

	err := parseProcNetV4("/proc/net/tcp", func(localIP string, localPort int, remoteIP string, remotePort int, state string) {
		if _, ok := connectedStates[state]; !ok {
			return
		}
		if !ipAllowed(localIP, internalNets) {
			return
		}

		if _, isListen := listenPorts[localPort]; isListen {
			k := fmt.Sprintf("%s|%d|%s", localIP, localPort, remoteIP)
			incoming[k]++
		} else {
			k := fmt.Sprintf("%s|%s|%d", localIP, remoteIP, remotePort)
			outgoing[k]++
		}
	})
	if err != nil {
		return nil, nil, err
	}

	return incoming, outgoing, nil
}

// parseProcNetV4 parses /proc/net/{tcp,udp} IPv4 tables.
// callback receives decoded local/remote ip+port and the state hex string (e.g. "01", "0A").
func parseProcNetV4(path string, cb func(localIP string, localPort int, remoteIP string, remotePort int, state string)) error {
	f, err := os.Open(path)
	if err != nil {
		// UDP may not exist on minimal systems; ignore those.
		if strings.Contains(path, "/udp") && os.IsNotExist(err) {
			return nil
		}
		return errs.Wrap(err, "failed to open "+path)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if first {
			first = false
			continue // header
		}

		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		localIP, localPort, ok1 := decodeProcAddrPortV4(fields[1])
		remoteIP, remotePort, ok2 := decodeProcAddrPortV4(fields[2])
		if !ok1 || !ok2 {
			continue
		}

		state := strings.ToUpper(fields[3])
		cb(localIP, localPort, remoteIP, remotePort, state)
	}
	if err := sc.Err(); err != nil {
		return errs.Wrap(err, "failed to scan "+path)
	}
	return nil
}

func decodeProcAddrPortV4(s string) (ip string, port int, ok bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return "", 0, false
	}

	ipHex := parts[0]
	portHex := parts[1]

	ipb, err := hex.DecodeString(ipHex)
	if err != nil || len(ipb) != 4 {
		return "", 0, false
	}
	// /proc uses little-endian for IPv4
	ip = net.IPv4(ipb[3], ipb[2], ipb[1], ipb[0]).String()

	p64, err := strconv.ParseInt(portHex, 16, 32)
	if err != nil {
		return "", 0, false
	}
	port = int(p64)
	return ip, port, true
}

func sortedIncomingKeys(m map[string]int) []string {
	type kParts struct {
		key      string
		localIP  string
		localPrt int
		remoteIP string
	}
	keys := make([]kParts, 0, len(m))
	for k := range m {
		p := strings.Split(k, "|")
		if len(p) != 3 {
			continue
		}
		lp, _ := strconv.Atoi(p[1])
		keys = append(keys, kParts{key: k, localIP: p[0], localPrt: lp, remoteIP: p[2]})
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].localIP != keys[j].localIP {
			return keys[i].localIP < keys[j].localIP
		}
		if keys[i].localPrt != keys[j].localPrt {
			return keys[i].localPrt < keys[j].localPrt
		}
		return keys[i].remoteIP < keys[j].remoteIP
	})
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.key)
	}
	return out
}

func sortedOutgoingKeys(m map[string]int) []string {
	type kParts struct {
		key       string
		localIP   string
		remoteIP  string
		remotePrt int
	}
	keys := make([]kParts, 0, len(m))
	for k := range m {
		p := strings.Split(k, "|")
		if len(p) != 3 {
			continue
		}
		rp, _ := strconv.Atoi(p[2])
		keys = append(keys, kParts{key: k, localIP: p[0], remoteIP: p[1], remotePrt: rp})
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].localIP != keys[j].localIP {
			return keys[i].localIP < keys[j].localIP
		}
		if keys[i].remoteIP != keys[j].remoteIP {
			return keys[i].remoteIP < keys[j].remoteIP
		}
		return keys[i].remotePrt < keys[j].remotePrt
	})
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.key)
	}
	return out
}
