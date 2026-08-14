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
	pluginName        = "ExchangeSE"
	pluginVersion     = "1.0.0"
	metricStatusJSON  = "exchange.se.status"
	powerShellTimeout = 100 * time.Second
	cacheTTL          = 30 * time.Minute
)

var (
	_ plugin.Exporter = (*exchangePlugin)(nil)

	powerShellScript = `
$ErrorActionPreference = 'Stop'
$WarningPreference = 'SilentlyContinue'
$VerbosePreference = 'SilentlyContinue'
$InformationPreference = 'SilentlyContinue'
$ProgressPreference = 'SilentlyContinue'

$script:collectionErrors = New-Object System.Collections.Generic.List[string]

function Add-CollectionError {
    param([string]$Area, [object]$ErrorRecord)

    $message = ''
    try { $message = [string]$ErrorRecord.Exception.Message } catch { $message = [string]$ErrorRecord }
    if ($null -eq $message) { $message = 'unknown error' }
    $message = $message.Trim()
    if ($message.Length -gt 300) { $message = $message.Substring(0, 300) }
    $null = $script:collectionErrors.Add($Area + ': ' + $message)
}

function Get-Text {
    param($Value)

    if ($null -eq $Value) { return '' }
    return ([string]$Value).Trim()
}

function Limit-Text {
    param($Value, [int]$MaxLength = 300)

    $text = Get-Text $Value
    if ($text.Length -gt $MaxLength) { return $text.Substring(0, $MaxLength) }
    return $text
}

function ConvertTo-Long {
    param($Value, $Default = 0)

    if ($null -eq $Value) { return $Default }
    try { return [long]$Value } catch { return $Default }
}

function ConvertTo-Double {
    param($Value, $Default = 0)

    if ($null -eq $Value) { return $Default }
    try { return [math]::Round([double]$Value, 2) } catch { return $Default }
}

function Normalize-Bool {
    param($Value, $Default = 0)

    if ($null -eq $Value) { return $Default }
    if ($Value -is [bool]) { if ($Value) { return 1 } return 0 }
    $text = (Get-Text $Value).ToLowerInvariant()
    switch -Regex ($text) {
        '^(1|true|yes)$' { return 1 }
        '^(0|false|no)$' { return 0 }
        default { return $Default }
    }
}

function ConvertTo-Bytes {
    param($Value)

    if ($null -eq $Value) { return $null }
    $method = $Value.PSObject.Methods['ToBytes']
    if ($null -ne $method) {
        try { return [long]$Value.ToBytes() } catch { }
    }
    $text = [string]$Value
    if ($text -match '\(([\d,.\s]+)\s*bytes\)') {
        $digits = ($Matches[1] -replace '[^\d]', '')
        if ($digits -ne '') { try { return [long]$digits } catch { } }
    }
    return $null
}

function To-IsoString {
    param($Value)

    if ($null -eq $Value -or (Get-Text $Value) -eq '') { return '' }
    try { return ([datetime]$Value).ToString('o') } catch { return Get-Text $Value }
}

function Get-AgeHours {
    param($Value)

    if ($null -eq $Value -or (Get-Text $Value) -eq '') { return -1 }
    try {
        $dt = [datetime]$Value
        return [math]::Round(((Get-Date) - $dt).TotalHours, 1)
    } catch {
        return -1
    }
}

# --- Load the Exchange Management Shell (read-only usage only) -------------
$shellMode = 'none'
$remoteSession = $null
$exchangeCmdlets = @(
    'Get-ExchangeServer', 'Get-MailboxServer', 'Get-Queue', 'Get-ExchangeCertificate',
    'Get-MailboxDatabase', 'Get-MailboxDatabaseCopyStatus', 'Get-ServerComponentState',
    'Get-HealthReport', 'Test-ReplicationHealth', 'Get-ExchangeDiagnosticInfo'
)

try {
    Add-PSSnapin -Name Microsoft.Exchange.Management.PowerShell.SnapIn -ErrorAction Stop
    $shellMode = 'snapin'
} catch {
    $snapinError = $_
    try {
        $fqdn = ([System.Net.Dns]::GetHostEntry($env:COMPUTERNAME)).HostName
        $remoteSession = New-PSSession -ConfigurationName Microsoft.Exchange -ConnectionUri ('http://{0}/PowerShell/' -f $fqdn) -Authentication Kerberos -ErrorAction Stop
        Import-PSSession -Session $remoteSession -DisableNameChecking -AllowClobber -CommandName $exchangeCmdlets -ErrorAction Stop | Out-Null
        $shellMode = 'remote'
    } catch {
        throw ('Unable to load the Exchange Management Shell snap-in ({0}) or open a remote session ({1}). Run the Zabbix Agent 2 service as LocalSystem on the Exchange server, or grant its service account the View-Only Organization Management role.' -f $snapinError.Exception.Message, $_.Exception.Message)
    }
}

$server = $env:COMPUTERNAME

# --- Exchange server identity ----------------------------------------------
$exchangeVersion = ''
$exchangeEdition = ''
$exchangeRoles = ''
$exchangeSite = ''
$dagName = ''
try {
    $exchangeServer = Get-ExchangeServer -Identity $server -ErrorAction Stop
    $exchangeVersion = Get-Text $exchangeServer.AdminDisplayVersion
    $exchangeEdition = Get-Text $exchangeServer.Edition
    $exchangeRoles = Get-Text $exchangeServer.ServerRole
    $exchangeSite = Get-Text $exchangeServer.Site
    if ($exchangeSite -ne '') { $exchangeSite = ($exchangeSite -split '/')[-1] }
} catch {
    Add-CollectionError 'ExchangeServer' $_
}
try {
    $mailboxServer = Get-MailboxServer -Identity $server -ErrorAction Stop
    $dagName = Get-Text $mailboxServer.DatabaseAvailabilityGroup
} catch {
    Add-CollectionError 'MailboxServer' $_
}

# --- Windows services -------------------------------------------------------
$services = @()
try {
    $services = @(
        Get-Service -ErrorAction Stop |
            Where-Object { $_.Name -like 'MSExchange*' -or $_.Name -in @('W3SVC', 'WinRM', 'IISADMIN') } |
            Sort-Object Name |
            ForEach-Object {
                $startType = ''
                try { $startType = Get-Text $_.StartType } catch { }
                [pscustomobject][ordered]@{
                    Name        = $_.Name
                    DisplayName = Limit-Text $_.DisplayName 120
                    Status      = Get-Text $_.Status
                    StartType   = $startType
                    Running     = [int]((Get-Text $_.Status) -eq 'Running')
                    AutoStart   = [int]($startType -eq 'Automatic')
                }
            }
    )
} catch {
    Add-CollectionError 'Services' $_
}

# --- Transport queues -------------------------------------------------------
$queues = @()
$queueListTruncated = 0
try {
    $queues = @(
        Get-Queue -Server $server -ErrorAction Stop | ForEach-Object {
            $deliveryType = Get-Text $_.DeliveryType
            [pscustomobject][ordered]@{
                Identity      = Get-Text $_.Identity
                DeliveryType  = $deliveryType
                NextHopDomain = Limit-Text $_.NextHopDomain 200
                Status        = Get-Text $_.Status
                MessageCount  = ConvertTo-Long $_.MessageCount 0
                IncomingRate  = ConvertTo-Double $_.IncomingRate 0
                OutgoingRate  = ConvertTo-Double $_.OutgoingRate 0
                Velocity      = ConvertTo-Double $_.Velocity 0
                IsShadow      = [int]($deliveryType -eq 'ShadowRedundancy')
                LastError     = Limit-Text $_.LastError 300
            }
        }
    )
} catch {
    Add-CollectionError 'Queues' $_
}

$nonShadowQueues = @($queues | Where-Object { $_.IsShadow -ne 1 })
$submissionDepth = 0
$poisonDepth = 0
$unreachableDepth = 0
$totalDepth = 0
$shadowDepth = 0
$incomingRate = 0.0
$outgoingRate = 0.0
$retryCount = 0
foreach ($q in $queues) {
    if ($q.IsShadow -eq 1) {
        $shadowDepth += $q.MessageCount
        continue
    }
    if ($q.Identity -match '\\Poison$') {
        $poisonDepth += $q.MessageCount
        continue
    }
    if ($q.Identity -match '\\Submission$') { $submissionDepth += $q.MessageCount }
    if ($q.Identity -match '\\Unreachable$') { $unreachableDepth += $q.MessageCount }
    $totalDepth += $q.MessageCount
    $incomingRate += $q.IncomingRate
    $outgoingRate += $q.OutgoingRate
    if ($q.Status -eq 'Retry') { $retryCount++ }
}
$incomingRate = [math]::Round($incomingRate, 2)
$outgoingRate = [math]::Round($outgoingRate, 2)

$queueList = $queues
if (@($queues).Count -gt 60) {
    $keep = @{}
    foreach ($q in $queues) {
        if ($q.IsShadow -ne 1 -and ($q.Status -eq 'Retry' -or $q.Identity -match '\\(Submission|Poison|Unreachable)$')) {
            $keep[$q.Identity] = $q
        }
    }
    foreach ($q in ($queues | Sort-Object MessageCount -Descending | Select-Object -First 40)) {
        $keep[$q.Identity] = $q
    }
    $queueList = @($keep.Values | Sort-Object MessageCount -Descending)
    $queueListTruncated = 1
}

# --- Certificates ------------------------------------------------------------
$certificates = @()
try {
    $certificates = @(
        Get-ExchangeCertificate -Server $server -ErrorAction Stop | ForEach-Object {
            $days = $null
            try { $days = [math]::Floor((([datetime]$_.NotAfter) - (Get-Date)).TotalDays) } catch { }
            $domains = ''
            try { $domains = Limit-Text (@($_.CertificateDomains | ForEach-Object { [string]$_ }) -join ' ') 300 } catch { }
            [pscustomobject][ordered]@{
                Thumbprint    = Get-Text $_.Thumbprint
                Subject       = Limit-Text $_.Subject 200
                Issuer        = Limit-Text $_.Issuer 200
                Services      = Get-Text $_.Services
                IsSelfSigned  = Normalize-Bool $_.IsSelfSigned 0
                NotAfter      = To-IsoString $_.NotAfter
                DaysRemaining = $days
                Domains       = $domains
            }
        }
    )
} catch {
    Add-CollectionError 'Certificates' $_
}

# --- Mailbox databases (copies hosted on this server) ------------------------
$databases = @()
try {
    $databases = @(
        Get-MailboxDatabase -Server $server -Status -ErrorAction Stop |
            Where-Object { -not (Normalize-Bool $_.Recovery 0) } |
            Sort-Object Name |
            ForEach-Object {
                $mountedOn = Get-Text $_.MountedOnServer
                $mountedOnShort = ''
                if ($mountedOn -ne '') { $mountedOnShort = ($mountedOn -split '\.')[0] }
                [pscustomobject][ordered]@{
                    Name                          = Get-Text $_.Name
                    Mounted                       = Normalize-Bool $_.Mounted 0
                    MountedOnServer               = $mountedOnShort
                    IsLocalActive                 = [int]($mountedOnShort -eq $server)
                    SizeBytes                     = ConvertTo-Bytes $_.DatabaseSize
                    AvailableNewMailboxSpaceBytes = ConvertTo-Bytes $_.AvailableNewMailboxSpace
                    CircularLogging               = Normalize-Bool $_.CircularLoggingEnabled 0
                    LastFullBackup                = To-IsoString $_.LastFullBackup
                    LastFullBackupAgeHours        = Get-AgeHours $_.LastFullBackup
                    LastIncrementalBackup         = To-IsoString $_.LastIncrementalBackup
                    LastIncrementalBackupAgeHours = Get-AgeHours $_.LastIncrementalBackup
                }
            }
    )
} catch {
    Add-CollectionError 'Databases' $_
}

# --- Database copy status -----------------------------------------------------
$databaseCopies = @()
try {
    $databaseCopies = @(
        Get-MailboxDatabaseCopyStatus -Server $server -ErrorAction Stop |
            Sort-Object Name |
            ForEach-Object {
                $status = Get-Text $_.Status
                $indexState = Get-Text $_.ContentIndexState
                [pscustomobject][ordered]@{
                    Name                = Get-Text $_.Name
                    Database            = Get-Text $_.DatabaseName
                    Status              = $status
                    IsActive            = [int]($status -in @('Mounted', 'Dismounted', 'Mounting', 'Dismounting'))
                    IsHealthy           = [int]($status -in @('Mounted', 'Healthy'))
                    CopyQueueLength     = ConvertTo-Long $_.CopyQueueLength 0
                    ReplayQueueLength   = ConvertTo-Long $_.ReplayQueueLength 0
                    ContentIndexState   = $indexState
                    ContentIndexHealthy = [int]($indexState -in @('Healthy', 'NotApplicable', 'Unknown', ''))
                    ActivationSuspended = Normalize-Bool $_.ActivationSuspended 0
                    LastInspectedLogTime= To-IsoString $_.LastInspectedLogTime
                }
            }
    )
} catch {
    Add-CollectionError 'DatabaseCopies' $_
}

# --- Server component states ---------------------------------------------------
$components = @()
try {
    $components = @(
        Get-ServerComponentState -Identity $server -ErrorAction Stop |
            Sort-Object Component |
            ForEach-Object {
                $state = Get-Text $_.State
                $stateRaw = 0
                if ($state -eq 'Active') { $stateRaw = 1 }
                elseif ($state -eq 'Draining') { $stateRaw = 2 }
                [pscustomobject][ordered]@{
                    Name     = Get-Text $_.Component
                    State    = $state
                    StateRaw = $stateRaw
                }
            }
    )
} catch {
    Add-CollectionError 'Components' $_
}

$defaultInactiveComponents = @('ForwardSyncDaemon', 'ProvisioningRps')
$componentsInactive = @($components | Where-Object { $_.State -ne 'Active' -and $_.Name -notin $defaultInactiveComponents }).Count
$serverWideOffline = Get-Text (($components | Where-Object { $_.Name -eq 'ServerWideOffline' } | Select-Object -First 1).State)
$hubTransportState = Get-Text (($components | Where-Object { $_.Name -eq 'HubTransport' } | Select-Object -First 1).State)
$maintenanceMode = 0
if (($serverWideOffline -ne '' -and $serverWideOffline -ne 'Active') -or ($hubTransportState -ne '' -and $hubTransportState -ne 'Active')) {
    $maintenanceMode = 1
}

# --- Managed Availability health sets -------------------------------------------
$healthSets = @()
try {
    $healthMap = @{}
    foreach ($row in @(Get-HealthReport -Identity $server -ErrorAction Stop)) {
        $name = Get-Text $row.HealthSet
        if ($name -eq '') { continue }
        $alert = Get-Text $row.AlertValue
        $raw = 3
        switch ($alert) {
            'Unhealthy' { $raw = 0 }
            'Degraded'  { $raw = 1 }
            'Repairing' { $raw = 2 }
            'Unknown'   { $raw = 3 }
            'Disabled'  { $raw = 4 }
            'Healthy'   { $raw = 5 }
        }
        $entry = [pscustomobject][ordered]@{
            Name       = $name
            State      = Get-Text $row.State
            AlertValue = $alert
            AlertRaw   = $raw
        }
        if (-not $healthMap.ContainsKey($name) -or $raw -lt $healthMap[$name].AlertRaw) {
            $healthMap[$name] = $entry
        }
    }
    $healthSets = @($healthMap.Values | Sort-Object Name)
} catch {
    Add-CollectionError 'HealthSets' $_
}

# --- DAG replication health -------------------------------------------------------
$replicationHealth = @()
if ($dagName -ne '') {
    try {
        $replicationHealth = @(
            Test-ReplicationHealth -Identity $server -ErrorAction Stop | ForEach-Object {
                $result = Get-Text $_.Result
                [pscustomobject][ordered]@{
                    Check    = Get-Text $_.Check
                    Result   = $result
                    IsFailed = [int]($result -match 'fail')
                    Error    = Limit-Text $_.Error 300
                }
            }
        )
    } catch {
        Add-CollectionError 'ReplicationHealth' $_
    }
}

# --- Protocol health check pages (Managed Availability probe targets) --------------
$vdirs = @()
try {
    if (-not ([System.Management.Automation.PSTypeName]'ZbxTrustAllCerts').Type) {
        Add-Type -TypeDefinition @'
using System.Net;
using System.Net.Security;
using System.Security.Cryptography.X509Certificates;
public static class ZbxTrustAllCerts {
    public static void Enable() {
        ServicePointManager.ServerCertificateValidationCallback = delegate (object s, X509Certificate c, X509Chain ch, SslPolicyErrors e) { return true; };
    }
}
'@
    }
    [ZbxTrustAllCerts]::Enable()
    [System.Net.ServicePointManager]::SecurityProtocol = [System.Net.ServicePointManager]::SecurityProtocol -bor [System.Net.SecurityProtocolType]::Tls12

    $vdirTargets = @(
        @{ Protocol = 'OWA';          Path = '/owa/healthcheck.htm' },
        @{ Protocol = 'ECP';          Path = '/ecp/healthcheck.htm' },
        @{ Protocol = 'EWS';          Path = '/ews/healthcheck.htm' },
        @{ Protocol = 'ActiveSync';   Path = '/microsoft-server-activesync/healthcheck.htm' },
        @{ Protocol = 'Autodiscover'; Path = '/autodiscover/healthcheck.htm' },
        @{ Protocol = 'MAPI';         Path = '/mapi/healthcheck.htm' },
        @{ Protocol = 'OAB';          Path = '/oab/healthcheck.htm' },
        @{ Protocol = 'RPC';          Path = '/rpc/healthcheck.htm' },
        @{ Protocol = 'PowerShell';   Path = '/powershell/healthcheck.htm' }
    )

    $vdirs = @(
        foreach ($target in $vdirTargets) {
            $url = 'https://localhost' + $target.Path
            $code = 0
            $stopwatch = [System.Diagnostics.Stopwatch]::StartNew()
            try {
                $response = Invoke-WebRequest -Uri $url -UseBasicParsing -TimeoutSec 5 -ErrorAction Stop
                $code = [int]$response.StatusCode
            } catch {
                try { $code = [int]$_.Exception.Response.StatusCode } catch { $code = 0 }
            }
            $stopwatch.Stop()
            [pscustomobject][ordered]@{
                Protocol   = $target.Protocol
                Url        = $url
                StatusCode = $code
                Healthy    = [int]($code -eq 200)
                ResponseMs = [long]$stopwatch.ElapsedMilliseconds
            }
        }
    )
} catch {
    Add-CollectionError 'Vdirs' $_
}

# --- Transport back pressure --------------------------------------------------------
$backPressureRaw = $null
$backPressureState = ''
$backPressureResources = @()
try {
    $diagRaw = Get-ExchangeDiagnosticInfo -Server $server -Process EdgeTransport -Component ResourceThrottling -ErrorAction Stop
    $diagText = ''
    foreach ($chunk in @($diagRaw)) { $diagText += [string]$chunk }
    [xml]$diagXml = $diagText
    $meters = @($diagXml.SelectNodes('//ResourceMeter'))
    $worst = 0
    foreach ($meter in $meters) {
        $currentUse = Get-Text $meter.GetAttribute('CurrentResourceUse')
        $level = 0
        if ($currentUse -eq 'Medium') { $level = 1 }
        elseif ($currentUse -eq 'High') { $level = 2 }
        if ($level -gt $worst) { $worst = $level }
        $backPressureResources += [pscustomobject][ordered]@{
            Resource = Limit-Text ($meter.GetAttribute('Resource')) 200
            Current  = $currentUse
            Previous = Get-Text $meter.GetAttribute('PreviousResourceUse')
            Pressure = Get-Text $meter.GetAttribute('Pressure')
        }
    }
    if ($meters.Count -gt 0) {
        $backPressureRaw = $worst
        $backPressureState = @('Normal', 'Medium', 'High')[$worst]
    }
} catch {
    Add-CollectionError 'BackPressure' $_
}

# --- Summary --------------------------------------------------------------------------
$certDays = @($certificates | Where-Object { $null -ne $_.DaysRemaining } | ForEach-Object { $_.DaysRemaining })
$certMinDays = $null
if ($certDays.Count -gt 0) { $certMinDays = ($certDays | Measure-Object -Minimum).Minimum }

$summary = [pscustomobject][ordered]@{
    QueueTotalDepth         = $totalDepth
    QueueSubmissionDepth    = $submissionDepth
    QueuePoisonDepth        = $poisonDepth
    QueueUnreachableDepth   = $unreachableDepth
    QueueShadowDepth        = $shadowDepth
    QueueRetryCount         = $retryCount
    QueueIncomingRate       = $incomingRate
    QueueOutgoingRate       = $outgoingRate
    QueueListTruncated      = $queueListTruncated
    ServicesNotRunning      = @($services | Where-Object { $_.AutoStart -eq 1 -and $_.Running -ne 1 }).Count
    ComponentsInactive      = $componentsInactive
    HealthSetsUnhealthy     = @($healthSets | Where-Object { $_.AlertRaw -eq 0 }).Count
    DbCopiesTotal           = @($databaseCopies).Count
    DbCopiesUnhealthy       = @($databaseCopies | Where-Object { $_.IsHealthy -ne 1 }).Count
    ReplicationChecksFailed = @($replicationHealth | Where-Object { $_.IsFailed -eq 1 }).Count
    VdirsUnhealthy          = @($vdirs | Where-Object { $_.Healthy -ne 1 }).Count
    CertMinDaysRemaining    = $certMinDays
    BackPressureRaw         = $backPressureRaw
}

$result = [pscustomobject][ordered]@{
    Timestamp         = (Get-Date).ToString('o')
    LocalServer       = $server
    ShellMode         = $shellMode
    Exchange          = [pscustomobject][ordered]@{
        Version           = $exchangeVersion
        Edition           = $exchangeEdition
        Roles             = $exchangeRoles
        Site              = $exchangeSite
        DagName           = $dagName
        IsDagMember       = [int]($dagName -ne '')
        MaintenanceMode   = $maintenanceMode
        ServerWideOffline = $serverWideOffline
    }
    Summary           = $summary
    Queues            = $queueList
    Services          = $services
    Components        = $components
    HealthSets        = $healthSets
    Certificates      = $certificates
    Databases         = $databases
    DatabaseCopies    = $databaseCopies
    ReplicationHealth = $replicationHealth
    Vdirs             = $vdirs
    BackPressure      = [pscustomobject][ordered]@{
        State     = $backPressureState
        StateRaw  = $backPressureRaw
        Resources = $backPressureResources
    }
    Errors            = @($script:collectionErrors)
}

$json = $result | ConvertTo-Json -Depth 8 -Compress

if ($null -ne $remoteSession) {
    try { Remove-PSSession -Session $remoteSession -ErrorAction SilentlyContinue } catch { }
}

$json
`
)

type cachedPayload struct {
	generatedAt time.Time
	payload     string
}

type exchangePlugin struct {
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
	fmt.Fprintln(os.Stderr, "Microsoft Exchange SE plugin self-test")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  zabbix-agent2-exchange-se.exe --standalone [--verbose]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Examples:")
	fmt.Fprintln(os.Stderr, "  & \"C:\\Program Files\\Zabbix Agent 2\\zabbix-agent2-exchange-se.exe\" --standalone")
	fmt.Fprintln(os.Stderr, "  & \"C:\\Program Files\\Zabbix Agent 2\\zabbix-agent2-exchange-se.exe\" --standalone --verbose")
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
	p := &exchangePlugin{}

	err := plugin.RegisterMetrics(
		p,
		pluginName,
		metricStatusJSON,
		"Returns a normalized JSON snapshot of the local Microsoft Exchange SE server health.",
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

func (p *exchangePlugin) Export(key string, params []string, _ plugin.ContextProvider) (any, error) {
	if key != metricStatusJSON {
		return nil, errs.Errorf("unknown item key %q", key)
	}

	if len(params) != 0 {
		return nil, errs.Errorf("%s does not accept parameters", metricStatusJSON)
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

	// Capture stdout and stderr separately. The JSON payload is on stdout; any
	// warning / verbose / non-terminating-error text PowerShell writes to stderr
	// must not be merged into it. CombinedOutput() would interleave them and make
	// json.Unmarshal fail on an otherwise-successful collection.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// If a child process inherits the output pipe and outlives powershell, don't
	// let Wait block past the deadline waiting for the pipe to close.
	cmd.WaitDelay = 5 * time.Second

	err := cmd.Run()
	if commandCtx.Err() == context.DeadlineExceeded {
		return "", errs.Errorf("powershell collection timed out after %s", powerShellTimeout)
	}

	if err != nil {
		errorText := strings.TrimSpace(stderr.String())
		if errorText == "" {
			errorText = strings.TrimSpace(stdout.String())
		}
		if errorText == "" {
			errorText = err.Error()
		}
		return "", errs.Wrap(err, fmt.Sprintf("powershell collection failed: %s", errorText))
	}

	return enrichPayload(stdout.Bytes(), "live", "", 0)
}

func resolvePowerShellPath() string {
	systemRoot := strings.TrimSpace(os.Getenv("SystemRoot"))
	if systemRoot == "" {
		return "powershell.exe"
	}

	return filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
}

func (p *exchangePlugin) storeCache(payload string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.cache.generatedAt = time.Now()
	p.cache.payload = payload
}

func (p *exchangePlugin) loadCached(liveErr error) (string, bool) {
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
