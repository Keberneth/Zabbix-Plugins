# Zabbix Agent2 Go Plugins

Production-ready Zabbix Agent2 plugins written in Go.

Each plugin is compiled into a standalone binary:

-   `.exe` for Windows\
-   Native binary for Linux

No PowerShell or shell scripts are required at runtime.

------------------------------------------------------------------------

## 📦 Requirements

-   Go 1.25 or newer\
-   Zabbix Agent 2 (7.x recommended)\
-   Windows Server 2016+ or modern Linux distribution

------------------------------------------------------------------------

# 🛠 Install Go

Install Go on the system used to compile the plugins.

## 🪟 Windows

1.  Download Go from:\
    https://go.dev/dl/

2.  Install using the MSI package.

3.  Verify installation:

``` powershell
go version
```

Expected output example:

``` text
go version go1.25.7 windows/amd64
```

------------------------------------------------------------------------

## 🐧 Linux

Example manual installation:

``` bash
wget https://go.dev/dl/go1.25.7.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.25.7.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc
go version
```

------------------------------------------------------------------------

# 🔨 Building Plugins

Each plugin directory contains:

-   `main.go`
-   Windows build script (`build_windows.ps1`)
-   Linux build script (`build_windows.sh`)
-   Plugin configuration file (`.conf`)

## Build Windows Binary

Run the script in the plugin folder to get the binary plugin file for Zabbix<br>

``` bash
powershell.exe -NoProfile -ExecutionPolicy Bypass -File build_windows.ps1
```

## Build Linux Binary

``` bash
./build_linux.sh
```

------------------------------------------------------------------------

# 🪟 Install Plugin -- Windows

1.  Copy the compiled `.exe` file to:

```{=html}
    C:\Program Files\Zabbix Agent 2\
```

2.  Copy the corresponding `.conf` file to:

```{=html}
    C:\Program Files\Zabbix Agent 2\conf.d\
```

3.  Restart Zabbix Agent 2:

``` powershell
Restart-Service "Zabbix Agent 2"
```

------------------------------------------------------------------------

# 🐧 Install Plugin -- Linux

1.  Copy the compiled plugin binary to:

```{=html}
    /usr/sbin/zabbix-agent2-plugin/
```

2.  Copy the corresponding `.conf` file to:

```{=html}
    /etc/zabbix/conf.d/plugins/
```

3.  Set secure permissions:

``` bash
sudo chown root:zabbix /usr/sbin/zabbix-agent2-plugin/zabbix-agent2-<plugin>
sudo chmod 750 /usr/sbin/zabbix-agent2-plugin/zabbix-agent2-<plugin>

sudo chown root:zabbix /etc/zabbix/conf.d/plugins/<plugin>.conf
sudo chmod 640 /etc/zabbix/conf.d/plugins/<plugin>.conf
```

4.  Restart Agent:

``` bash
sudo systemctl restart zabbix-agent2
```

------------------------------------------------------------------------

# 🧪 Manual Testing

All plugins support standalone testing mode.

## Windows

``` powershell
zabbix-agent2-<plugin>.exe --standalone --verbose
```

## Linux

``` bash
./zabbix-agent2-<plugin> --standalone --verbose
```
