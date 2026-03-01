#!/usr/bin/env bash
set -euo pipefail

# Build Windows Zabbix Agent2 Go plugin: service.listening.port (amd64) from Linux
# Run in the same folder as main.go

PLUGIN_EXE="zabbix-agent2-windows-service-listening-port.exe"
SDK_COMMIT="3b95c058c0e"   # matches your Windows build workflow

if [ ! -f ./main.go ]; then
  echo "main.go not found in current folder."
  exit 1
fi

rm -f go.mod go.sum

go mod init windows_service_listening_port
go get golang.zabbix.com/sdk@${SDK_COMMIT}
go mod tidy

export GOOS=windows
export GOARCH=amd64
export CGO_ENABLED=0

go build -trimpath -ldflags "-s -w" -o "${PLUGIN_EXE}" .

echo "Built: $(realpath "${PLUGIN_EXE}")"
