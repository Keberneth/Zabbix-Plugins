#!/usr/bin/env bash
set -euo pipefail

OUTPUT_NAME="zabbix-agent2-application_inventory.exe"

echo "== Building Windows Application Inventory Zabbix Agent2 Plugin =="

rm -f go.mod go.sum

go mod init windows_application_inventory

# Pin to same SDK commit as other plugins
go get golang.zabbix.com/sdk@3b95c058c0e

go mod tidy

export GOOS=windows
export GOARCH=amd64
export CGO_ENABLED=0

go build -ldflags "-s -w" -o "${OUTPUT_NAME}" .

echo "Build completed: ${OUTPUT_NAME}"