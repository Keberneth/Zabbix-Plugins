#!/usr/bin/env bash
set -euo pipefail

# Build the Zabbix Agent 2 loadable plugin for Linux (amd64).
# Run in the project folder (where main.go is).

PLUGIN_NAME="zabbix-agent2-needs-reboot-check"
SDK_COMMIT="36ea4c1c90d"   # matches your existing Linux plugin build workflow

if [ ! -f ./main.go ]; then
  echo "main.go not found"
  exit 1
fi

export GOOS=linux
export GOARCH=amd64
export CGO_ENABLED=0

rm -f go.mod go.sum

go mod init linux_needs_reboot_check
go get golang.zabbix.com/sdk@${SDK_COMMIT}
go mod tidy

go build -trimpath -ldflags "-s -w" -o ${PLUGIN_NAME} .

echo "Built: $(realpath ${PLUGIN_NAME})"
