# Build the Zabbix Agent 2 loadable plugin for Windows (amd64).
# Run in the project folder (where main.go is).

$ErrorActionPreference = "Stop"

$PluginName = "zabbix-agent2-needs-reboot-check.exe"
$SdkCommit  = "3b95c058c0e"   # matches your existing Windows plugin build workflow

if (!(Test-Path ".\main.go")) {
    throw "main.go not found"
}

$env:GOOS="windows"
$env:GOARCH="amd64"
$env:CGO_ENABLED="0"

Remove-Item go.mod, go.sum -ErrorAction SilentlyContinue

go mod init windows_needs_reboot_check
go get golang.zabbix.com/sdk@$SdkCommit
go mod tidy

go build -trimpath -ldflags "-s -w" -o $PluginName .

Write-Host "Built: $PluginName"
