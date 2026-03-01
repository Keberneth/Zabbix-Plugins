#!/usr/bin/env bash
set -euo pipefail

# Build the Zabbix Agent 2 loadable plugin for Windows (amd64) from Linux.
# Run in the project folder (where main.go is).

if [ ! -f ./go.mod ]; then
  go mod init adusermonitoring
fi

# Zabbix Go SDK is versioned by commit hash (no stable tags).
# This commit hash matches Zabbix 7.0 release branch usage.
go get golang.zabbix.com/sdk@d9643740a558

go mod tidy

export GOOS=windows
export GOARCH=amd64
export CGO_ENABLED=0

go build -trimpath -ldflags "-s -w" -o zabbix-agent2-ad-user-monitoring.exe .
