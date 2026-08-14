# Microsoft Exchange SE Monitoring (Zabbix Agent 2 Go plugin)

Functional and health monitoring for **Microsoft Exchange SE (Subscription Edition)** Mailbox
servers. The native Zabbix "Microsoft Exchange Server 2016 by Zabbix agent active" template is
performance-counter based (DB I/O latency, RPC latency, LDAP timings, request rates) and cannot
see mail flow or health problems. This plugin fills that gap — including the classic failure
mode where **the mail queue only grows and nothing is sent**.

Both templates complement each other; link both to the host.

## What it monitors

| Area | Source | Alerts (defaults) |
|---|---|---|
| Transport queues | `Get-Queue` | Queue growing with outgoing rate 0 (HIGH), total depth warn/high, submission queue high, queues in Retry, poison/unreachable messages |
| Transport back pressure | `Get-ExchangeDiagnosticInfo` ResourceThrottling + event IDs 15004/15006/15007 | Medium (WARNING) / High (HIGH) pressure, disk/memory critical events |
| Certificates | `Get-ExchangeCertificate` | Per certificate: < 30 days (WARNING), < 7 days (HIGH). Includes self-signed/OAuth certs |
| Databases / DAG copies | `Get-MailboxDatabase -Status`, `Get-MailboxDatabaseCopyStatus` | Active copy not mounted (DISASTER), passive copy unhealthy (HIGH), copy/replay queue length, content index, size/whitespace trending |
| Backups | `Get-MailboxDatabase -Status` | Full backup older than 36 h (WARNING), never backed up (INFO) — per database ON/OFF via context macro |
| DAG replication | `Test-ReplicationHealth` | Failing checks (AVERAGE) with detail item |
| Managed Availability | `Get-HealthReport` | Per health set Unhealthy for 30 m (WARNING) |
| Exchange services | `Get-Service MSExchange*` + W3SVC/WinRM/IISADMIN | Auto-start service not running (HIGH) |
| Component states | `Get-ServerComponentState` | Component Inactive (AVERAGE), maintenance mode detection (INFO, suppresses component/vdir alerts) |
| Protocol health | `https://localhost/<vdir>/healthcheck.htm` | OWA, ECP, EWS, ActiveSync, Autodiscover, MAPI, OAB, RPC, PowerShell failing (HIGH) + response time |
| Database corruption | Event log ESE 474 / 447 | Page checksum mismatch / logical corruption (HIGH) |

All output is aggregated into **one JSON master item** (`exchange.se.status`); everything else is
dependent items and dependent low-level discovery, so the server is queried once per interval
(default 5 minutes).

## Read-only guarantee

The collector only runs `Get-*` cmdlets, `Test-ReplicationHealth` (a read-only diagnostic) and
local HTTP GET probes against `healthcheck.htm`. It never modifies Exchange or Windows
configuration and never sends test mail.

## Requirements

- Exchange SE (also works on Exchange 2019) **Mailbox role** server. Not intended for Edge
  Transport servers (AD-dependent areas would report collection errors there).
- Zabbix Agent 2 (7.x) running as **LocalSystem** on the Exchange server (default install).
  The collector loads the Exchange Management Shell snap-in, which works as LocalSystem on an
  Exchange server. If the agent runs as a custom service account instead, give that account the
  **View-Only Organization Management** RBAC role; the collector then falls back to a local
  remote-PowerShell session automatically.
- A full collection takes 30–60 seconds (snap-in load + cmdlets). The plugin enforces a 100 s
  PowerShell timeout, the template sets a 120 s item timeout.
- On transient failures the plugin serves the last good payload for up to 30 minutes and flags
  it (`CollectionMode: cached`), so the template warns without flapping.

## Build

```powershell
go build -ldflags "-s -w" -o zabbix-agent2-exchange-se.exe .
```

(or use the repo build scripts; requires Go 1.26.6+)

## Install

1. Copy `zabbix-agent2-exchange-se.exe` to `C:\Program Files\Zabbix Agent 2\`
2. Copy `zabbix-agent2-ExchangeSE.conf` to `C:\Program Files\Zabbix Agent 2\conf.d\`
3. Restart the Zabbix Agent 2 service.
   **Note:** Agent 2 aborts on startup if a configured plugin binary is missing — deploy the
   `.exe` before (or together with) the `.conf`.
4. Import `Microsoft Exchange SE by Zabbix Agent 2 Active.yaml` in the Zabbix frontend and link
   the template to the host.

## Test

```powershell
# Directly (run elevated; use PsExec -s to test exactly as LocalSystem):
& "C:\Program Files\Zabbix Agent 2\zabbix-agent2-exchange-se.exe" --standalone --verbose

# Through the agent:
& "C:\Program Files\Zabbix Agent 2\zabbix_agent2.exe" -c "C:\Program Files\Zabbix Agent 2\zabbix_agent2.conf" -t exchange.se.status
```

## Customization macros (most used)

Every threshold/window is a `{$EXCH.*}` macro — see the template for the full list of 36 with
descriptions. Highlights:

| Macro | Default | Purpose |
|---|---|---|
| `{$EXCH.QUEUE.STALL.WINDOW}` / `{$EXCH.QUEUE.STALL.MIN.DEPTH}` | `30m` / `50` | "Queue growing and nothing sent" detection |
| `{$EXCH.QUEUE.DEPTH.WARN}` / `{$EXCH.QUEUE.DEPTH.HIGH}` | `100` / `1000` | Total queue depth thresholds |
| `{$EXCH.CERT.WARN.DAYS}` / `{$EXCH.CERT.CRIT.DAYS}` | `30` / `7` | Certificate expiry (context per thumbprint) |
| `{$EXCH.DB.COPYQUEUE.MAX}` / `{$EXCH.DB.REPLAYQUEUE.MAX}` | `50` / `200` | DAG copy lag (context per database, e.g. `{$EXCH.DB.REPLAYQUEUE.MAX:"DB01"}` for lagged copies) |
| `{$EXCH.BACKUP.ALERT}` / `{$EXCH.BACKUP.MAX.AGE.HOURS}` | `1` / `36` | Backup alerting (context per database) |
| `{$EXCH.HEALTHSET.EXCLUDE.MATCHES}` | `^$` | Silence noisy Managed Availability health sets |
| `{$EXCH.CERT.SERVICES.MATCHES}` | `.*` | e.g. `IIS\|SMTP` to only monitor client-access/mail-flow certs |
| `{$EXCH.ALERT.IN.MAINTENANCE}` | `0` | Keep component/vdir alerts firing during maintenance mode |
| `{$EXCH.COLLECT.INTERVAL}` | `5m` | Collection interval |

## Troubleshooting

- **"Unable to load the Exchange Management Shell snap-in..."** — the agent service account can
  load neither the snap-in nor a remote session. Check the service account (see Requirements).
- **Collector partial errors trigger** — one collection area failed; the item value lists which
  (for example `BackPressure: ...` while the transport service is restarting). Other areas keep
  working.
- **Plugin serving cached data** — live collection keeps failing; see the `Collection error`
  item for the live error.
