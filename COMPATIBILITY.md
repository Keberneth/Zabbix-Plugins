# Zabbix Agent 2 compatibility

This repository targets **Zabbix Agent 2 7.0.28** on `linux/amd64` and
`windows/amd64`.

All modules intentionally pin:

- Go language/toolchain baseline: `1.25.9`
- Agent 2 SDK: `golang.zabbix.com/sdk v1.2.2-0.20260513084934-aa676675694e`
- Loadable-plugin protocol reported by that SDK: `6.4.0`

The SDK revision is the same revision used by the official MSSQL loadable
plugin sources published for Zabbix 7.0.28 and 7.0.29. The 7.0.29 source
change log records no code change from 7.0.28, but production deployments
should still keep the Agent 2 executable and loadable plugins on the same
Zabbix patch whenever possible.

## Upgrade rule

Do not update `golang.zabbix.com/sdk` as an ordinary dependency bump. For an
Agent 2 upgrade:

1. Download the official loadable-plugin source archive for the exact target
   Zabbix patch.
2. Copy its Go version and exact SDK pseudo-version into every module.
3. Run `scripts/Test-Plugins.ps1` natively on Linux and Windows.
4. Build all plugins and smoke-test them through that exact Agent 2 version.
5. Upgrade the Agent 2 executable and plugins together.

The current `master` branch of an official plugin may target Zabbix 8.0 and a
new startup API. It is an architectural reference, not a safe source-level
dependency for this 7.0 branch.
