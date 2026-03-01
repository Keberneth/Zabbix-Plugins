# Zabbix Agent2 Go Plugins

Zabbix Agent2 plugins written in Go.

Each plugin is compiled into a standalone binary:

- `.exe` for Windows
- Native binary for Linux

No PowerShell or shell scripts are required at runtime.

---

## 📦 Requirements

- Go 1.25 or newer
- Zabbix Agent 2 (7.x recommended)
- Windows Server 2016+ or modern Linux distribution (RHEL / SLES / Ubuntu)

---

# 🛠 Install Go

Install Go on the system used to compile the plugins.

## Windows

1. Download Go from: https://go.dev/dl/
2. Install using the MSI package.
3. Verify installation:

```powershell
go version
Linux (Example Manual Installation)
wget https://go.dev/dl/go1.25.7.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.25.7.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc
go version

Or install using your distribution package manager.

🔨 Building Plugins

Each plugin directory contains:

main.go

Windows build script (build_windows.ps1)

Linux build script (build_windows.sh or similar)

Plugin configuration file (.conf)

To build manually:

Build Windows Binary
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
go build -ldflags "-s -w" -o zabbix-agent2-<plugin>.exe .
Build Linux Binary
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
go build -ldflags "-s -w" -o zabbix-agent2-<plugin> .
Build Flags Explained
Flag	Purpose
GOOS	Target operating system
GOARCH	CPU architecture
CGO_ENABLED=0	Static binary (recommended)
-s -w	Strip debug symbols (smaller binary)
🪟 Install Plugin – Windows

Copy the compiled .exe file to:

C:\Program Files\Zabbix Agent 2\

Copy the corresponding .conf file to:

C:\Program Files\Zabbix Agent 2\conf.d\

Restart Zabbix Agent 2:

Restart-Service "Zabbix Agent 2"

Verify in the Zabbix Agent log that the plugin is loaded successfully.

🐧 Install Plugin – Linux

Copy the compiled plugin binary to:

/usr/sbin/zabbix-agent2-plugin/

Copy the corresponding .conf file to:

/etc/zabbix/conf.d/plugins/

Set correct ownership and permissions:

sudo chown root:zabbix /usr/sbin/zabbix-agent2-plugin/zabbix-agent2-<plugin>
sudo chmod 750 /usr/sbin/zabbix-agent2-plugin/zabbix-agent2-<plugin>

sudo chown root:zabbix /etc/zabbix/conf.d/plugins/<plugin>.conf
sudo chmod 640 /etc/zabbix/conf.d/plugins/<plugin>.conf

Restart Zabbix Agent 2:

sudo systemctl restart zabbix-agent2

Verify the agent log for successful plugin initialization.

🧪 Manual Testing

All plugins support standalone testing mode.

Windows
zabbix-agent2-<plugin>.exe --standalone --verbose
Linux
./zabbix-agent2-<plugin> --standalone --verbose

This will output the metric result directly to the console for validation.
