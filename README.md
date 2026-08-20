# Zabbix Agent 2 Go plugins

Loadable Go plugins and separate active templates for Zabbix Agent 2. The
current compatibility baseline is **Zabbix Agent 2 7.0.28** on
`windows/amd64` and `linux/amd64`.

The project keeps each collector as its own executable and each template as a
separate importable YAML file. See [COMPATIBILITY.md](COMPATIBILITY.md) before
changing Go or the Zabbix SDK dependency.

## Compatibility

- Go `1.25.9`
- Zabbix Agent 2 `7.0.28`
- Agent 2 plugin protocol `6.4.0`
- SDK `golang.zabbix.com/sdk v1.2.2-0.20260513084934-aa676675694e`

The SDK is deliberately pinned to the revision used by the official MSSQL
plugin sources for Zabbix 7.0.28/7.0.29. Renovate is prevented from updating
that dependency automatically because the plugin SDK and Agent 2 patch must be
reviewed together.

## Release contents

GitHub releases retain the individual binaries and also provide one deployment
archive per plugin. An archive contains:

- the executable;
- its Agent 2 `.conf` file;
- its separate Zabbix template;
- plugin or repository documentation;
- Go build information; and
- a CycloneDX SBOM.

`SHA256SUMS.txt` covers every published file. Tests stay in source control and
are intentionally excluded from deployment archives.

The authoritative list of modules, binary names, configuration files, and
templates is [plugins.json](plugins.json). CI, validation, and packaging all
read that catalog so those lists cannot drift independently.

## Build and validate

Install Go 1.25.9, clone the repository, and run the native validation script:

```powershell
pwsh -File .\scripts\Test-Plugins.ps1
```

Run it once on Windows and once on Linux. It verifies every native module's
catalog assets and pinned SDK, then runs `go mod verify`, `go test`, `go vet`,
and a clean build. CI performs the native test pass on both operating systems
and cross-builds all release binaries.

To build one plugin manually:

```powershell
Set-Location .\Windows\WindowsLatestUpdate
go build -trimpath -o zabbix-agent2-windows-latest-update.exe .
```

```bash
cd Linux/linux_ntp_sync
go build -trimpath -o zabbix-agent2-linux-ntp-sync .
```

There are no per-plugin build scripts; the Go command and the repository
validation script are the supported local build paths.

## Install on Windows

1. Copy the `.exe` to `C:\Program Files\Zabbix Agent 2\`.
2. Copy its `.conf` into a directory included by `zabbix_agent2.conf`, normally
   `C:\Program Files\Zabbix Agent 2\conf.d\`.
3. Import the bundled YAML template and link it to the host.
4. Restart and test Agent 2:

```powershell
Restart-Service 'Zabbix Agent 2'
& 'C:\Program Files\Zabbix Agent 2\zabbix_agent2.exe' `
  -c 'C:\Program Files\Zabbix Agent 2\zabbix_agent2.conf' `
  -t '<item.key>'
```

Use the exact key shown in the plugin's `.conf` comments. Expensive collectors
may need to be tested as the Agent 2 service account to reproduce its
permissions and PowerShell/registry access.

## Install on Linux

1. Copy the executable to `/usr/sbin/zabbix-agent2-plugin/`.
2. Copy its `.conf` into the plugin include directory used by your package,
   commonly `/etc/zabbix/zabbix_agent2.d/plugins.d/`.
3. Import the bundled YAML template and link it to the host.
4. Apply permissions, restart, and test:

```bash
sudo chown root:zabbix /usr/sbin/zabbix-agent2-plugin/zabbix-agent2-<plugin>
sudo chmod 0750 /usr/sbin/zabbix-agent2-plugin/zabbix-agent2-<plugin>
sudo systemctl restart zabbix-agent2
sudo zabbix_agent2 -c /etc/zabbix/zabbix_agent2.conf -t '<item.key>'
```

Distribution paths vary, so confirm the `Include=` directives in the active
Agent 2 configuration rather than assuming one directory layout.

## Standalone diagnostics

The shipped `.conf` file documents the supported standalone/self-test command
for that plugin. Options differ by collector; do not assume every binary
supports `--verbose` or the same arguments. Standalone output is diagnostic.
The authoritative monitoring result is the value returned through Agent 2.
