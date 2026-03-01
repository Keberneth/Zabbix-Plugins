# Build windows-network-connections plugin (amd64)
# Run in same folder as main.go
# powershell.exe -NoProfile -ExecutionPolicy Bypass -File build_windows.ps1

$ErrorActionPreference = "Stop"

$PluginName = "zabbix-agent2-windows-network-connections.exe"
$SdkCommit  = "3b95c058c0e"

if (!(Test-Path ".\main.go")) {
    Write-Error "main.go not found"
}

Remove-Item go.mod, go.sum -ErrorAction SilentlyContinue

go mod init windows_network_connections
go get golang.zabbix.com/sdk@$SdkCommit
go mod tidy

$env:GOOS="windows"
$env:GOARCH="amd64"
$env:CGO_ENABLED="0"

go build -trimpath -ldflags "-s -w" -o $PluginName .

Write-Host "Built: $PluginName"