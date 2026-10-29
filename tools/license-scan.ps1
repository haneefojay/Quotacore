[CmdletBinding()]
param(
    [string] $Binary = 'bin/quotacore',
    [string] $Allowlist = 'tools/license-allowlist.txt',
    [string] $SbomPath = '',
    [string] $Image = '',
    [string] $Dockerfile = 'Dockerfile',
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

# With -Image the runtime path is the whole shipped image, not the binary alone (IP-15 D18,
# NFR-C3): the image's filesystem is exported, the binary scanned is the one inside it, every OS
# package it carries is classified by its copyright text exactly as a module is by its licence,
# and any file that neither the binary nor a package accounts for fails the scan, because a file
# nobody scanned is a licence nobody accepted.
$imageInfo = $null
$packages = @()
$unowned = @()
if ($Image -ne '') {
    $rootfs = Join-Path ([System.IO.Path]::GetTempPath()) ("quotacore-image-" + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $rootfs | Out-Null
    # The container is never started; the command only satisfies an image
    # that has no entrypoint of its own.
    $cid = (& docker create --entrypoint /quotacore-scan-never-runs $Image 2>&1)
    if ($LASTEXITCODE -ne 0) {
        Write-Error "docker create $Image failed: $cid"
        exit 2
    }
    $tarPath = Join-Path $rootfs '..' | Join-Path -ChildPath ("quotacore-image-" + [guid]::NewGuid().ToString('N') + '.tar')
    & docker export -o $tarPath $cid
    & docker rm $cid | Out-Null
    & tar -xf $tarPath -C $rootfs
    Remove-Item -LiteralPath $tarPath -Force
    $inspect = (& docker image inspect $Image | ConvertFrom-Json)[0]
    $base = $inspect.Config.Labels.'org.opencontainers.image.base.name'
    if (-not $base) { $base = 'unknown' }
    $builder = ''
    if (Test-Path -LiteralPath $Dockerfile) {
        $from = Select-String -LiteralPath $Dockerfile -Pattern '^FROM\s+(\S+)' | Select-Object -First 1
        if ($from) { $builder = $from.Matches[0].Groups[1].Value }
    }
    $imageInfo = [pscustomobject]@{
        Ref = $Image; Id = $inspect.Id; Base = $base; Builder = $builder
        Revision = $inspect.Config.Labels.'org.opencontainers.image.revision'
        Version = $inspect.Config.Labels.'org.opencontainers.image.version'
    }
    $Binary = Join-Path $rootfs 'usr/local/bin/quotacore'

    # The files the container runtime itself puts in every container, which no image ships.
    $runtimeFiles = @('.dockerenv', 'etc/hostname', 'etc/hosts', 'etc/resolv.conf', 'etc/mtab', 'dev/console')
    $owned = @{ 'usr/local/bin/quotacore' = 'the binary' }
    $statusDir = Join-Path $rootfs 'var/lib/dpkg/status.d'
    if (Test-Path -LiteralPath $statusDir) {
        foreach ($entry in Get-ChildItem -LiteralPath $statusDir -File | Where-Object { $_.Name -notlike '*.md5sums' }) {
            $fields = @{}
            foreach ($line in Get-Content -LiteralPath $entry.FullName) {
                if ($line -match '^(Package|Version):\s*(.+)$') { $fields[$Matches[1]] = $Matches[2].Trim() }
            }
            $name = $fields['Package']
            $copyright = Join-Path $rootfs "usr/share/doc/$name/copyright"
            $packages += [pscustomobject]@{ Name = $name; Version = $fields['Version']; Copyright = $copyright }
            $sums = Join-Path $statusDir "$($entry.Name).md5sums"
            if (Test-Path -LiteralPath $sums) {
                foreach ($line in Get-Content -LiteralPath $sums) {
                    $parts = $line -split '\s+', 2
                    if ($parts.Count -eq 2) { $owned[$parts[1].TrimStart('/')] = $name }
                }
            }
            $owned["var/lib/dpkg/status.d/$($entry.Name)"] = $name
            $owned["var/lib/dpkg/status.d/$($entry.Name).md5sums"] = $name
        }
    }
    foreach ($f in Get-ChildItem -LiteralPath $rootfs -Recurse -Force | Where-Object { -not $_.PSIsContainer -and $_.LinkType -ne 'SymbolicLink' }) {
        # A symbolic link carries no content of its own; dpkg lists only files.
        $rel = [System.IO.Path]::GetRelativePath($rootfs, $f.FullName).Replace('\', '/')
        if ($runtimeFiles -contains $rel -or $owned.ContainsKey($rel)) { continue }
        $unowned += $rel
    }
}

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

# An image's OS packages are classified by their Debian copyright file with the same rules, and
# allowed by the same list: '<package> ADR-nnnn'.
foreach ($pkg in $packages) {
    if (-not (Test-Path -LiteralPath $pkg.Copyright)) {
        $results += [pscustomobject]@{ Path = $pkg.Name; Version = $pkg.Version; License = 'NOASSERTION'; Text = ''; Source = 'no copyright file'; Kind = 'os' }
        $failures += "OS package $($pkg.Name): no copyright file, so it is not demonstrably permissive"
        continue
    }
    $upper = (Get-Content -LiteralPath $pkg.Copyright -Raw).ToUpperInvariant()
    $isCopyleft = $false
    foreach ($marker in $copyleftMarkers) { if ($upper.Contains($marker)) { $isCopyleft = $true } }
    if ($upper.Contains('GPL-2') -or $upper.Contains('MPL-2.0') -or $upper.Contains('LGPL')) { $isCopyleft = $true }
    $id = 'NOASSERTION'
    foreach ($rule in $permissiveRules) {
        $hit = $true
        foreach ($phrase in $rule.All) { if (-not $upper.Contains($phrase.ToUpperInvariant())) { $hit = $false } }
        if ($rule.Absent) { foreach ($phrase in $rule.Absent) { if ($upper.Contains($phrase.ToUpperInvariant())) { $hit = $false } } }
        if ($hit) { $id = $rule.Id; break }
    }
    if ($upper.Contains('PUBLIC DOMAIN') -and -not $isCopyleft -and $id -eq 'NOASSERTION') { $id = 'LicenseRef-public-domain' }
    $source = 'usr/share/doc/' + $pkg.Name + '/copyright'
    if ($isCopyleft) {
        $id = 'COPYLEFT'
        if ($allowed.ContainsKey($pkg.Name)) { $id = $allowed[$pkg.Name]; $source += ", covered by $($allowed[$pkg.Name])" }
        else { $failures += "OS package $($pkg.Name) $($pkg.Version) carries copyleft text and is not covered by a decision record" }
    } elseif ($id -eq 'NOASSERTION') {
        $failures += "OS package $($pkg.Name) $($pkg.Version): its copyright file matches no licence this scan recognises"
    }
    $results += [pscustomobject]@{ Path = $pkg.Name; Version = $pkg.Version; License = $id; Text = ''; Source = $source; Kind = 'os' }
}
foreach ($rel in $unowned) {
    $failures += "$rel is in the image and neither the binary nor an OS package accounts for it, so no licence for it was scanned"
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
        if ($r.Kind -eq 'os') {
            $components += [ordered]@{
                type     = 'library'
                name     = $r.Path
                version  = $r.Version
                purl     = "pkg:deb/debian/$($r.Path)@$($r.Version)"
                licenses = $licences
                evidence = @(@{ identity = @{ field = 'dpkg status' }; description = $r.Source })
            }
            continue
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
    $subject = [ordered]@{
        type    = 'application'
        name    = 'github.com/quotacore/quotacore'
        version = 'v0.1.0-dev'
    }
    $properties = @(
        @{ name = 'quotacore:source:runtime-path'; value = 'modules reported by go version -m on the built binary' }
    )
    if ($imageInfo) {
        # The subject is the image, and the base it was built on is a component of it: for
        # scratch that component has no packages, which the SBOM states rather than omits.
        $subject = [ordered]@{
            type    = 'container'
            name    = $imageInfo.Ref
            version = $imageInfo.Version
            hashes  = @(@{ alg = 'SHA-256'; content = $imageInfo.Id -replace '^sha256:', '' })
        }
        $components = @([ordered]@{
            type        = 'container'
            name        = $imageInfo.Base
            description = $(if ($packages.Count -eq 0) { 'the base image; it ships no OS package' } else { "the base image; $($packages.Count) OS packages, listed as components" })
        }) + $components
        $properties = @(
            @{ name = 'quotacore:source:runtime-path'; value = "the exported filesystem of $($imageInfo.Ref): the binary's modules by go version -m, and every OS package by its dpkg record" },
            @{ name = 'quotacore:image:builder'; value = $imageInfo.Builder },
            @{ name = 'quotacore:image:revision'; value = "$($imageInfo.Revision)" },
            @{ name = 'quotacore:image:os-packages'; value = "$($packages.Count)" }
        )
    }
    $sbom = [ordered]@{
        bomFormat   = 'CycloneDX'
        specVersion = '1.6'
        version     = 1
        metadata    = [ordered]@{
            component = $subject
            tools = @(
                @{ vendor = 'quotacore'; name = 'tools/license-scan.ps1' }
            )
            properties = $properties
        }
        components = $components
    }
    $json = $sbom | ConvertTo-Json -Depth 12
    $target = $SbomPath
    if (-not [System.IO.Path]::IsPathRooted($target)) { $target = Join-Path (Get-Location) $target }
    [System.IO.File]::WriteAllText($target, $json, (New-Object System.Text.UTF8Encoding($false)))
    Write-Report "wrote $SbomPath with $(@($components).Count) components"
}

if ($failures.Count -gt 0) {
    Write-Output "FAIL - $($failures.Count) licence problem(s) in the runtime path:"
    foreach ($f in $failures) { Write-Output "  - $f" }
    exit 1
}

if ($imageInfo) {
    Write-Report "PASS - $Image ships $($packages.Count) OS packages and the binary's $(@($results | Where-Object { $_.Kind -ne 'os' }).Count) modules, no unlicensed copyleft and no unscanned file"
    exit 0
}
Write-Report "PASS - $($results.Count) modules in the runtime path, no unlicensed copyleft dependency"
exit 0
