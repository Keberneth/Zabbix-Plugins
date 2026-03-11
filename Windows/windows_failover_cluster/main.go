package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.zabbix.com/sdk/errs"
	"golang.zabbix.com/sdk/plugin"
	"golang.zabbix.com/sdk/plugin/container"
)

const (
	pluginName        = "WindowsFailoverCluster"
	pluginVersion     = "1.0.3"
	metricClusterJSON = "wfc.cluster.status"
	powerShellTimeout = 20 * time.Second
	cacheTTL          = 15 * time.Minute
)

var (
	_ plugin.Exporter = (*wfcPlugin)(nil)

	powerShellScript = `
$ErrorActionPreference = 'Stop'
$WarningPreference = 'SilentlyContinue'
$VerbosePreference = 'SilentlyContinue'
$InformationPreference = 'SilentlyContinue'
$ProgressPreference = 'SilentlyContinue'

function Get-PropValue {
    param([object]$Object, [string[]]$Names, $Default = $null)

    if ($null -eq $Object) {
        return $Default
    }

    foreach ($name in $Names) {
        $prop = $Object.PSObject.Properties[$name]
        if ($null -ne $prop) {
            $value = $prop.Value
            if ($null -ne $value -and "$value" -ne '') {
                return $value
            }
        }
    }

    return $Default
}

function Get-MapValue {
    param([hashtable]$Map, [string[]]$Names, $Default = $null)

    if ($null -eq $Map) {
        return $Default
    }

    foreach ($name in $Names) {
        if ($Map.ContainsKey($name) -and $null -ne $Map[$name] -and "$($Map[$name])" -ne '') {
            return $Map[$name]
        }
    }

    return $Default
}

function Get-ObjectName {
    param([object]$Value)

    if ($null -eq $Value) {
        return $null
    }

    if ($Value -is [string]) {
        return $Value
    }

    $nameProp = $Value.PSObject.Properties['Name']
    if ($null -ne $nameProp -and $null -ne $nameProp.Value) {
        return [string]$nameProp.Value
    }

    return [string]$Value
}

function Get-EnumText {
    param([object]$Value)

    if ($null -eq $Value) {
        return $null
    }

    return [string]$Value
}

function Get-EnumRaw {
    param([object]$Value)

    if ($null -eq $Value) {
        return $null
    }

    try {
        return [int]$Value.value__
    } catch {
        try {
            return [int]$Value
        } catch {
            return $null
        }
    }
}

function Normalize-Bool {
    param([object]$Value)

    if ($null -eq $Value) {
        return $null
    }

    if ($Value -is [bool]) {
        if ($Value) {
            return 1
        }
        return 0
    }

    $text = ([string]$Value).Trim().ToLowerInvariant()

    switch -Regex ($text) {
        '^(1|true|yes|online|enabled)$' { return 1 }
        '^(0|false|no|offline|disabled)$' { return 0 }
        default {
            try {
                if ([int]$Value -ne 0) {
                    return 1
                }
                return 0
            } catch {
                return $null
            }
        }
    }
}

Import-Module FailoverClusters -ErrorAction Stop

$cluster = Get-Cluster -ErrorAction Stop
$quorum = Get-ClusterQuorum -ErrorAction Stop
$allNodes = @(Get-ClusterNode -ErrorAction Stop)
$allGroups = @(Get-ClusterGroup -ErrorAction Stop)
$allResources = @(Get-ClusterResource -ErrorAction SilentlyContinue)
$allNetworks = @(Get-ClusterNetwork -ErrorAction SilentlyContinue)
$allInterfaces = @(Get-ClusterNetworkInterface -ErrorAction SilentlyContinue)

$resourceCountByGroup = @{}
foreach ($resource in $allResources) {
    $groupName = Get-ObjectName (Get-PropValue $resource @('OwnerGroup') $null)
    if ([string]::IsNullOrWhiteSpace($groupName)) {
        continue
    }

    if (-not $resourceCountByGroup.ContainsKey($groupName)) {
        $resourceCountByGroup[$groupName] = 0
    }

    $resourceCountByGroup[$groupName]++
}

$coreGroup = $allGroups | Where-Object {
    $groupType = Get-PropValue $_ @('GroupType', 'Type') $null
    [string]$groupType -match 'Cluster'
} | Select-Object -First 1

if ($null -eq $coreGroup) {
    $clusterName = [string](Get-PropValue $cluster @('Name') '')
    if (-not [string]::IsNullOrWhiteSpace($clusterName)) {
        $clusterNameResource = $allResources | Where-Object {
            ([string](Get-PropValue $_ @('Name') '')) -eq $clusterName -and
            ([string](Get-PropValue $_ @('ResourceType') '')) -eq 'Network Name'
        } | Select-Object -First 1

        if ($null -ne $clusterNameResource) {
            $coreGroupName = Get-ObjectName (Get-PropValue $clusterNameResource @('OwnerGroup') $null)
            if (-not [string]::IsNullOrWhiteSpace($coreGroupName)) {
                $coreGroup = $allGroups | Where-Object { [string]$_.Name -eq $coreGroupName } | Select-Object -First 1
            }
        }
    }
}

if ($null -eq $coreGroup) {
    $coreGroup = $allGroups | Where-Object { [string]$_.Name -eq 'Cluster Group' } | Select-Object -First 1
}

$coreGroupOwner = Get-ObjectName (Get-PropValue $coreGroup @('OwnerNode') $null)

$nodes = @(
    $allNodes |
        Sort-Object Name |
        ForEach-Object {
            $stateText = Get-EnumText $_.State
            $stateRaw = Get-EnumRaw $_.State
            $isUp = 0

            if ($stateText -eq 'Up' -or $stateText -eq 'Online' -or $stateRaw -eq 0) {
                $isUp = 1
            }

            [pscustomobject][ordered]@{
                Name          = [string]$_.Name
                Id            = Get-PropValue $_ @('Id') $null
                State         = $stateText
                StateRaw      = $stateRaw
                IsUp          = $isUp
                NodeWeight    = Get-PropValue $_ @('NodeWeight') $null
                DynamicWeight = Get-PropValue $_ @('DynamicWeight') $null
                FaultDomain   = Get-PropValue $_ @('FaultDomain') $null
            }
        }
)

$groups = @(
    $allGroups |
        Sort-Object Name |
        ForEach-Object {
            $stateText = Get-EnumText $_.State
            $ownerNode = Get-ObjectName (Get-PropValue $_ @('OwnerNode') $null)
            $name = [string]$_.Name
            $isOnline = 0

            if ($stateText -eq 'Online') {
                $isOnline = 1
            }

            [pscustomobject][ordered]@{
                Name          = $name
                State         = $stateText
                StateRaw      = Get-EnumRaw $_.State
                IsOnline      = $isOnline
                OwnerNode     = $ownerNode
                IsCoreGroup   = [int]($name -eq [string](Get-ObjectName (Get-PropValue $coreGroup @('Name') $null)))
                ResourceCount = $(if ($resourceCountByGroup.ContainsKey($name)) { $resourceCountByGroup[$name] } else { 0 })
            }
        }
)

$networks = @(
    $allNetworks |
        Sort-Object Name |
        ForEach-Object {
            [pscustomobject][ordered]@{
                Name       = [string]$_.Name
                State      = Get-EnumText (Get-PropValue $_ @('State') $null)
                StateRaw   = Get-EnumRaw (Get-PropValue $_ @('State') $null)
                Role       = Get-EnumText (Get-PropValue $_ @('Role') $null)
                RoleRaw    = Get-EnumRaw (Get-PropValue $_ @('Role') $null)
                Address    = Get-PropValue $_ @('Address') $null
                AddressMask= Get-PropValue $_ @('AddressMask') $null
                Metric     = Get-PropValue $_ @('Metric') $null
                AutoMetric = Get-PropValue $_ @('AutoMetric') $null
            }
        }
)

$networkInterfaces = @(
    $allInterfaces |
        Sort-Object Name |
        ForEach-Object {
            [pscustomobject][ordered]@{
                Name      = [string]$_.Name
                Node      = Get-ObjectName (Get-PropValue $_ @('Node') $null)
                Network   = Get-ObjectName (Get-PropValue $_ @('Network') $null)
                State     = Get-EnumText (Get-PropValue $_ @('State') $null)
                StateRaw  = Get-EnumRaw (Get-PropValue $_ @('State') $null)
                Address   = Get-PropValue $_ @('Address', 'IPv4Addresses', 'IPv6Addresses') $null
                Adapter   = Get-PropValue $_ @('Adapter', 'NetworkAdapter') $null
            }
        }
)

$clusterIPs = @(
    $allResources |
        Where-Object {
            $resourceType = [string](Get-PropValue $_ @('ResourceType') '')
            $resourceType -eq 'IP Address' -or $resourceType -eq 'IPv6 Address'
        } |
        Sort-Object Name |
        ForEach-Object {
            $parameters = @{}
            try {
                $_ | Get-ClusterParameter -ErrorAction Stop | ForEach-Object {
                    $parameters[$_.Name] = $_.Value
                }
            } catch {
            }

            $stateText = Get-EnumText $_.State
            $isOnline = 0
            if ($stateText -eq 'Online') {
                $isOnline = 1
            }

            [pscustomobject][ordered]@{
                Name        = [string]$_.Name
                ResourceType= [string](Get-PropValue $_ @('ResourceType') '')
                State       = $stateText
                StateRaw    = Get-EnumRaw $_.State
                IsOnline    = $isOnline
                OwnerNode   = Get-ObjectName (Get-PropValue $_ @('OwnerNode') $null)
                OwnerGroup  = Get-ObjectName (Get-PropValue $_ @('OwnerGroup') $null)
                Address     = Get-MapValue $parameters @('Address', 'IPv6Address', 'Ipv6Address') $null
                SubnetMask  = Get-MapValue $parameters @('SubnetMask') $null
                Network     = Get-MapValue $parameters @('Network') $null
                EnableDhcp  = Normalize-Bool (Get-MapValue $parameters @('EnableDhcp', 'DhcpEnabled') $null)
            }
        }
)

$quorumResourceName = [string](Get-PropValue $quorum @('QuorumResource') '')
$quorumMode = [string](Get-PropValue $quorum @('QuorumType', 'QuorumMode', 'Type') '')
$dynamicQuorum = Normalize-Bool (Get-PropValue $cluster @('DynamicQuorum') $null)

$forceQuorum = $null
foreach ($candidateName in @('ForceQuorum', 'ForcedQuorum', 'IsForcedQuorum', 'InForcedQuorum')) {
    $candidateValue = Get-PropValue $cluster @($candidateName) $null
    if ($null -ne $candidateValue) {
        $forceQuorum = Normalize-Bool $candidateValue
        break
    }
}
if ($null -eq $forceQuorum) {
    $forceQuorum = 0
}

$witnessResource = $null
if (-not [string]::IsNullOrWhiteSpace($quorumResourceName)) {
    $witnessResource = $allResources | Where-Object { [string]$_.Name -eq $quorumResourceName } | Select-Object -First 1
}
if ($null -eq $witnessResource) {
    $witnessResource = $allResources | Where-Object { [string](Get-PropValue $_ @('ResourceType') '') -match 'Witness' } | Select-Object -First 1
}

$witnessParameters = @{}
if ($null -ne $witnessResource) {
    try {
        $witnessResource | Get-ClusterParameter -ErrorAction Stop | ForEach-Object {
            $witnessParameters[$_.Name] = $_.Value
        }
    } catch {
    }
}

$witnessTypeHint = [string](Get-PropValue $witnessResource @('ResourceType') '')
if ([string]::IsNullOrWhiteSpace($witnessTypeHint)) {
    $witnessTypeHint = $quorumMode
}
if ([string]::IsNullOrWhiteSpace($witnessTypeHint)) {
    $witnessTypeHint = $quorumResourceName
}

$witnessType = $null
switch -Regex ($witnessTypeHint) {
    'file\s*share|nodeandfilesharemajority' { $witnessType = 'FileShareWitness'; break }
    'cloud|nodeandcloudmajority'           { $witnessType = 'CloudWitness'; break }
    'disk|nodeanddiskmajority'             { $witnessType = 'DiskWitness'; break }
    'node\s*majority|majority node set'    { $witnessType = 'None'; break }
    default {
        if ([string]::IsNullOrWhiteSpace($witnessTypeHint)) {
            $witnessType = 'None'
        } else {
            $witnessType = $witnessTypeHint
        }
    }
}

if ([string]::IsNullOrWhiteSpace($quorumMode)) {
    switch ($witnessType) {
        'FileShareWitness' { $quorumMode = 'NodeAndFileShareMajority'; break }
        'CloudWitness'     { $quorumMode = 'NodeAndCloudMajority'; break }
        'DiskWitness'      { $quorumMode = 'NodeAndDiskMajority'; break }
        default            { $quorumMode = 'NodeMajority' }
    }
}

$witnessState = Get-EnumText (Get-PropValue $witnessResource @('State') $null)
$witnessPresent = 0
if ($null -ne $witnessResource -or ($witnessType -ne 'None' -and -not [string]::IsNullOrWhiteSpace($quorumResourceName))) {
    $witnessPresent = 1
}

$witnessHealth = 1
if ($witnessPresent -eq 1 -and -not [string]::IsNullOrWhiteSpace($witnessState) -and $witnessState -ne 'Online') {
    $witnessHealth = 0
}

$witnessInUse = 0
if ($witnessPresent -eq 1 -and ($witnessState -eq 'Online' -or [string]::IsNullOrWhiteSpace($witnessState))) {
    $witnessInUse = 1
}

$summary = [pscustomobject][ordered]@{
    NodesTotal            = @($nodes).Count
    NodesDown             = @($nodes | Where-Object { $_.IsUp -ne 1 }).Count
    GroupsTotal           = @($groups).Count
    GroupsOffline         = @($groups | Where-Object { $_.IsOnline -ne 1 }).Count
    ClusterIPTotal        = @($clusterIPs).Count
    NetworkTotal          = @($networks).Count
    NetworkInterfaceTotal = @($networkInterfaces).Count
}

$result = [pscustomobject][ordered]@{
    Timestamp             = (Get-Date).ToString('o')
    LocalNode             = $env:COMPUTERNAME
    ClusterName           = [string](Get-PropValue $cluster @('Name') '')
    ClusterGroupOwnerNode = $coreGroupOwner
    Summary               = $summary
    Quorum                = [pscustomobject][ordered]@{
        Mode                = $quorumMode
        QuorumResource      = $quorumResourceName
        DynamicQuorum       = $dynamicQuorum
        ForceQuorum         = $forceQuorum
        WitnessType         = $witnessType
        WitnessPresent      = $witnessPresent
        WitnessInUse        = $witnessInUse
        WitnessHealth       = $witnessHealth
        WitnessDynamicWeight= Get-MapValue $witnessParameters @('DynamicWeight', 'WitnessDynamicWeight') $null
        WitnessResource     = $(if ($null -ne $witnessResource) { [string]$witnessResource.Name } else { $quorumResourceName })
        WitnessState        = $witnessState
        WitnessOwnerNode    = Get-ObjectName (Get-PropValue $witnessResource @('OwnerNode') $null)
        WitnessOwnerGroup   = Get-ObjectName (Get-PropValue $witnessResource @('OwnerGroup') $null)
        WitnessPath         = Get-MapValue $witnessParameters @('SharePath', 'Path') $null
        WitnessAccountName  = Get-MapValue $witnessParameters @('AccountName', 'StorageAccountName') $null
        WitnessEndpoint     = Get-MapValue $witnessParameters @('Endpoint') $null
    }
    Nodes                 = $nodes
    Groups                = $groups
    Networks              = $networks
    NetworkInterfaces     = $networkInterfaces
    ClusterIPs            = $clusterIPs
}

$result | ConvertTo-Json -Depth 8 -Compress
`
)

type cachedPayload struct {
	generatedAt time.Time
	payload     string
}

type wfcPlugin struct {
	plugin.Base
	mu    sync.Mutex
	cache cachedPayload
}

func main() {
	if exitCode, handled := maybeRunStandalone(os.Args[1:]); handled {
		os.Exit(exitCode)
	}

	if err := run(); err != nil {
		panic(err)
	}
}

func printStandaloneUsage() {
	fmt.Fprintln(os.Stderr, "Windows Failover Cluster plugin self-test")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  zabbix-agent2-windows-failover-cluster.exe --standalone [--verbose]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Examples:")
	fmt.Fprintln(os.Stderr, "  & \"C:\\Program Files\\Zabbix Agent 2\\zabbix-agent2-windows-failover-cluster.exe\" --standalone")
	fmt.Fprintln(os.Stderr, "  & \"C:\\Program Files\\Zabbix Agent 2\\zabbix-agent2-windows-failover-cluster.exe\" --standalone --verbose")
}

func maybeRunStandalone(args []string) (int, bool) {
	if len(args) == 0 {
		printStandaloneUsage()
		return 2, true
	}

	standalone := false
	verbose := false

	for _, arg := range args {
		switch strings.ToLower(arg) {
		case "--standalone", "-standalone", "--selftest", "-selftest":
			standalone = true
		case "--verbose", "-verbose", "-v":
			verbose = true
		case "--help", "-h", "-?", "/?", "/h", "/help":
			printStandaloneUsage()
			return 0, true
		default:
			if strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "/") {
				fmt.Fprintf(os.Stderr, "unknown argument: %s\n\n", arg)
				printStandaloneUsage()
				return 2, true
			}

			return 0, false
		}
	}

	if !standalone {
		return 0, false
	}

	payload, err := collectLive()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1, true
	}

	if verbose {
		var normalized any
		if err := json.Unmarshal([]byte(payload), &normalized); err == nil {
			pretty, err := json.MarshalIndent(normalized, "", "  ")
			if err == nil {
				payload = string(pretty)
			}
		}
	}

	fmt.Println(payload)
	return 0, true
}

func run() error {
	p := &wfcPlugin{}

	err := plugin.RegisterMetrics(
		p,
		pluginName,
		metricClusterJSON,
		"Returns a normalized JSON snapshot of the local Windows Failover Cluster.",
	)
	if err != nil {
		return errs.Wrap(err, "failed to register metrics")
	}

	h, err := container.NewHandler(pluginName)
	if err != nil {
		return errs.Wrap(err, "failed to create new handler")
	}

	p.Logger = h

	err = h.Execute()
	if err != nil {
		return errs.Wrap(err, "failed to execute plugin handler")
	}

	return nil
}

func (p *wfcPlugin) Export(key string, params []string, _ plugin.ContextProvider) (any, error) {
	if key != metricClusterJSON {
		return nil, errs.Errorf("unknown item key %q", key)
	}

	if len(params) != 0 {
		return nil, errs.Errorf("%s does not accept parameters", metricClusterJSON)
	}

	payload, err := collectLive()
	if err == nil {
		p.storeCache(payload)
		return payload, nil
	}

	if cached, ok := p.loadCached(err); ok {
		return cached, nil
	}

	return nil, err
}

func collectLive() (string, error) {
	commandCtx, cancel := context.WithTimeout(context.Background(), powerShellTimeout)
	defer cancel()

	cmd := exec.CommandContext(
		commandCtx,
		resolvePowerShellPath(),
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy",
		"Bypass",
		"-Command",
		"-",
	)
	cmd.Stdin = strings.NewReader(powerShellScript)

	output, err := cmd.CombinedOutput()
	if commandCtx.Err() == context.DeadlineExceeded {
		return "", errs.Errorf("powershell collection timed out after %s", powerShellTimeout)
	}

	if err != nil {
		errorText := strings.TrimSpace(string(output))
		if errorText == "" {
			errorText = err.Error()
		}
		return "", errs.Wrap(err, fmt.Sprintf("powershell collection failed: %s", errorText))
	}

	return enrichPayload(output, "live", "", 0)
}

func resolvePowerShellPath() string {
	systemRoot := strings.TrimSpace(os.Getenv("SystemRoot"))
	if systemRoot == "" {
		return "powershell.exe"
	}

	return filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
}

func (p *wfcPlugin) storeCache(payload string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.cache.generatedAt = time.Now()
	p.cache.payload = payload
}

func (p *wfcPlugin) loadCached(liveErr error) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cache.payload == "" {
		return "", false
	}

	age := time.Since(p.cache.generatedAt)
	if age > cacheTTL {
		return "", false
	}

	payload, err := enrichPayload([]byte(p.cache.payload), "cached", liveErr.Error(), age)
	if err != nil {
		return p.cache.payload, true
	}

	return payload, true
}

func enrichPayload(raw []byte, mode string, collectionErr string, age time.Duration) (string, error) {
	var payload map[string]any

	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", errs.Errorf("empty payload returned by powershell collector")
	}

	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", errs.Wrap(err, "failed to parse powershell JSON output")
	}

	if payload == nil {
		payload = map[string]any{}
	}

	payload["CollectorVersion"] = pluginVersion
	payload["CollectionMode"] = mode
	payload["CollectionAgeSeconds"] = int(age.Seconds())

	if collectionErr == "" {
		delete(payload, "CollectionError")
	} else {
		payload["CollectionError"] = collectionErr
	}

	normalized, err := json.Marshal(payload)
	if err != nil {
		return "", errs.Wrap(err, "failed to marshal normalized payload")
	}

	return string(normalized), nil
}
