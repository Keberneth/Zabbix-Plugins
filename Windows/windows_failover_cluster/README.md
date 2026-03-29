What is in the template

The template is Windows Failover Cluster by Zabbix agent2 Go. It uses one master item, wfc.cluster.status, then dependent items and LLD to avoid duplicate collection and reduce noise. It includes 38 base items, 4 discovery rules, 16 WSFC event-log items, and 13 tuning macros.

It discovers and monitors:

cluster roles / groups and which node owns each one

nodes, node state, vote weight, dynamic weight

quorum mode and forced quorum

witness presence, type, path/resource/account/endpoint, state, and derived health

cluster IP resources and their owner group / owner node

cluster networks and cluster network interfaces inside the JSON payload

local plugin collection state, including cached-vs-live mode and collection age

False-positive reduction

Because both nodes will use the same template, cluster-wide triggers are deduplicated by default. The template only raises cluster-level problems from the current core cluster owner node, controlled by {$WFC.DEDUP.CLUSTER_ALERTS}. Planned role moves are informational by default, and witness / node / group / IP triggers use time windows instead of firing on one bad sample. That keeps local node/plugin problems visible on each node while avoiding duplicate cluster alarms.

Macros you can tune

The main macros are:

{$WFC.COLLECT.INTERVAL}

{$WFC.NODATA.WINDOW}

{$WFC.CACHED.WARN.SEC}

{$WFC.NODE.PROBLEM.WINDOW}

{$WFC.GROUP.PROBLEM.WINDOW}

{$WFC.IP.PROBLEM.WINDOW}

{$WFC.WITNESS.PROBLEM.WINDOW}

{$WFC.WITNESS.REQUIRED}

{$WFC.DEDUP.CLUSTER_ALERTS}

{$WFC.GROUP.EXCLUDE.MATCHES}

{$WFC.GROUP.FAILOVER.INFO}

{$WFC.EVENTLOG.INTERVAL}

{$WFC.EVENTLOG.WINDOW}

Build and versioning

The build scripts pin golang.zabbix.com/sdk to d9643740a558, which matches the current release/7.0 SDK revision shown in Zabbix’s own repositories. Zabbix’s current requirements page also says Agent 2 and its plugins are built with Go, with Go 1.24.10 or later supported.

Config file notes

I kept the .conf style aligned with your example and added the requested testing comments at the top. The config uses:

Plugins.WindowsFailoverCluster.System.Path=C:\Program Files\Zabbix Agent 2\zabbix-agent2-windows-failover-cluster.exe

That follows Zabbix’s documented Plugins.<PluginName>.<Parameter> naming, and the plugin name must match the name used when registering metrics in the Go code.

Deployment

Build the plugin with one of the included scripts.

Copy zabbix-agent2-windows-failover-cluster.exe to C:\Program Files\Zabbix Agent 2\

Copy zabbix-agent2-WindowsFailoverCluster.conf to C:\Program Files\Zabbix Agent 2\zabbix_agent2.d\plugins.d\

Restart Zabbix Agent 2.

Test locally with:
zabbix_agent2.exe -c "C:\Program Files\Zabbix Agent 2\zabbix_agent2.conf" -t wfc.cluster.status

Import the template and link it to both cluster nodes.