#!/usr/bin/env bash
set -euo pipefail

# Build the Zabbix Agent 2 loadable plugin for Windows (amd64) from Linux.
# Run in the project folder (where main.go is).

PLUGIN_NAME="zabbix-agent2-needs-reboot-check.exe"
SDK_COMMIT="3b95c058c0e"   # matches your existing Windows plugin build workflow

if [ ! -f ./main.go ]; then
  echo "main.go not found"
  exit 1
fi

export GOOS=windows
export GOARCH=amd64
export CGO_ENABLED=0

rm -f go.mod go.sum

go mod init windows_needs_reboot_check
go get golang.zabbix.com/sdk@${SDK_COMMIT}
go mod tidy

go build -trimpath -ldflags "-s -w" -o ${PLUGIN_NAME} .

echo "Built: $(realpath ${PLUGIN_NAME})"
