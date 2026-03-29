# Zabbix AD DS User Monitoring

Zabbix Agent 2 plugin written in Go and a matching Zabbix template for monitoring Active Directory user state.

## What it monitors

The plugin and template cover these user conditions:

- locked-out users
- disabled users older than a defined number of days
- users whose passwords expire within a defined number of days
- users whose accounts expire within a defined number of days

The template uses active master items that return JSON, then dependent discovery rules, item prototypes, and trigger prototypes to create per-user monitoring and alerts.

## Files in this folder

- `AD DS User Monitoring Agent Active.yaml` - Zabbix template
- `main.go` - Go source for the Agent 2 plugin
- `build_windows.ps1` - build script for Windows
- `build_windows_from_linux.sh` - cross-build script from Linux
- `zabbix-agent2-ADUserMonitoring.conf` - Zabbix Agent 2 plugin .conf "C:\Program Files\Zabbix Agent 2\zabbix_agent2.d\plugins.d"

## Requirements

- Windows AD serrver running Zabbix Agent 2 or server that can query the AD
- Go toolchain on computer that will build the plugin .exe file

## Build

### Build on Windows

Place build_windows.ps1 file and main.go file in the same folder.<br>
Run from PowerShell in this folder:<br>

```powershell
go mod init adusermonitoring
go get golang.zabbix.com/sdk@d9643740a558
go mod tidy
go build -trimpath -ldflags "-s -w" -o zabbix-agent2-ad-user-monitoring.exe .
```
<br>
Or use:
<br>
```powershell
.\build_windows.ps1
```

### Cross-build from Linux

Place build_windows_from_linux.sh file and main.go file in the same folder.<br>
Run from this folder:

```bash
go mod init adusermonitoring
go get golang.zabbix.com/sdk@d9643740a558
go mod tidy
export GOOS=windows
export GOARCH=amd64
export CGO_ENABLED=0
go build -trimpath -ldflags "-s -w" -o zabbix-agent2-ad-user-monitoring.exe .
```

Or use:

```bash
./build_windows_from_linux.sh
```

## Installation

1. Build `zabbix-agent2-ad-user-monitoring.exe`.
2. Copy the executable to:

   ```text
   C:\Program Files\Zabbix Agent 2\zabbix-agent2-ad-user-monitoring.exe
   ```

3. Place zabbix-agent2-ADUserMonitoring.conf in "C:\Program Files\Zabbix Agent 2\zabbix_agent2.d\plugins.d"

4. Restart Zabbix Agent 2.
5. Import `AD DS User Monitoring Agent Active.yaml` into Zabbix.
6. Link the template to the Windows AD host.
7. Set the template macros to match your environment.

## Template macros

| Macro | Default | Purpose |
|---|---:|---|
| `{$AD.ACCOUNT.EXPIRES.IN.DAYS}` | `7` | Alert window for accounts that are about to expire |
| `{$AD.DISABLED.OLDER.THAN.DAYS}` | `30` | Threshold for disabled users |
| `{$AD.LDAP.SERVER}` | empty | Optional domain controller hostname or FQDN |
| `{$AD.PASSWORD.EXPIRES.IN.DAYS}` | `7` | Alert window for passwords that are about to expire |
| `{$AD.SEARCH.BASES}` | empty | Base DN scope for the search |

### Search base notes

- Leave `{$AD.SEARCH.BASES}` empty to search the whole domain.
- You can limit the search to one or more OUs or containers. (Will be faster and take less resources on AD server)
- Multiple search bases can be separated with `;` or `|`.

Example:

```text
OU=Users,DC=example,DC=com;OU=Admins,DC=example,DC=com
```

## Standalone testing

The binary supports a simple standalone mode for testing.

Example:

```powershell
.\zabbix-agent2-ad-user-monitoring.exe --standalone PasswordExpiringUsers 14
```

Useful key formats:

```text
LockedOutUsers[searchBases,server]
DisabledUsers[days,searchBases,server]
PasswordExpiringUsers[days,searchBases,server]
UsersAboutToBeDisabled[days,searchBases,server]
```

## Notes

- LDAP binding uses the current Windows security context, so the Agent 2 service account should have the required read access in AD.
- If `{$AD.LDAP.SERVER}` is empty, Windows chooses the server.
- The template uses Zabbix Agent active checks for the master items. Because of that, `Execute now` does not work for those active master items or their dependent discovery rules.
- The `AD Disabled Users JSON` master item is included but is disabled by default in the template.

## Version

Template export version: `7.0`
