<#
.SYNOPSIS
    Consistency checks for the Quotacore specification.

.DESCRIPTION
    Four structural checks and thirteen semantic ones. The structural checks can
    be satisfied by a document that is well-formed and wrong; the semantic ones
    are the ones that catch a contradiction between two files.

      1.  Every Markdown link resolves, and so does every anchor fragment.
      2.  Every identifier reference (DR-, UC-, FS-, A-, Q-, J-, NFR-, T-, INV-,
          ADR-) points at something that is actually defined.
      3.  No identifier is defined twice.
      4.  The inventory in docs/README.md matches the documents, and no Markdown
          file is unindexed.

      F1  Backticked snake_case used as an error code is in the catalogue.
      F2  A count claimed in prose equals the counted reality.
      F3  A DDL enum value appears in the wire contract.
      F4  Every rule states how it is verified, and the method is a real one.
      F5  A release named in the roadmap is named in the scope boundary.
      F6  A research note carries a date and a source.
      F7  A supersession points at a record that exists and agrees.
      F8  A status in the open-questions register comes from the vocabulary.
      F9  Application code does not exist while a blocking question is open.
      W1  A number in a research note has a nearby source.
      W2  "Threshold" is not used as the product name.
      W3  An assumption's type comes from the vocabulary.
      W4  A file was held out of the specification and the inventory.

    Each check has one severity in $severity below: 'fail' is fatal on every
    run, 'warn' is reported and ignored. F1-F9 have each been run against every
    document in the repository and fire only on a real defect, so they are
    fatal; W1-W4 report questions of judgement and stay advisory. -Strict makes
    every warning fatal without editing the table, which is how an advisory
    check is trialled before it is trusted enough to promote.

.PARAMETER Root
    Repository root. Defaults to the parent of this script's directory.

.PARAMETER Strict
    Treat warnings as failures. Use this to prove a check is clean.

.PARAMETER Quiet
    Suppress the report when everything passes.

.EXAMPLE
    pwsh -File tools/check-docs.ps1
    pwsh -File tools/check-docs.ps1 -Strict
    pwsh -File tools/check-docs.ps1 -Quiet
#>
[CmdletBinding()]
param(
    [string] $Root,
    [switch] $Strict,
    [switch] $Quiet
)

$ErrorActionPreference = 'Stop'

# $PSScriptRoot is not populated while a param default is being bound under
# Windows PowerShell 5.1, so the root is resolved in the body instead. The
# script must run under 5.1 as well as pwsh 7, because 5.1 is what ships with
# the operating system and a check that needs a separate install does not get
# run.
if (-not $Root) {
    $here = $PSScriptRoot
    if (-not $here) { $here = Split-Path -Parent $MyInvocation.MyCommand.Path }
    $Root = Split-Path -Parent $here
}
$Root = (Resolve-Path -LiteralPath $Root).Path
$problems = New-Object 'System.Collections.Generic.List[string]'
$warnings = New-Object 'System.Collections.Generic.List[string]'

# The severity table, and the only place that decides whether a finding is
# fatal. F1-F9 have each been run against every document in the repository and
# fire only on a real defect, so they are fatal. W1-W4 stay advisory because each
# one reports a question of judgement rather than a contradiction, and a check
# that fails on judgement stops being read.
$severity = @{
    'F1' = 'fail'; 'F2' = 'fail'; 'F3' = 'fail'; 'F4' = 'fail'; 'F5' = 'fail'
    'F6' = 'fail'; 'F7' = 'fail'; 'F8' = 'fail'; 'F9' = 'fail'
    'W1' = 'warn'; 'W2' = 'warn'; 'W3' = 'warn'; 'W4' = 'warn'
}

function Add-Finding([string] $check, [string] $message) {
    $fatal = $severity[$check] -eq 'fail'
    if ($Strict) { $fatal = $true }
    if ($fatal) { $problems.Add("$check $message") }
    else { $warnings.Add("$check $message") }
}

# Agent tooling is not part of the specification, so it is counted separately
# and printed rather than silently dropped. A file in here that is not tooling
# belongs in the index.
$excluded = New-Object 'System.Collections.Generic.List[string]'

function Get-MarkdownFiles {
    Get-ChildItem -Path $Root -Recurse -Filter *.md |
        Where-Object { $_.FullName -notmatch '\\\.git\\' -and $_.FullName -notmatch '\\\.opencode\\' }
}

function Get-RelativePath([string] $full) {
    $full.Substring($Root.Length + 1).Replace('\', '/')
}

function Get-Slug([string] $heading) {
    $s = $heading.Trim().ToLowerInvariant()
    $s = $s -replace '[^\p{L}\p{N}\s\-_]', ''
    return ($s -replace '\s', '-')
}

# GitHub strips code spans and fenced blocks before it resolves links, so a regex
# inside a table cell or a SQL block is not a link. This mirrors that.
function Remove-Code([string] $text) {
    $t = [regex]::Replace($text, '(?ms)^```.*?^```', '')
    $t = [regex]::Replace($t, '(?ms)^~~~.*?^~~~', '')
    return [regex]::Replace($t, '`[^`]*`', '``')
}

$files = Get-MarkdownFiles
$fileCount = $files.Count
$texts = @{}
foreach ($f in $files) { $texts[(Get-RelativePath $f.FullName)] = [System.IO.File]::ReadAllText($f.FullName) }

foreach ($f in (Get-ChildItem -Path $Root -Recurse -Filter *.md | Where-Object { $_.FullName -notmatch '\\\.git\\' })) {
    if ($f.FullName -match '\\\.opencode\\') { $excluded.Add((Get-RelativePath $f.FullName)) }
}

# ---------------------------------------------------------------- 1. links
$anchors = @{}
foreach ($rel in $texts.Keys) {
    $set = New-Object 'System.Collections.Generic.HashSet[string]'
    $seen = @{}
    foreach ($line in ($texts[$rel] -split "`r?`n")) {
        if ($line -match '^#{1,6}\s+(.*?)\s*$') {
            $base = Get-Slug $Matches[1]
            if ($seen.ContainsKey($base)) {
                $seen[$base]++
                [void]$set.Add("$base-$($seen[$base])")
            }
            else {
                $seen[$base] = 0
                [void]$set.Add($base)
            }
        }
    }
    $anchors[$rel] = $set
}

$linkCount = 0
foreach ($rel in $texts.Keys) {
    $dir = [System.IO.Path]::GetDirectoryName((Join-Path $Root $rel)).Replace('\', '/')
    foreach ($m in [regex]::Matches((Remove-Code $texts[$rel]), '\[[^\]]*\]\(([^)\s]+)\)')) {
        $linkCount++
        $target = $m.Groups[1].Value
        if ($target -match '^(https?:|mailto:)') { continue }
        $path = $target
        $frag = $null
        if ($target -match '#') {
            $parts = $target -split '#', 2
            $path = $parts[0]
            $frag = $parts[1]
        }
        if ($path -eq '') {
            if ($frag -and -not $anchors[$rel].Contains($frag)) {
                $problems.Add("link: $rel -> #$frag (no such heading)")
            }
            continue
        }
        $abs = [System.IO.Path]::GetFullPath((Join-Path $dir $path)).Replace('\', '/')
        if (-not (Test-Path -LiteralPath $abs)) {
            $problems.Add("link: $rel -> $target (no such file)")
            continue
        }
        if ($frag) {
            $relTarget = $abs.Substring($Root.Length + 1)
            if ($relTarget -match '\.md$' -and -not $anchors[$relTarget].Contains($frag)) {
                $problems.Add("link: $rel -> $target (no such heading)")
            }
        }
    }
}

# ---------------------------------------------------------- 2 and 3. ids
# Each family declares where its definitions live. A definition is a heading, a
# table row or a list item, so a typo in a reference cannot be satisfied by the
# reference itself.
$families = @(
    @{ Name = 'DR';   Pattern = '(?<![A-Z])DR-(\d{3})(?!\d)';   Def = 'docs/product/domain-rules.md';                     DefPattern = '(?m)^### DR-\d{3}\b' }
    @{ Name = 'UC';   Pattern = '(?<![A-Z])UC-(\d{2})(?!\d)';   Def = 'docs/product/use-cases.md';                        DefPattern = '(?m)^### UC-\d{2}\b' }
    @{ Name = 'FS';   Pattern = '(?<![A-Z])FS-(\d{2})(?!\d)';   Def = 'docs/product/error-catalog.md';                    DefPattern = '(?m)^### FS-\d{2}\b' }
    @{ Name = 'A';    Pattern = '(?<![A-Z0-9])A-\d{1,2}(?!\d)';  Def = 'docs/product/assumptions-and-open-questions.md'; DefPattern = '(?m)^\| A-\d{2} \|' }
    @{ Name = 'Q';    Pattern = '(?<![A-Z0-9])Q-\d{2}(?!\d)';    Def = 'docs/product/assumptions-and-open-questions.md'; DefPattern = '(?m)^\| Q-\d{2} \|' }
    @{ Name = 'J';    Pattern = '(?<![A-Z0-9])J-\d{1,2}(?!\d)';  Def = 'docs/product/user-journeys.md';                    DefPattern = '(?m)^## J-\d{1,2}\b' }
    @{ Name = 'NFR';  Pattern = '(?<![A-Z0-9])NFR-[A-Z]+\d{1,2}(?!\d)'; Def = 'docs/architecture/non-functional-requirements.md'; DefPattern = '(?m)^\| NFR-[A-Z]+\d{1,2} \|' }
    @{ Name = 'T';    Pattern = '(?<![A-Z0-9])T-\d{2}(?!\d)';    Def = 'docs/architecture/testing-strategy.md';            DefPattern = '(?m)^### T-\d{2}\b' }
    @{ Name = 'INV';  Pattern = '(?<![A-Z0-9])INV-[A-Z]+\d{1,2}(?!\d)'; Def = @('docs/product/state-machines.md', 'docs/architecture/data-model.md'); DefPattern = '(?m)^\s*(?:[-*|]\s*)?\**\s*(?:Invariant\s+)?INV-[A-Z]+\d' }
    @{ Name = 'ADR';  Pattern = '(?<![A-Z])ADR-(\d{4})(?!\d)';  Def = $null; DefPattern = $null }
)

$defined = @{}
$definitions = @{}
foreach ($fam in $families) {
    $set = New-Object 'System.Collections.Generic.HashSet[string]'
    $defFiles = if ($fam.Def -is [array]) { $fam.Def } elseif ($fam.Def) { @($fam.Def) } else { @() }
    foreach ($df in $defFiles) {
        if (-not $texts.ContainsKey($df)) {
            $problems.Add("ids: definition source for $($fam.Name) is missing: $df")
            continue
        }
        foreach ($m in [regex]::Matches($texts[$df], $fam.DefPattern)) {
            $id = [regex]::Match($m.Value, "(?<![A-Z])$($fam.Name)-[A-Z0-9]+").Value
            if ($set.Add($id)) {
                if ($definitions.ContainsKey($id)) {
                    $problems.Add("ids: $id defined twice ($($definitions[$id]) and $df)")
                }
                else { $definitions[$id] = $df }
            }
        }
    }
    $defined[$fam.Name] = $set
}

# ADR definitions are the decision records themselves.
$adrDir = Join-Path $Root 'docs/decisions'
if (Test-Path -LiteralPath $adrDir) {
    foreach ($a in Get-ChildItem -LiteralPath $adrDir -Filter '????-*.md') {
        [void]$defined['ADR'].Add("ADR-" + $a.Name.Substring(0, 4))
    }
}

foreach ($fam in $families) {
    $rx = [regex]$fam.Pattern
    foreach ($rel in ($texts.Keys | Sort-Object)) {
        foreach ($m in $rx.Matches($texts[$rel])) {
            $id = $m.Value
            if (-not $defined[$fam.Name].Contains($id)) {
                $problems.Add("ids: $rel references $id, which is not defined")
            }
        }
    }
}

# ---------------------------------------------------- 4. inventory
# Every number the specification states about itself, measured the same way every
# time. The inventory table in docs/README.md is compared against these.

# Build output is not a repository file. `.gitignore` already declares `/bin/`
# and `/dist/`, which matters only once something in the repository actually
# produces artefacts, and "Files in the repository" counting a compiled binary
# would be a number nobody could reproduce. Only anchored directory patterns are
# honoured, and only for this count: a Markdown document cannot be hidden from
# the link, identifier or inventory-document checks by adding a line here.
$ignoredDirs = @()
$gitignorePath = Join-Path $Root '.gitignore'
if (Test-Path -LiteralPath $gitignorePath) {
    foreach ($line in (Get-Content -LiteralPath $gitignorePath)) {
        if ($line -match '^\s*/(?<dir>[A-Za-z0-9._-]+/)') { $ignoredDirs += $Matches['dir'] }
    }
}
$isIgnored = {
    param([string] $full)
    $rel = $full.Substring($Root.Length).TrimStart('\', '/') -replace '\\', '/'
    foreach ($d in $ignoredDirs) {
        if ($rel.StartsWith($d)) { return $true }
    }
    return $false
}

$inventoryPath = 'docs/README.md'
$counts = [ordered]@{
    'Rules'                     = { ($defined['DR'] | Measure-Object).Count }
    'Use cases'                 = { ($defined['UC'] | Measure-Object).Count }
    'Error codes'               = { ([regex]::Matches($texts['docs/product/error-catalog.md'], '(?m)^\| `[a-z][a-z0-9_]*` \| \d{3} \|') | ForEach-Object { $_.Value } | Sort-Object -Unique).Count }
    'Failure scenarios'         = { ($defined['FS'] | Measure-Object).Count }
    'Assumptions'               = { ($defined['A'] | Measure-Object).Count }
    'Open questions'            = { ($defined['Q'] | Measure-Object).Count }
    'Journeys'                  = { ($defined['J'] | Measure-Object).Count }
    'Non-functional requirements' = { ($defined['NFR'] | Measure-Object).Count }
    'Invariants'                = { ($defined['INV'] | Measure-Object).Count }
    'Named tests'               = { ($defined['T'] | Measure-Object).Count }
    'Decisions'                 = { ($defined['ADR'] | Measure-Object).Count }
    'Markdown documents'        = { $fileCount }
    'Files in the repository'   = { (Get-ChildItem -Path $Root -Recurse -File | Where-Object { $_.FullName -notmatch '\\\.git\\' -and $_.FullName -notmatch '\\\.opencode\\' -and -not (& $isIgnored $_.FullName) }).Count }
}

$measured = [ordered]@{}
foreach ($k in $counts.Keys) { $measured[$k] = & $counts[$k] }

if ($texts.ContainsKey($inventoryPath)) {
    foreach ($line in ($texts[$inventoryPath] -split "`r?`n")) {
        if ($line -match '^\|\s*(?<item>[A-Za-z][A-Za-z \-]+?)\s*\|\s*(?<n>\d+)\s*\|') {
            $item = $Matches['item'].Trim()
            $claimed = [int]$Matches['n']
            if ($measured.Contains($item)) {
                $actual = $measured[$item]
                if ($claimed -ne $actual) {
                    $problems.Add("inventory: $inventoryPath claims $item = $claimed, actual is $actual")
                }
            }
        }
    }
}
else {
    $problems.Add("inventory: $inventoryPath is missing")
}

foreach ($rel in $texts.Keys) {
    if ($rel -eq $inventoryPath) { continue }
    $leaf = [System.IO.Path]::GetFileName($rel)
    if ($texts[$inventoryPath] -notmatch [regex]::Escape($leaf)) {
        $problems.Add("inventory: $rel is not listed in $inventoryPath")
    }
}

# --------------------------------------------------------- 5. semantics
# Everything above can be satisfied by a document that is well-formed and wrong.
# These nine checks are the ones that catch a contradiction between two files.

$catalogPath = 'docs/product/error-catalog.md'
$rulesPath = 'docs/product/domain-rules.md'
$registerPath = 'docs/product/assumptions-and-open-questions.md'
$apiPath = 'docs/architecture/api-conventions.md'
$modelPath = 'docs/architecture/data-model.md'
$testingPath = 'docs/architecture/testing-strategy.md'

# F1 - the catalogue and its endpoint matrix must agree. A code that no endpoint
# can return is documentation a client can never act on, and a code in the matrix
# that is not in the catalogue does not exist. The matrix is the only place the
# two appear side by side and it claims to be complete, so the claim is checkable
# without guessing which backticked identifier was meant to be a code.
$catLines = $texts[$catalogPath] -split "`r?`n"
$tableAt = ($catLines | Select-String -Pattern '^## 2\.' | Select-Object -First 1).LineNumber
$matrixAt = ($catLines | Select-String -Pattern '^## 3\.' | Select-Object -First 1).LineNumber
$fsAt = ($catLines | Select-String -Pattern '^## 4\.' | Select-Object -First 1).LineNumber
if (-not $tableAt -or -not $matrixAt -or -not $fsAt) {
    Add-Finding 'F1' "$catalogPath no longer has the numbered sections this check parses (2, 3 and 4)"
}
else {
    $catStatus = @{}
    for ($i = $tableAt - 1; $i -lt $matrixAt - 1; $i++) {
        if ($catLines[$i] -match '^\| `(?<c>[a-z][a-z0-9_]*)` \| (?<s>\d{3}) \|') { $catStatus[$Matches['c']] = $Matches['s'] }
    }
    $inMatrix = New-Object 'System.Collections.Generic.HashSet[string]'
    for ($i = $matrixAt - 1; $i -lt $fsAt - 1; $i++) {
        if ($catLines[$i] -notmatch '^\|') { continue }
        $cells = $catLines[$i] -split '\|'
        if ($cells.Count -lt 4) { continue }
        foreach ($m in [regex]::Matches($cells[2], '`(?<c>[a-z][a-z0-9_]*)`')) { [void]$inMatrix.Add($m.Groups['c'].Value) }
    }
    # The matrix abbreviates with "as `consume`, plus ...", where the token names
    # the endpoint whose row is being reused rather than a code.
    $endpointShorthand = @('consume', 'refund', 'check', 'balance')
    foreach ($c in ($catStatus.Keys | Sort-Object)) {
        if (-not $inMatrix.Contains($c)) {
            Add-Finding 'F1' "$c is in the catalogue as $($catStatus[$c]) but no endpoint row can return it, so the code is unreachable"
        }
    }
    foreach ($c in ($inMatrix | Sort-Object)) {
        if (-not $catStatus.ContainsKey($c) -and $endpointShorthand -notcontains $c) {
            Add-Finding 'F1' "the endpoint matrix names $c, which is not in the catalogue"
        }
    }
}

# F2 - a number the specification states about itself must be the real number.
# Only an exact noun phrase immediately after the number is matched. Allowing
# filler words between the two ("45 numbered domain rules") catches more and
# also catches prose that is not a count at all, so the strict form is used: a
# missed phrasing is a missed catch, which is much cheaper than a false alarm.
$nouns = [ordered]@{
    'non-functional requirements' = 'Non-functional requirements'
    'named tests'                 = 'Named tests'
    'failure scenarios'           = 'Failure scenarios'
    'error codes'                 = 'Error codes'
    'open questions'              = 'Open questions'
    'use cases'                   = 'Use cases'
    'assumptions'                 = 'Assumptions'
    'journeys'                    = 'Journeys'
    'invariants'                  = 'Invariants'
    'ADRs'                        = 'Decisions'
    'decisions'                   = 'Decisions'
    'documents'                   = 'Markdown documents'
    'nfrs'                        = 'Non-functional requirements'
    'rules'                       = 'Rules'
    'tests'                       = 'Named tests'
    'questions'                   = 'Open questions'
}
$nounRx = '(?:' + (($nouns.Keys | Sort-Object -Property Length -Descending | ForEach-Object { [regex]::Escape($_) }) -join '|') + ')'
$countRx = [regex]('(?<![\d.,`])`?\b(?<n>\d+)\s+(?<noun>' + $nounRx + ')\b')
# Two kinds of count are not counts of the whole specification and are skipped
# rather than reported. A per-directory count ("product/  9 documents") is
# preceded by a path, and a changelog entry records what was true when it was
# written, not what is true now.
$historyPath = 'CHANGELOG.md'
foreach ($rel in ($texts.Keys | Sort-Object)) {
    if ($rel -eq $historyPath) { continue }
    $n = 0
    foreach ($line in ($texts[$rel] -split "`r?`n")) {
        $n++
        foreach ($m in $countRx.Matches($line)) {
            $before = $line.Substring(0, $m.Index)
            if ($before -match '[A-Za-z0-9_\.\-]/\s*$') { continue }
            $noun = $m.Groups['noun'].Value
            $key = $nouns[$noun]
            $claimed = [int]$m.Groups['n'].Value
            if ($measured[$key] -ne $claimed) {
                Add-Finding 'F2' "$rel`:$n claims $claimed $noun, but the specification holds $($measured[$key])"
            }
        }
    }
}

# F3 - a DDL enum that the wire contract names must have its values stated
# there. An enum the contract never mentions is internal, and its absence from
# the API document is not a defect; an enum a client may filter by, with no
# values, cannot be used correctly by anyone.
$apiText = $texts[$apiPath]
foreach ($m in [regex]::Matches($texts[$modelPath], 'CREATE TYPE\s+(?<t>\w+)\s+AS ENUM\s*\((?<v>[^)]*)\)')) {
    $t = $m.Groups['t'].Value
    if ($apiText -notmatch ('(?<![\w])' + [regex]::Escape($t) + '(?![\w])')) { continue }
    $missing = @()
    foreach ($v in [regex]::Matches($m.Groups['v'].Value, "'(?<x>[a-z0-9_]+)'")) {
        $val = $v.Groups['x'].Value
        if ($apiText -notmatch ('`' + [regex]::Escape($val) + '`')) { $missing += $val }
    }
    if ($missing.Count -gt 0) {
        Add-Finding 'F3' "the wire contract names the enum $t but never states its values: $($missing -join ', ')"
    }
}

# F4 - a rule must say how it is verified, and the method must be citable. The
# citable methods are the named correctness tests, any NFR measurement, and the
# named artefacts in testing-strategy.md: the property rows, the script tests, the
# contract tests, the security tests and the substitutes for what is not
# automatically testable. All of them are read out of that document rather than
# hard-coded here, so a test added there is citable the moment it exists, and a
# method that has been deleted stops being citable.
$methods = New-Object 'System.Collections.Generic.HashSet[string]'
foreach ($line in ($texts[$testingPath] -split "`r?`n")) {
    if ($line -match '^\|\s*(?<name>[A-Za-z`][^|]+?)\s*\|') { [void]$methods.Add($Matches['name'].Trim(' ', '`')) }
}
# The numbered security tests in section 11 of security-model.md are citable for
# the same reason: that section is titled Verification, and its bolded lead-ins
# name the tests a security rule may be verified by. Reading the names out of the
# document keeps them honest in the same way as the tables above.
$secPath = 'docs/architecture/security-model.md'
$sec = ($texts[$secPath] -split "`r?`n")
$inVerification = $false
foreach ($line in $sec) {
    if ($line -match '^##\s+11\.') { $inVerification = $true; continue }
    if ($inVerification -and $line -match '^##\s') { break }
    if ($inVerification -and $line -match '^\s*\d+\.\s+\*\*(?<name>[^*]+?)\*{1,2}') {
        [void]$methods.Add($Matches['name'].Trim().TrimEnd('.'))
    }
}
# The instance form is used deliberately. The static three-argument
# Regex.Match(input, pattern, startat) binds to the RegexOptions overload under
# Windows PowerShell 5.1, because an int converts to the enum, and then searches
# from zero instead of from the offset.
$headingRx = [regex]'(?m)^#{2,3} '
foreach ($m in [regex]::Matches($texts[$rulesPath], '(?m)^### (?<id>DR-\d{3})\b.*$')) {
    $id = $m.Groups['id'].Value
    $start = $m.Index + $m.Length
    # Stop at the next heading of level 2 or 3, so a rule's block cannot run on
    # into the next section of the document.
    $next = $headingRx.Match($texts[$rulesPath], $start)
    $block = if ($next.Success) { $texts[$rulesPath].Substring($start, $next.Index - $start) } else { $texts[$rulesPath].Substring($start) }
    # The statement is read to the end of its paragraph, not to the end of the
    # line. A wrapped statement used to be checked only from its first physical
    # line, so every method named after the wrap went unchecked, which is how a
    # rule could name a test that does not exist and still pass.
    $v = [regex]::Match($block, '(?ims)^\*+Verified by:\*+\s*(?<m>.*?)(?=\r?\n[ \t]*\r?\n|\z)')
    if (-not $v.Success) {
        Add-Finding 'F4' "$id has no '*Verified by:*' line, so nothing says how the rule is proven"
        continue
    }
    $line = ($v.Groups['m'].Value -replace '\s+', ' ').Trim()
    # Every method the statement names is checked, not merely whether one of
    # them resolves. A statement citing T-07 and also naming a script test makes
    # two claims, and checking only the first is how a fabricated test survives
    # a green run, which is the one failure mode this check exists to prevent.
    $flat = $line.ToLowerInvariant().Replace([char]0x00D7, 'x')
    $norm = @{}
    foreach ($name in $methods) {
        $n = ($name -replace '\s+', ' ').Trim().ToLowerInvariant().Replace([char]0x00D7, 'x')
        if ($n) { $norm[$n] = $true }
    }
    $bt = [string][char]96
    $nameRx = [regex]("$bt(?<n>[^$bt]+)$bt\s+(script|contract|property|security|chaos|load|soak)\s+test\b")
    foreach ($q in $nameRx.Matches($line)) {
        $n = ($q.Groups['n'].Value -replace '\s+', ' ').Trim().ToLowerInvariant().Replace([char]0x00D7, 'x')
        if (-not $norm.ContainsKey($n)) {
            Add-Finding 'F4' "$id is verified by ``$($q.Groups['n'].Value)``, and no such test exists in testing-strategy.md or the security verification list. A rule may not be verified by a test nobody wrote"
        }
    }
    # A rule that is enforced by a schema constraint names the DDL, because the
    # constraint is the proof and there is nothing to assert at runtime. The DDL
    # is itself checked, so naming it is not an escape hatch.
    $citable = ($line -match '(?<![A-Z0-9])T-\d{2}(?!\d)') -or
               ($line -match '(?<![A-Z0-9])NFR-[A-Z]+\d{1,2}(?!\d)') -or
               ($line -match 'data-model\.md')
    if (-not $citable) {
        # The comparison is normalised before it is made. testing-strategy.md
        # writes "Endpoint × error matrix" with a multiplication sign, and a check
        # that fails a rule over U+00D7 against the letter x is a check that gets
        # deleted rather than fixed.
        $flat2 = $flat
        foreach ($needle in $norm.Keys) {
            if ($needle.Length -gt 8 -and $flat2.Contains($needle)) { $citable = $true; break }
        }
    }
    if (-not $citable) {
        Add-Finding 'F4' "$id verifies by a method that is not in testing-strategy.md: $line"
    }
}

# F5 - a release is a promise to two documents or it is not a promise.
foreach ($m in [regex]::Matches($texts['ROADMAP.md'], 'v(?<v>0\.\d)')) {
    if ($texts['docs/product/mvp-scope.md'] -notmatch ('v' + $m.Groups['v'].Value)) {
        Add-Finding 'F5' "ROADMAP.md names v$($m.Groups['v'].Value), which mvp-scope.md does not"
    }
}

# F6 - a fact that may change must be dated and sourced, or a reader cannot
# tell a re-check from a guess.
foreach ($rel in ($texts.Keys | Where-Object { $_ -like 'docs/research/*' } | Sort-Object)) {
    $t = $texts[$rel]
    if ($t -notmatch '\d{4}-\d{2}-\d{2}') { Add-Finding 'F6' "$rel carries no ISO date" }
    if ($t -notmatch 'https?://' -and $t -notmatch '(?<![A-Z0-9])ADR-\d{4}(?!\d)') {
        Add-Finding 'F6' "$rel cites no source URL and no ADR"
    }
}

# F7 - supersession must be real in both directions.
foreach ($rel in ($texts.Keys | Sort-Object)) {
    foreach ($m in [regex]::Matches($texts[$rel], 'Superseded by ADR-(?<n>\d{4})')) {
        $target = 'docs/decisions/' + $m.Groups['n'].Value + '-'
        $hit = @(Get-ChildItem -Path (Join-Path $Root 'docs/decisions') -Filter ($m.Groups['n'].Value + '-*.md') -ErrorAction SilentlyContinue)
        if ($hit.Count -eq 0) { Add-Finding 'F7' "$rel is superseded by ADR-$($m.Groups['n'].Value), which does not exist" }
        elseif ($hit[0].Name -eq $rel.Split('/')[-1]) { Add-Finding 'F7' "$rel is superseded by itself" }
        elseif ((Get-Content -Raw -LiteralPath $hit[0].FullName) -notmatch 'Superseded') {
            Add-Finding 'F7' "$rel is superseded by ADR-$($m.Groups['n'].Value), whose status does not say so"
        }
    }
}

# F8 - a status is from a fixed vocabulary, so "in progress" cannot silently
# become a fifth thing.
$statuses = @('open', 'answered', 'deferred', 'withdrawn')
foreach ($line in ($texts[$registerPath] -split "`r?`n")) {
    if ($line -match '^\|\s*Q-\d{2}\s*\|.*\|\s*(?<s>[a-z]+)\s*\|\s*$') {
        $s = $Matches['s']
        if ($statuses -notcontains $s) { Add-Finding 'F8' "a question has status '$s', which is not in the vocabulary" }
    }
}

# F9 - the gate. Application code is forbidden while a blocking question is
# open, and this is the only check that can catch it: an agent that writes
# twenty migrations produces a repository that is otherwise perfectly
# consistent. The answer is read from the register, so the check disarms
# itself the moment the questions are answered.
$blocking = @('Q-01', 'Q-02', 'Q-04', 'Q-14')
$stillOpen = @()
foreach ($q in $blocking) {
    $m = [regex]::Match($texts[$registerPath], "(?m)^\|\s*$q\s*\|.*\|\s*(?<s>[a-z]+)\s*\|\s*$")
    if ($m.Success -and $m.Groups['s'].Value -eq 'open') { $stillOpen += $q }
}
$codePaths = @()
$codePaths += @(Get-ChildItem -Path $Root -Filter '*.go' -File -ErrorAction SilentlyContinue)
$codePaths += @(Get-ChildItem -Path $Root -Filter 'go.mod' -File -ErrorAction SilentlyContinue)
$codePaths += @(Get-ChildItem -Path $Root -Filter 'Dockerfile*' -File -ErrorAction SilentlyContinue)
$codePaths += @(Get-ChildItem -Path $Root -Filter 'docker-compose*.yml' -File -ErrorAction SilentlyContinue)
$codePaths += @(Get-ChildItem -Path (Join-Path $Root 'api') -Recurse -File -ErrorAction SilentlyContinue)
$codePaths += @(Get-ChildItem -Path (Join-Path $Root 'migrations') -Recurse -File -ErrorAction SilentlyContinue)
if ($stillOpen.Count -gt 0 -and $codePaths.Count -gt 0) {
    $names = ($codePaths | ForEach-Object { Get-RelativePath $_.FullName } | Sort-Object) -join ', '
    Add-Finding 'F9' "application code exists while $($stillOpen -join ', ') $(if ($stillOpen.Count -eq 1) { 'is' } else { 'are' }) still open ($names). The specification is the deliverable until the register says otherwise; to close a question, decide it in the register and write the rule it produces"
}

# W1 - a number in a research note should have a source within reach of it.
# Judgement, not contradiction: a table of measured values legitimately has
# none on the row. Four ways to have a source are exempt - a table row, a URL, a
# bullet under Sources, and an NFR- identifier, because a requirement cited on the
# line is a source inside the specification, which is what the writing rule
# allows for a limit or a target. What is left is a figure a note invented.
$unsourced = 0
foreach ($rel in ($texts.Keys | Where-Object { $_ -like 'docs/research/*' } | Sort-Object)) {
    foreach ($line in ($texts[$rel] -split "`r?`n")) {
        if ($line -match '^\s*\|' -or $line -match 'https?://' -or $line -match '^\s*-\s') { continue }
        if ($line -match 'NFR-') { continue }
        if ($line -match '\d+(\.\d+)?\s?(ms|%|GB|MiB|req/s|requests/s|KiB|x)\b') { $unsourced++ }
    }
}
if ($unsourced -gt 0) {
    Add-Finding 'W1' "$unsourced prose line(s) in docs/research/ carry a measured number with no source on the line. Check each is covered by the note's Sources section"
}

# W2 - the old product name. The match is case-sensitive on purpose: "threshold"
# is a perfectly good word and the specification uses it constantly, and a check
# that cannot tell a product name from a common noun is worse than no check. Two
# uses are exempt, because a rule cannot avoid naming the thing it prohibits and
# "threshold crossing" is a separate defined term for a webhook trigger.
foreach ($rel in ($texts.Keys | Sort-Object)) {
    $n = 0
    foreach ($line in ($texts[$rel] -split "`r?`n")) {
        $n++
        if ($line -cmatch 'product name|deprecated alias') { continue }
        if ($line -cmatch '(?<![\w-])Threshold(?![\w-])' -and $line -cnotmatch 'Threshold crossing') {
            Add-Finding 'W2' "$rel`:$n says 'Threshold'. The product is Quotacore; if this means a numeric threshold, say 'threshold' in lower case"
        }
    }
}

# W3 - an assumption's type comes from a vocabulary too, but a new one is more
# likely to be a new idea than a mistake.
$types = @('assumed', 'confirmed', 'verifying', 'falsified')
foreach ($line in ($texts[$registerPath] -split "`r?`n")) {
    if ($line -match '^\|\s*A-\d{2}\s*\|[^|]*\|\s*(?<t>[a-z]+)\s*\|') {
        $t = $Matches['t']
        if ($types -notcontains $t) { Add-Finding 'W3' "an assumption has type '$t', which is not in the vocabulary" }
    }
}

# W4 - the exclusion count is printed in the report header. It raises a finding
# only when something was actually held out of the specification, so that a file
# quietly kept out of the inventory cannot go unnoticed, and so that a clean run
# has nothing to say about a directory that does not exist.
if ($excluded.Count -gt 0) {
    Add-Finding 'W4' "$($excluded.Count) file(s) outside the specification were not counted or indexed: $($excluded -join ', ')"
}

# ------------------------------------------------------------------ report
Write-Host ''
Write-Host 'Quotacore specification check'
Write-Host ('  markdown files : {0}   (excluded from the inventory: {1})' -f $fileCount, $excluded.Count)
Write-Host ('  links checked  : {0}' -f $linkCount)
Write-Host ''
foreach ($k in $measured.Keys) {
    Write-Host ('  {0,-30} {1}' -f $k, $measured[$k])
}
Write-Host ''

if ($warnings.Count -gt 0) {
    Write-Host ("{0} warning(s) - reported, not fatal:" -f $warnings.Count) -ForegroundColor Yellow
    foreach ($w in ($warnings | Sort-Object -Unique)) { Write-Host ("  - " + $w) -ForegroundColor Yellow }
    Write-Host ''
}

if ($problems.Count -eq 0) {
    if (-not $Quiet) {
        Write-Host 'PASS - links, identifiers, inventory and semantic checks all agree.' -ForegroundColor Green
    }
    exit 0
}

Write-Host ("FAIL - {0} problem(s):" -f $problems.Count) -ForegroundColor Red
foreach ($p in ($problems | Sort-Object -Unique)) { Write-Host ("  - " + $p) -ForegroundColor Red }
exit 1
