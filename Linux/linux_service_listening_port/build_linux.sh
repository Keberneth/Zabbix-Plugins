#!/usr/bin/env bash
set -euo pipefail

# Build Linux Zabbix Agent2 Go plugin: service.listening.port (amd64)
# Run in the same folder as main.go

PLUGIN_BIN="zabbix-agent2-linux-service-listening-port"
SDK_COMMIT="36ea4c1c90d"   # matches your Linux build workflow

if [ ! -f ./main.go ]; then
  echo "main.go not found in current folder."
  exit 1
fi

rm -f go.mod go.sum

go mod init linux_service_listening_port
go get golang.zabbix.com/sdk@${SDK_COMMIT}
go mod tidy

export GOOS=linux
export GOARCH=amd64
export CGO_ENABLED=0

go build -trimpath -ldflags "-s -w" -o "${PLUGIN_BIN}" .

echo "Built: $(realpath "${PLUGIN_BIN}")"
