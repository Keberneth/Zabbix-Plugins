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
  primary metric for triggers. Before the current month's Patch Tuesday (the
  second Tuesday of the month) the CU has not been released by Microsoft yet, so
  the plugin reports 0 (not missing) until then to avoid false alarms early in
  the month. See "Patch Tuesday caveat" below.
- wlu.update.installed[YYYY-MM]
  Same check against an explicit month, e.g. wlu.update.installed[2026-04].
- wlu.update.installed.previous
  Returns 0 if the previous month's CU is installed, 1 otherwise. Useful for
  an escalated trigger once Patch Tuesday for last month has come and gone.
- wlu.update.status
  Returns a JSON snapshot:

    {
      "Timestamp": "2026-04-29T12:34:56.789+02:00",
      "LocalNode": "WIN-HOST",
      "MonthChecked": "2026-04",
      "Installed": 0,
      "RawInstalled": 0,
      "PatchTuesday": "2026-04-14",
      "ReleaseDue": true,
      "Suppressed": false,
      "MatchedTitles": ["2026-04 Cumulative Update for ..."],
      "InstalledOn": "2026-04-10T03:15:22+02:00",
      "KBs": ["KB5082142"],
      "HistoryCount": 412,
      "Source": "WindowsUpdateCOM",
      "ErrorMessage": null,
      "CollectorVersion": "1.2.0",
      "CollectionMode": "live",
      "CollectionAgeSeconds": 0
    }

  Patch Tuesday fields:
  - RawInstalled  - the raw detection (0 installed, 1 not found) before any
                    suppression. "Installed" is the effective value the trigger
                    reads; it equals RawInstalled except when Suppressed is true.
  - PatchTuesday  - the second Tuesday (yyyy-MM-dd) of MonthChecked.
  - ReleaseDue    - true once the local clock reaches PatchTuesday.
  - Suppressed    - true when a "missing" detection was reported as installed
                    (Installed=0) because the CU is not due yet. A collector
                    error (Source="Error") is never suppressed, so a broken
                    Windows Update check still surfaces as missing.

  The month reasoned about comes from MonthChecked (the collector's own clock),
  so the decision can never drift to the wrong month at a month boundary.

  On transient collection failures the plugin serves the last good payload for
  up to 30 minutes and sets "CollectionMode" to "cached" plus
  "CollectionError" to the live error.

- wlu.update.status.previous
  Same JSON snapshot, but for the previous month.

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

Trigger examples

  Last value of wlu.update.installed equals 1
    -> Information: "Current month CU missing on {HOST.NAME}"
  Last value of wlu.update.installed.previous equals 1
    -> Average:     "Previous month CU missing on {HOST.NAME}"

The bundled template "Windows Latest CU Update Zabbix Agent Active" already
wires both triggers with these severities.

Patch Tuesday caveat

Microsoft releases the monthly CU on the second Tuesday. Early in the month the
current CU has not been released yet. The plugin handles this itself: for the
current month it reports wlu.update.installed=0 (not missing) until the local
clock reaches that month's Patch Tuesday (the second Tuesday at 00:00 local
time). From Patch Tuesday onward it reports the real detection. This means the
"Current month CU missing" trigger no longer fires from the 1st of the month;
it can only fire once the CU is actually due. No template change is required -
the suppression is applied to the metric value the trigger reads.

The suppression is keyed on the month being checked, so it never hides a real
gap: the previous-month metric (wlu.update.installed.previous) and explicit
past-month checks (wlu.update.installed[YYYY-MM]) have a Patch Tuesday in the
past and are reported as-is. The JSON snapshot exposes the raw detection and
the decision via RawInstalled, PatchTuesday, ReleaseDue and Suppressed.

The current-month trigger ships at Information severity; the previous-month
trigger ships at Average severity because by then Patch Tuesday has long passed
and a missing CU represents real exposure.

Permissions

The plugin runs as the Zabbix Agent 2 service account (LocalSystem by
default). LocalSystem can query Microsoft.Update.Session history without
extra rights. If you change the agent service account, ensure it has rights
to use the Windows Update Agent COM API.
