//go:build linux
// +build linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"golang.zabbix.com/sdk/plugin"
	"golang.zabbix.com/sdk/plugin/container"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	pluginName = "LinuxServiceListeningPort"
	metricKey  = "service.listening.port"
)

type listeningPortEntry struct {
	Port        int   `json:"Port"`
	PID         *int  `json:"PID"`
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
		return nil, plugin.UnsupportedMetricError
	}

	out, err := collectListeningPorts()
	if err != nil {
		p.logf("[%s] %v", pluginName, err)
		return "[]", nil
	}

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		p.logf("[%s] JSON marshal error: %v", pluginName, err)
		return "[]", nil
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
	if err := plugin.RegisterMetrics(&pluginImpl, pluginName, metricKey, "Returns TCP listening ports with PID/process/systemd-unit mapping as JSON."); err != nil {
		return err
	}

	h, err := container.NewHandler(pluginName)
	if err != nil {
		return err
	}

	pluginImpl.Logger = h
	return h.Execute()
}

type uniqKey struct {
	Port    int
	PID     int
	HasPID  bool
	Process string
}

func collectListeningPorts() ([]listeningPortEntry, error) {
	// Manual fallback for known ports (matches your script).
	manualPortMap := map[int]string{
		22:    "SSH",
		3306:  "MySQL",
		5432:  "PostgreSQL",
		6379:  "Redis",
		27017: "MongoDB",
	}

	// ss users() format is typically: users:(("proc",pid=123,fd=3))
	reUser := regexp.MustCompile(`\("([^"]+)",pid=(\d+)`)

	cmd := exec.Command("ss", "-tnlp")
	outBytes, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to run ss -tnlp: %w", err)
	}

	lines := strings.Split(string(outBytes), "\n")

	entriesByKey := make(map[uniqKey]listeningPortEntry)

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		// Header line often starts with "State"
		if strings.HasPrefix(line, "State") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if fields[0] != "LISTEN" {
			continue
		}

		port, err := parsePort(fields[3])
		if err != nil || port <= 0 || port > 65535 {
			continue
		}

		var pidPtr *int
		proc := ""

		if m := reUser.FindStringSubmatch(line); len(m) == 3 {
			proc = m[1]
			if pid, convErr := strconv.Atoi(m[2]); convErr == nil {
				pidPtr = &pid
			}
		}
		if proc == "" {
			proc = "unknown"
		}

		serviceName := ""
		if pidPtr != nil {
			serviceName = systemdUnitFromCgroup(*pidPtr)
		}

		description := ""
		// Detect web services by unit name (matches your script behavior).
		if strings.Contains(serviceName, "nginx") {
			description = "NGINX Web Server"
		} else if strings.Contains(serviceName, "apache2") || strings.Contains(serviceName, "httpd") {
			description = "Apache HTTP Server"
		}


		// Fallback to manual port map.
		if description == "" {
			if v, ok := manualPortMap[port]; ok {
				description = v
			}
		}
		if description == "" {
			description = "N/A"
		}

		e := listeningPortEntry{
			Port:        port,
			PID:         pidPtr,
			Process:     proc,
			ServiceName: serviceName,
			Description: description,
		}

		k := uniqKey{Port: port, Process: proc}
		if pidPtr != nil {
			k.HasPID = true
			k.PID = *pidPtr
		}
		if _, exists := entriesByKey[k]; !exists {
			entriesByKey[k] = e
		}
	}

	all := make([]listeningPortEntry, 0, len(entriesByKey))
	for _, v := range entriesByKey {
		all = append(all, v)
	}

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Port != all[j].Port {
			return all[i].Port < all[j].Port
		}
		pi, okI := pidValue(all[i].PID)
		pj, okJ := pidValue(all[j].PID)

		// Non-nil PID first, nil last (stable).
		if okI != okJ {
			return okI
		}
		if okI && pi != pj {
			return pi < pj
		}
		return all[i].Process < all[j].Process
	})

	return all, nil
}

func pidValue(p *int) (int, bool) {
	if p == nil {
		return 0, false
	}
	return *p, true
}

func parsePort(local string) (int, error) {
	// local is typically "0.0.0.0:22", "[::]:80", "*:443"
	idx := strings.LastIndex(local, ":")
	if idx == -1 || idx == len(local)-1 {
		return 0, fmt.Errorf("invalid local address: %q", local)
	}
	portStr := local[idx+1:]
	return strconv.Atoi(portStr)
}

func systemdUnitFromCgroup(pid int) string {
	path := fmt.Sprintf("/proc/%d/cgroup", pid)
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		// Typical format: 0::/system.slice/sshd.service
		if strings.Contains(line, "system.slice") && strings.Contains(line, ".service") {
			parts := strings.Split(line, "/")
			if len(parts) > 0 {
				last := parts[len(parts)-1]
				if strings.HasSuffix(last, ".service") {
					return last
				}
			}
		}
	}
	return ""
}

func readFirstLine(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	if sc.Scan() {
		return strings.TrimSpace(sc.Text()), nil
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", nil
}
