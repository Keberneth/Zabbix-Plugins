#!/usr/bin/env bash
set -euo pipefail

# Build linux-network-connections plugin (amd64)
# Run in same folder as main.go

PLUGIN_NAME="zabbix-agent2-linux-network-connections"
SDK_COMMIT="36ea4c1c90d"   # same as your Linux workflow

if [ ! -f ./main.go ]; then
  echo "main.go not found"
  exit 1
fi

rm -f go.mod go.sum

go mod init linux_network_connections
go get golang.zabbix.com/sdk@${SDK_COMMIT}
go mod tidy

export GOOS=linux
export GOARCH=amd64
export CGO_ENABLED=0

go build -trimpath -ldflags "-s -w" -o ${PLUGIN_NAME} .

echo "Built: $(realpath ${PLUGIN_NAME})"