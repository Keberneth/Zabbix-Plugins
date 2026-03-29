Windows Failover Cluster - Zabbix Agent 2 Go plugin

Files
- main.go
- build_windows.ps1
- build_windows_from_linux.sh
- zabbix-agent2-WindowsFailoverCluster.conf
- template_windows_failover_cluster_agent2_go.yaml

Build
1. Put main.go in an empty folder.
2. Run build_windows.ps1 on Windows, or build_windows_from_linux.sh on Linux.
3. The build creates zabbix-agent2-windows-failover-cluster.exe.

Deploy on each cluster node
1. Copy zabbix-agent2-windows-failover-cluster.exe to:
   C:\Program Files\Zabbix Agent 2\
2. Copy zabbix-agent2-WindowsFailoverCluster.conf to:
   C:\Program Files\Zabbix Agent 2\zabbix_agent2.d\plugins.d\
3. Restart Zabbix Agent 2.
4. Test locally:
   zabbix_agent2.exe -c "C:\Program Files\Zabbix Agent 2\zabbix_agent2.conf" -t wfc.cluster.status

Template notes
- The template expects the custom key wfc.cluster.status.
- Cluster-level triggers are deduplicated by default and fire only from the current core cluster owner node.
- Event log items are still collected locally on each node.
- Tune behavior with the {$WFC.*} user macros in the template.
