param(
    [string]$OutputName = "zabbix-agent2-application_inventory.exe"
)

$ErrorActionPreference = "Stop"

Write-Host "== Building Windows Application Inventory Zabbix Agent2 Plugin =="

# Clean module files
if (Test-Path go.mod) { Remove-Item go.mod -Force }
if (Test-Path go.sum) { Remove-Item go.sum -Force }

# Initialize module
go mod init windows_application_inventory

# Pin SDK to same revision as your working plugins
go get golang.zabbix.com/sdk@3b95c058c0e

# Ensure dependency resolution is clean
go mod tidy

# Build
$env:GOOS = "windows"
$env:GOARCH = "amd64"
$env:CGO_ENABLED = "0"

go build -ldflags "-s -w" -o $OutputName .

Write-Host "Build completed: $OutputName"