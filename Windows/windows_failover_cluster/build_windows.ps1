# Build the Zabbix Agent 2 loadable plugin for Windows (amd64)
# Run in PowerShell from the project folder (where main.go is)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

if (-not (Test-Path .\go.mod)) {
  go mod init windowsfailovercluster
}

# Zabbix Go SDK is versioned by commit hash (no stable tags).
# This commit hash matches the Zabbix release/7.0 SDK currently used by upstream examples.
go get golang.zabbix.com/sdk@d9643740a558

go mod tidy

$env:CGO_ENABLED = '0'
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'

go build -trimpath -ldflags "-s -w" -o zabbix-agent2-windows-failover-cluster.exe .
