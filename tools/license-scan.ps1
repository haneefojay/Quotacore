[CmdletBinding()]
param(
    [string] $Binary = 'bin/quotacore',
    [string] $Allowlist = 'tools/license-allowlist.txt',
    [string] $SbomPath = '',
    [switch] $Quiet
)

# NFR-C4: no copyleft dependency in the runtime path without an explicit
# decision record. The runtime path is read from the built binary rather than
# from the module graph, because `go version -m` lists exactly the modules
# linked into the binary and a module that is only a test dependency of a
# dependency is not in the product. Licence text is read from the module cache
# via `go list -m -json`, never by reimplementing Go's path escaping.

$ErrorActionPreference = 'Stop'

function Write-Report($message) {
    if (-not $Quiet) { Write-Output $message }
}

$copyleftMarkers = @(
    'GNU AFFERO GENERAL PUBLIC LICENSE',
    'GNU LESSER GENERAL PUBLIC LICENSE',
    'GNU GENERAL PUBLIC LICENSE',
    'Mozilla Public License',
    'Common Development and Distribution License',
    'European Union Public Licence',
    'Server Side Public License',
    'Business Source License'
)

# Classification matches on the text of the grant, not on the SPDX name, because
# most licence files do not contain their own SPDX identifier. Go's BSD-3 text
# never says "BSD 3-Clause" and Uber's MIT text never says "MIT License", and a
# scan that reports NOASSERTION for those is a scan nobody will trust. A phrase
# that is not in the file cannot classify it, and an unclassified licence fails
# the build: a licence nobody can name is not a licence anyone has accepted.
$permissiveRules = @(
    @{ Id = 'Apache-2.0';   All = @('apache license', 'version 2.0, january 2004') },
    @{ Id = 'MIT';          All = @('permission is hereby granted, free of charge', 'the software is provided "as is"') },
    @{ Id = 'ISC';          All = @('permission to use, copy, modify, and/or distribute this software for any purpose') },
    @{ Id = '0BSD';         All = @('with or without fee') },
    @{ Id = 'Unlicense';    All = @('released into the public domain') },
    @{ Id = 'PostgreSQL';   All = @('postgresql license') },
    @{ Id = 'BSD-3-Clause'; All = @('redistribution and use in source and binary forms', 'neither the name of'); Absent = @('mozilla public license') },
    @{ Id = 'BSD-2-Clause'; All = @('redistribution and use in source and binary forms'); Absent = @('neither the name of', 'mozilla public license') }
)

if (-not (Test-Path -LiteralPath $Binary)) {
    Write-Error "binary $Binary does not exist; run 'make build' first"
    exit 2
}

$version = & go version -m $Binary 2>&1
if ($LASTEXITCODE -ne 0) {
    Write-Error "go version -m $Binary failed: $version"
    exit 2
}

$modules = @()
foreach ($line in $version) {
    if ($line -match '^\s*dep\s+(\S+)\s+(\S+)') {
        $modules += [pscustomobject]@{ Path = $Matches[1]; Version = $Matches[2] }
    }
}
if ($modules.Count -eq 0) {
    Write-Error "no modules found in $Binary; this is not a Go binary, or it was built without dependency information"
    exit 2
}

$allowed = @{}
if (Test-Path -LiteralPath $Allowlist) {
    foreach ($line in Get-Content -LiteralPath $Allowlist) {
        $trimmed = $line.Trim()
        if ($trimmed -eq '' -or $trimmed.StartsWith('#')) { continue }
        $parts = $trimmed -split '\s+'
        if ($parts.Count -lt 2) {
            Write-Error "allowlist entry '$trimmed' is not '<module path> <ADR-nnnn>'"
            exit 2
        }
        if ($parts[1] -notmatch '^ADR-\d{4}$') {
            Write-Error "allowlist entry '$trimmed' cites '$($parts[1])', which is not a decision record; NFR-C4 allows a copyleft dependency only with one"
            exit 2
        }
        $allowed[$parts[0]] = $parts[1]
    }
}

$results = @()
$failures = @()
foreach ($module in $modules) {
    # Queried by path rather than by path@version on purpose: a version-pinned
    # query asks about the upstream module, and a `replace` has no upstream
    # directory, so it returns no Dir and the licence of every replaced module
    # would be unreadable.
    $meta = & go list -m -json $module.Path 2>$null | ConvertFrom-Json
    $dir = $null
    if ($meta -and $meta.Replace -and $meta.Replace.Dir) { $dir = $meta.Replace.Dir }
    elseif ($meta -and $meta.Dir) { $dir = $meta.Dir }
    if (-not $dir) {
        $results += [pscustomobject]@{ Path = $module.Path; Version = $module.Version; License = 'NOASSERTION'; Text = ''; Source = 'module not in the cache' }
        $failures += "$($module.Path): no licence could be read, so it is not demonstrably permissive"
        continue
    }
    $file = Get-ChildItem -LiteralPath $dir -File -ErrorAction SilentlyContinue |
        Where-Object { $_.Name -match '^(LICENSE|LICENCE|COPYING)' } |
        Select-Object -First 1
    if (-not $file) {
        $results += [pscustomobject]@{ Path = $module.Path; Version = $module.Version; License = 'NOASSERTION'; Text = ''; Source = 'no licence file' }
        $failures += "$($module.Path): no licence file, so it is not demonstrably permissive"
        continue
    }
    $text = (Get-Content -LiteralPath $file.FullName -Raw -ErrorAction SilentlyContinue)
    if ($null -eq $text) { $text = '' }
    $upper = $text.ToUpperInvariant()
    $isCopyleft = $false
    foreach ($marker in $copyleftMarkers) {
        if ($upper.Contains($marker)) { $isCopyleft = $true }
    }
    $id = 'NOASSERTION'
    $matched = ''
    foreach ($rule in $permissiveRules) {
        $hit = $true
        foreach ($phrase in $rule.All) {
            if (-not $upper.Contains($phrase.ToUpperInvariant())) { $hit = $false }
        }
        if ($rule.Absent) {
            foreach ($phrase in $rule.Absent) {
                if ($upper.Contains($phrase.ToUpperInvariant())) { $hit = $false }
            }
        }
        if ($hit) { $id = $rule.Id; $matched = $rule.Id; break }
    }
    $source = $file.Name
    if ($isCopyleft) {
        $id = 'COPYLEFT'
        $matched = 'a copyleft licence'
        if ($allowed.ContainsKey($module.Path)) {
            $id = $allowed[$module.Path]
            $source = "$($file.Name), covered by $($allowed[$module.Path])"
        }
    }
    $results += [pscustomobject]@{ Path = $module.Path; Version = $module.Version; License = $id; Text = $matched; Source = $source }
    if ($isCopyleft -and -not $allowed.ContainsKey($module.Path)) {
        $failures += "$($module.Path) $($module.Version) is $matched and is not covered by a decision record; add it to $Allowlist with the ADR that accepts it, or do not depend on it"
    }
    if (-not $isCopyleft -and $id -eq 'NOASSERTION') {
        $failures += "$($module.Path) $($module.Version): $($file.Name) does not match any licence this scan recognises, so it is not demonstrably permissive; the scan fails closed rather than passing an unclassified licence"
    }
}

$results | Sort-Object Path | ForEach-Object { Write-Report ("  {0,-46} {1,-14} {2}" -f $_.Path, $_.License, $_.Text) }

if ($SbomPath -ne '') {
    $dir = Split-Path -Parent $SbomPath
    if ($dir -ne '' -and -not (Test-Path -LiteralPath $dir)) {
        New-Item -ItemType Directory -Path $dir -Force | Out-Null
    }
    $components = @()
    foreach ($r in ($results | Sort-Object Path)) {
        $licences = @()
        if ($r.License -ne 'NOASSERTION' -and $r.License -ne 'COPYLEFT') {
            $licences = @(@{ license = @{ id = $r.License } })
        } elseif ($r.License -eq 'COPYLEFT') {
            $licences = @(@{ license = @{ id = $r.License } }, @{ license = @{ name = 'see ' + $r.Source } })
        }
        $escaped = ($r.Path -creplace '([A-Z])', '!$1')
        $components += [ordered]@{
            type    = 'library'
            name    = $r.Path
            version = $r.Version
            purl    = "pkg:golang/$escaped@$($r.Version)"
            licenses = $licences
            evidence = @(@{ identity = @{ field = 'go.version -m' }; description = $r.Source })
        }
    }
    $sbom = [ordered]@{
        bomFormat   = 'CycloneDX'
        specVersion = '1.6'
        version     = 1
        metadata    = [ordered]@{
            component = [ordered]@{
                type    = 'application'
                name    = 'github.com/quotacore/quotacore'
                version = 'v0.1.0-dev'
            }
            tools = @(
                @{ vendor = 'quotacore'; name = 'tools/license-scan.ps1' }
            )
            properties = @(
                @{ name = 'quotacore:source:runtime-path'; value = 'modules reported by go version -m on the built binary' }
            )
        }
        components = $components
    }
    $json = $sbom | ConvertTo-Json -Depth 12
    [System.IO.File]::WriteAllText((Join-Path (Get-Location) $SbomPath), $json, (New-Object System.Text.UTF8Encoding($false)))
    Write-Report "wrote $SbomPath with $(@($components).Count) components"
}

if ($failures.Count -gt 0) {
    Write-Output "FAIL - $($failures.Count) licence problem(s) in the runtime path:"
    foreach ($f in $failures) { Write-Output "  - $f" }
    exit 1
}

Write-Report "PASS - $($results.Count) modules in the runtime path, no unlicensed copyleft dependency"
exit 0
