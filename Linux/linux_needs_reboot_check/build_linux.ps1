# Build the Zabbix Agent 2 loadable plugin for Linux (amd64) from Windows.
# Run in the project folder (where main.go is).
# powershell.exe -NoProfile -ExecutionPolicy Bypass -File build_linux.ps1

$ErrorActionPreference = "Stop"

$PluginName = "zabbix-agent2-needs-reboot-check"
$SdkCommit  = "36ea4c1c90d"   # matches your existing Linux plugin build workflow

if (!(Test-Path ".\main.go")) {
    throw "main.go not found"
}

$env:GOOS="linux"
$env:GOARCH="amd64"
$env:CGO_ENABLED="0"

Remove-Item go.mod, go.sum -ErrorAction SilentlyContinue

go mod init linux_needs_reboot_check
go get golang.zabbix.com/sdk@$SdkCommit
go mod tidy

go build -trimpath -ldflags "-s -w" -o $PluginName .

Write-Host "Built: $PluginName"
