Windows Latest Update - Zabbix Agent 2 Go plugin

What it does

The plugin reports whether the current month's Windows Cumulative Update is
installed on the local host. It shells out to PowerShell, queries the Windows
Update history COM API (Microsoft.Update.Session), filters out Defender
signatures, MSRT, .NET Framework, and Servicing Stack entries, and looks for a
successfully-installed CU whose title matches the current YYYY-MM and any of:

- "Windows Server" (legacy naming)
- "Microsoft server operating system" (Windows Server 2022 / 2025 naming)
- "Windows 10" or "Windows 11" (client OS)

Both ResultCode 2 (Succeeded) and 3 (SucceededWithErrors) count as installed.

Items

- wlu.update.installed
  Returns 0 if the current month's CU is installed, 1 otherwise. This is the
  primary metric for triggers.
- wlu.update.installed[YYYY-MM]
  Same check against an explicit month, e.g. wlu.update.installed[2026-04].
- wlu.update.status
  Returns a JSON snapshot:

    {
      "Timestamp": "2026-04-29T12:34:56.789+02:00",
      "LocalNode": "WIN-HOST",
      "MonthChecked": "2026-04",
      "Installed": 0,
      "MatchedTitles": ["2026-04 Cumulative Update for ..."],
      "InstalledOn": "2026-04-10T03:15:22+02:00",
      "KBs": ["KB5082142"],
      "HistoryCount": 412,
      "Source": "WindowsUpdateCOM",
      "ErrorMessage": null,
      "CollectorVersion": "1.0.0",
      "CollectionMode": "live",
      "CollectionAgeSeconds": 0
    }

  On transient collection failures the plugin serves the last good payload for
  up to 30 minutes and sets "CollectionMode" to "cached" plus
  "CollectionError" to the live error.

Build

1. Put main.go in this folder.
2. Run build_windows.ps1 on Windows, or build_windows_from_linux.sh on Linux.
3. The build produces zabbix-agent2-windows-latest-update.exe.

The build scripts pin golang.zabbix.com/sdk to commit d9643740a558, matching
the release/7.0 SDK revision used by the upstream Zabbix examples. Go 1.24.10
or later is supported by current Zabbix Agent 2 plugin requirements.

Deploy

1. Copy zabbix-agent2-windows-latest-update.exe to:
   C:\Program Files\Zabbix Agent 2\
2. Copy zabbix-agent2-WindowsLatestUpdate.conf to:
   C:\Program Files\Zabbix Agent 2\zabbix_agent2.d\plugins.d\
3. Restart Zabbix Agent 2.
4. Test locally:
   zabbix_agent2.exe -c "C:\Program Files\Zabbix Agent 2\zabbix_agent2.conf" -t wlu.update.installed
   zabbix_agent2.exe -c "C:\Program Files\Zabbix Agent 2\zabbix_agent2.conf" -t wlu.update.status

Standalone self-test (without the agent)

  & "C:\Program Files\Zabbix Agent 2\zabbix-agent2-windows-latest-update.exe" --standalone --verbose
  & "C:\Program Files\Zabbix Agent 2\zabbix-agent2-windows-latest-update.exe" --standalone --verbose --month 2026-04

Trigger example

  Last value of wlu.update.installed equals 1 for 24h
    -> "Current month's Windows Cumulative Update is missing on {HOST.NAME}"

Patch Tuesday caveat

Microsoft releases the monthly CU on the second Tuesday. Early in the month
the current CU has not been released yet, so a naive trigger on
wlu.update.installed=1 will fire from the 1st until the host installs the new
CU. Tune the trigger with a time window (for example, only alert after day
15), or point templates at a previous month with the parameterized key.

Permissions

The plugin runs as the Zabbix Agent 2 service account (LocalSystem by
default). LocalSystem can query Microsoft.Update.Session history without
extra rights. If you change the agent service account, ensure it has rights
to use the Windows Update Agent COM API.
