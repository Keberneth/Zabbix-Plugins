# Build Linux Zabbix Agent2 Go plugin: service.listening.port
# Run in the same folder as main.go

$ErrorActionPreference = "Stop"

$PluginBin = "zabbix-agent2-linux-service-listening-port"
$SdkCommit = "36ea4c1c90d"   # matches your Linux build workflow

if (!(Test-Path ".\main.go")) {
    Write-Error "main.go not found in current folder."
}

Remove-Item go.mod, go.sum -ErrorAction SilentlyContinue

go mod init linux_service_listening_port
go get golang.zabbix.com/sdk@$SdkCommit
go mod tidy

$env:GOOS        = "linux"
$env:GOARCH      = "amd64"
$env:CGO_ENABLED = "0"

go build -trimpath -ldflags "-s -w" -o $PluginBin .

Write-Host "Built: $PluginBin"
