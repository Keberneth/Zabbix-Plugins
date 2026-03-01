# Build Windows Zabbix Agent2 Go plugin: service.listening.port
# Run in the same folder as main.go

$ErrorActionPreference = "Stop"

$PluginExe  = "zabbix-agent2-windows-service-listening-port.exe"
$SdkCommit  = "3b95c058c0e"   # matches your Windows build workflow

if (!(Test-Path ".\main.go")) {
    Write-Error "main.go not found in current folder."
}

Remove-Item go.mod, go.sum -ErrorAction SilentlyContinue

go mod init windows_service_listening_port
go get golang.zabbix.com/sdk@$SdkCommit
go mod tidy

$env:GOOS        = "windows"
$env:GOARCH      = "amd64"
$env:CGO_ENABLED = "0"

go build -trimpath -ldflags "-s -w" -o $PluginExe .

Write-Host "Built: $PluginExe"
