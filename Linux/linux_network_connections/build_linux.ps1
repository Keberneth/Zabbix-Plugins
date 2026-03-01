# Build linux-network-connections plugin (amd64)
# Run in same folder as main.go
# powershell.exe -NoProfile -ExecutionPolicy Bypass -File build_linux.ps1

$ErrorActionPreference = "Stop"

$PluginName = "zabbix-agent2-linux-network-connections"
$SdkCommit  = "36ea4c1c90d"

if (!(Test-Path ".\main.go")) {
    Write-Error "main.go not found"
}

Remove-Item go.mod, go.sum -ErrorAction SilentlyContinue

go mod init linux_network_connections
go get golang.zabbix.com/sdk@$SdkCommit
go mod tidy

$env:GOOS="linux"
$env:GOARCH="amd64"
$env:CGO_ENABLED="0"

go build -trimpath -ldflags "-s -w" -o $PluginName .

Write-Host "Built: $PluginName"