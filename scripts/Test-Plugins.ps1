[CmdletBinding()]
param(
    [ValidateSet('current', 'linux', 'windows')]
    [string] $Platform = 'current',

    [switch] $SkipBuild,
    [switch] $SkipVet,
    [switch] $Race
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$catalogPath = Join-Path $repoRoot 'plugins.json'
$catalog = Get-Content -LiteralPath $catalogPath -Raw | ConvertFrom-Json

$nativePlatform = if ($IsWindows) { 'windows' } else { 'linux' }
if ($Platform -eq 'current') {
    $Platform = $nativePlatform
}
elseif ($Platform -ne $nativePlatform) {
    throw "Platform '$Platform' requires a native $Platform runner; this host is $nativePlatform."
}

$plugins = @($catalog.plugins | Where-Object { $_.goos -eq $Platform })
if ($plugins.Count -eq 0) {
    throw "No $Platform plugins are defined in plugins.json."
}

$duplicateBinaries = @($catalog.plugins | Group-Object binary | Where-Object Count -gt 1)
$duplicateDirectories = @($catalog.plugins | Group-Object dir | Where-Object Count -gt 1)
if ($duplicateBinaries.Count -gt 0 -or $duplicateDirectories.Count -gt 0) {
    throw 'plugins.json contains duplicate binary names or module directories.'
}

$expectedGo = '1.26.7'
$expectedSdk = [string] $catalog.zabbix.sdk
$outputRoot = Join-Path ([IO.Path]::GetTempPath()) ('zabbix-plugin-validation-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $outputRoot | Out-Null

function Invoke-Go {
    param([Parameter(Mandatory)][string[]] $Arguments)

    & go @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "go $($Arguments -join ' ') failed with exit code $LASTEXITCODE."
    }
}

try {
    foreach ($entry in $plugins) {
        $moduleDir = Join-Path $repoRoot ([string] $entry.dir)
        $goModPath = Join-Path $moduleDir 'go.mod'
        $goMod = Get-Content -LiteralPath $goModPath -Raw

        if ($goMod -notmatch "(?m)^go $([regex]::Escape($expectedGo))\r?$") {
            throw "$($entry.dir)/go.mod must declare 'go $expectedGo'."
        }
        if ($goMod -match '(?m)^toolchain ') {
            throw "$($entry.dir)/go.mod must not override the repository toolchain baseline."
        }
        if ($goMod -notmatch "golang\.zabbix\.com/sdk $([regex]::Escape($expectedSdk))") {
            throw "$($entry.dir)/go.mod does not use the catalogued Agent 2 SDK revision."
        }

        foreach ($relativeAsset in @([string] $entry.conf, [string] $entry.template)) {
            if (-not (Test-Path -LiteralPath (Join-Path $moduleDir $relativeAsset) -PathType Leaf)) {
                throw "$($entry.dir) is missing catalogued asset '$relativeAsset'."
            }
        }

        $conf = Get-Content -LiteralPath (Join-Path $moduleDir ([string] $entry.conf)) -Raw
        if ($conf -notmatch [regex]::Escape([string] $entry.binary)) {
            throw "$($entry.dir)/$($entry.conf) does not reference catalogued binary '$($entry.binary)'."
        }

        $template = Get-Content -LiteralPath (Join-Path $moduleDir ([string] $entry.template)) -Raw
        if ($template -notmatch "(?m)^  version: '7\.0'\r?$" ) {
            throw "$($entry.dir)/$($entry.template) is not a Zabbix 7.0 export."
        }

        $templateUuids = @([regex]::Matches($template, '(?m)^[ \t]*-?[ \t]*uuid:[ \t]*([0-9a-fA-F]+)[ \t]*\r?$'))
        foreach ($uuid in $templateUuids) {
            if ($uuid.Groups[1].Value.Length -ne 32) {
                throw "$($entry.dir)/$($entry.template) contains a UUID that is not 32 hexadecimal characters."
            }
        }
        $duplicateUuids = @($templateUuids | ForEach-Object { $_.Groups[1].Value.ToLowerInvariant() } |
            Group-Object | Where-Object Count -gt 1)
        if ($duplicateUuids.Count -gt 0) {
            throw "$($entry.dir)/$($entry.template) contains duplicate UUIDs."
        }

        Write-Host "==> $($entry.dir)" -ForegroundColor Cyan
        Push-Location $moduleDir
        try {
            Invoke-Go -Arguments @('mod', 'verify')

            $testArgs = @('test')
            if ($Race) { $testArgs += '-race' }
            $testArgs += './...'
            Invoke-Go -Arguments $testArgs

            if (-not $SkipVet) {
                Invoke-Go -Arguments @('vet', '-unsafeptr=false', './...')
            }

            if (-not $SkipBuild) {
                $output = Join-Path $outputRoot ([string] $entry.binary)
                # Validation may run from a sandbox-owned or copied worktree.
                # Release CI performs the authoritative VCS-stamped build.
                Invoke-Go -Arguments @('build', '-buildvcs=false', '-trimpath', '-o', $output, '.')
            }
        }
        finally {
            Pop-Location
        }
    }

    Write-Host "Validated $($plugins.Count) $Platform plugins." -ForegroundColor Green
}
finally {
    if (Test-Path -LiteralPath $outputRoot) {
        Remove-Item -LiteralPath $outputRoot -Recurse -Force
    }
}
