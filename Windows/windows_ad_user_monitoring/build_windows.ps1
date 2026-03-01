# Build the Zabbix Agent 2 loadable plugin for Windows (amd64)
# Run in PowerShell from the project folder (where main.go is)

$ErrorActionPreference = 'Stop'

# Ensure module is initialized
if (-not (Test-Path .\go.mod)) {
  go mod init adusermonitoring
}

# Zabbix Go SDK is versioned by commit hash (no stable tags).
# This commit hash matches Zabbix 7.0 release branch usage.
go get golang.zabbix.com/sdk@d9643740a558

go mod tidy

go build -trimpath -ldflags "-s -w" -o zabbix-agent2-ad-user-monitoring.exe .
