<#
.SYNOPSIS
Pushes a validated release candidate, waits for CI, and explicitly publishes it.

.DESCRIPTION
The script never deploys production. It requires a clean repository, HTTPS origin,
Git Credential Manager, and a source version matching the requested version. A real
run pushes the current integration branch and master, waits for the ordinary push CI,
then dispatches the explicit release workflow and verifies the immutable assets.

.EXAMPLE
pwsh -File scripts/publish-release.ps1 -DryRun

.EXAMPLE
pwsh -File scripts/publish-release.ps1 -Version 2.4.7
#>
[CmdletBinding()]
param(
    [ValidatePattern('^\d+\.\d+\.\d+$')]
    [string]$Version = '2.4.7',

    [ValidatePattern('^[0-9a-f]{40}$')]
    [string]$ExpectedRemoteMaster = 'e573252bd9156ddddcdf8e572edacc94ff493909',

    [string]$Repository = 'dayou0168/telegram-ledger-bot',

    [string]$Workflow = 'go-ledger.yml',

    [switch]$DryRun
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Invoke-Native {
    param([string]$File, [string[]]$Arguments)
    & $File @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "$File failed with exit code $LASTEXITCODE"
    }
}

function Get-NativeOutput {
    param([string]$File, [string[]]$Arguments)
    $output = @(& $File @Arguments)
    if ($LASTEXITCODE -ne 0) {
        throw "$File failed with exit code $LASTEXITCODE"
    }
    return (($output | ForEach-Object { [string]$_ }) -join [Environment]::NewLine).Trim()
}

function Invoke-GitPush {
    param([string[]]$Arguments)
    $gitBash = 'C:\Program Files\Git\bin\bash.exe'
    if (-not (Test-Path -LiteralPath $gitBash)) {
        throw 'Git for Windows Bash is required for the verified HTTPS/GCM push path'
    }
    & $gitBash -lc 'git "$@"' -- @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "Git Bash push failed with exit code $LASTEXITCODE"
    }
}

function Find-WorkflowRun {
    param(
        [string]$Event,
        [string]$HeadSha,
        [DateTimeOffset]$NotBefore
    )
    $deadline = [DateTimeOffset]::UtcNow.AddMinutes(5)
    do {
        $json = Get-NativeOutput gh @(
            'run', 'list', '--repo', $Repository, '--workflow', $Workflow,
            '--event', $Event, '--branch', 'master', '--limit', '30',
            '--json', 'databaseId,headSha,status,conclusion,url,createdAt'
        )
        $runs = if ($json) { @($json | ConvertFrom-Json) } else { @() }
        $match = $runs | Where-Object {
            $_.headSha -eq $HeadSha -and [DateTimeOffset]$_.createdAt -ge $NotBefore
        } | Sort-Object { [DateTimeOffset]$_.createdAt } -Descending | Select-Object -First 1
        if ($null -ne $match) {
            return $match
        }
        Start-Sleep -Seconds 5
    } while ([DateTimeOffset]::UtcNow -lt $deadline)
    throw "Timed out waiting for $Event workflow run"
}

function Wait-And-ValidateRun {
    param(
        [object]$Run,
        [string[]]$RequiredJobs
    )
    Write-Host "Waiting for Actions run $($Run.databaseId): $($Run.url)"
    Invoke-Native gh @('run', 'watch', [string]$Run.databaseId, '--repo', $Repository, '--exit-status') | Out-Host
    $view = Get-NativeOutput gh @(
        'run', 'view', [string]$Run.databaseId, '--repo', $Repository,
        '--json', 'conclusion,url,jobs'
    ) | ConvertFrom-Json
    if ($view.conclusion -ne 'success') {
        throw "Actions run $($Run.databaseId) did not succeed"
    }
    foreach ($name in $RequiredJobs) {
        $job = @($view.jobs | Where-Object { $_.name -eq $name })
        if ($job.Count -ne 1 -or $job[0].conclusion -ne 'success') {
            throw "Required job '$name' did not succeed"
        }
    }
    $bad = @($view.jobs | Where-Object { $_.conclusion -notin @('success', 'skipped') })
    if ($bad.Count -ne 0) {
        throw "Actions run contains a non-successful job"
    }
    return $view
}

function Test-ReleaseAssets {
    param([string]$Tag, [string]$HeadSha)
    $release = Get-NativeOutput gh @(
        'release', 'view', $Tag, '--repo', $Repository,
        '--json', 'tagName,targetCommitish,url,assets'
    ) | ConvertFrom-Json
    if ($release.tagName -ne $Tag -or $release.targetCommitish -ne $HeadSha) {
        throw 'Release tag or target commit does not match the published candidate'
    }

    $package = "ledger-chain-watcher-v$Version-linux-amd64"
    $expected = @(
        'ledger-chain-watcher-linux-amd64',
        'ledger-chain-watcher-linux-amd64.sha256',
        "$package.tar.gz",
        "$package.tar.gz.sha256",
        "telegram-ledger-bot-v$Version-source.tar.gz",
        "telegram-ledger-bot-v$Version-source.tar.gz.sha256",
        'SHA256SUMS',
        'image-digests.txt'
    )
    $assetNames = @($release.assets | ForEach-Object { $_.name })
    foreach ($name in $expected) {
        if ($name -notin $assetNames) {
            throw "Release asset missing: $name"
        }
    }

    $assetDir = Join-Path ([IO.Path]::GetTempPath()) ("ledger-release-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $assetDir | Out-Null
    try {
        Invoke-Native gh @('release', 'download', $Tag, '--repo', $Repository, '--dir', $assetDir) | Out-Host
        $sumLines = Get-Content -LiteralPath (Join-Path $assetDir 'SHA256SUMS')
        foreach ($line in $sumLines) {
            if ($line -notmatch '^([0-9a-fA-F]{64})\s+\*?(.+)$') {
                throw 'Malformed SHA256SUMS entry'
            }
            $expectedHash = $Matches[1].ToLowerInvariant()
            $name = $Matches[2]
            $path = Join-Path $assetDir $name
            if (-not (Test-Path -LiteralPath $path)) {
                throw "Checksummed asset missing: $name"
            }
            $actualHash = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
            if ($actualHash -ne $expectedHash) {
                throw "SHA256 mismatch: $name"
            }
        }

        foreach ($name in @(
            'ledger-chain-watcher-linux-amd64',
            "$package.tar.gz",
            "telegram-ledger-bot-v$Version-source.tar.gz"
        )) {
            $sidecar = Get-Content -LiteralPath (Join-Path $assetDir "$name.sha256") -Raw
            if ($sidecar -notmatch '^([0-9a-fA-F]{64})\s+\*?') {
                throw "Malformed sidecar checksum: $name.sha256"
            }
            $actualHash = (Get-FileHash -LiteralPath (Join-Path $assetDir $name) -Algorithm SHA256).Hash.ToLowerInvariant()
            if ($actualHash -ne $Matches[1].ToLowerInvariant()) {
                throw "Sidecar SHA256 mismatch: $name"
            }
        }

        $digestLines = @(Get-Content -LiteralPath (Join-Path $assetDir 'image-digests.txt'))
        $botPattern = '^ghcr\.io/dayou0168/telegram-ledger-bot-go:' + [regex]::Escape($Version) + ' ghcr\.io/dayou0168/telegram-ledger-bot-go@sha256:[0-9a-f]{64}$'
        $watcherPattern = '^ghcr\.io/dayou0168/telegram-ledger-chain-watcher:' + [regex]::Escape($Version) + ' ghcr\.io/dayou0168/telegram-ledger-chain-watcher@sha256:[0-9a-f]{64}$'
        if (@($digestLines | Where-Object { $_ -match $botPattern }).Count -ne 1) {
            throw 'Bot image digest record is missing or malformed'
        }
        if (@($digestLines | Where-Object { $_ -match $watcherPattern }).Count -ne 1) {
            throw 'Watcher image digest record is missing or malformed'
        }
        Write-Host "Release assets and SHA256SUMS verified: $($release.url)"
        $digestLines | ForEach-Object { Write-Host $_ }
        return $release
    }
    finally {
        Remove-Item -LiteralPath $assetDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}

$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location -LiteralPath $repoRoot

foreach ($command in @('git', 'gh')) {
    if ($null -eq (Get-Command $command -ErrorAction SilentlyContinue)) {
        throw "Required command is missing: $command"
    }
}

if (@(git status --porcelain=v1).Count -ne 0) {
    throw 'Repository must be clean before publishing'
}
$origin = Get-NativeOutput git @('remote', 'get-url', 'origin')
if ($origin -ne "https://github.com/$Repository.git") {
    throw 'origin must be the canonical HTTPS repository URL'
}
$sslBackend = Get-NativeOutput git @('config', '--get', 'http.sslBackend')
if ($sslBackend -ne 'openssl') {
    throw 'This repository must use the verified OpenSSL HTTPS backend'
}
$helpers = Get-NativeOutput git @('config', '--show-origin', '--get-all', 'credential.helper')
if ($helpers -notmatch '(?m)\bmanager\s*$') {
    throw 'Git Credential Manager is not the effective credential helper'
}

& gh auth status --hostname github.com *> $null
if ($LASTEXITCODE -ne 0) {
    throw 'GitHub CLI OAuth is unavailable'
}
$permission = Get-NativeOutput gh @('repo', 'view', $Repository, '--json', 'viewerPermission') | ConvertFrom-Json
if ($permission.viewerPermission -notin @('ADMIN', 'WRITE')) {
    throw 'GitHub account lacks repository write permission'
}

Invoke-Native git @('fetch', '--prune', 'origin')
$remoteMaster = Get-NativeOutput git @('rev-parse', 'refs/remotes/origin/master')
if ($remoteMaster -ne $ExpectedRemoteMaster) {
    throw "origin/master changed: expected $ExpectedRemoteMaster, got $remoteMaster"
}
$head = Get-NativeOutput git @('rev-parse', 'HEAD')
$branch = Get-NativeOutput git @('branch', '--show-current')
if ($branch -notmatch '^codex/') {
    throw 'Publish from a codex integration branch, not an arbitrary branch'
}
& git merge-base --is-ancestor refs/remotes/origin/master HEAD
if ($LASTEXITCODE -ne 0) {
    throw 'Candidate is not a fast-forward of origin/master'
}

$configText = Get-Content -LiteralPath 'go-ledger/internal/config/config.go' -Raw
$versionMatch = [regex]::Match($configText, '(?m)^const Version = "([0-9]+\.[0-9]+\.[0-9]+)"\r?$')
if (-not $versionMatch.Success -or $versionMatch.Groups[1].Value -ne $Version) {
    throw 'Requested version does not match config.Version'
}
$tag = "v$Version"
$notes = "docs/releases/$tag.md"
if (-not (Test-Path -LiteralPath $notes)) {
    throw "Release notes are missing: $notes"
}

$tagOutput = @(& git ls-remote --tags origin "refs/tags/$tag")
if ($LASTEXITCODE -ne 0) {
    throw 'Unable to query remote tags'
}
if ($tagOutput.Count -ne 0) {
    throw "Remote tag already exists: $tag"
}
$savedErrorActionPreference = $ErrorActionPreference
$ErrorActionPreference = 'SilentlyContinue'
& gh release view $tag --repo $Repository *> $null
$releaseExists = $LASTEXITCODE -eq 0
$ErrorActionPreference = $savedErrorActionPreference
if ($releaseExists) {
    throw "GitHub Release already exists: $tag"
}

$pushArguments = @('push')
if ($DryRun) {
    $pushArguments += '--dry-run'
}
Write-Host "Publishing candidate branch $branch at $head"
Invoke-GitPush ($pushArguments + @('origin', "HEAD:refs/heads/$branch"))
Invoke-GitPush ($pushArguments + @('origin', 'HEAD:refs/heads/master'))
if ($DryRun) {
    Write-Host 'Dry run passed. No refs, workflows, images, tags, or Releases were created.'
    exit 0
}

$pushStarted = [DateTimeOffset]::UtcNow.AddMinutes(-1)
$pushRun = Find-WorkflowRun -Event 'push' -HeadSha $head -NotBefore $pushStarted
$pushView = Wait-And-ValidateRun -Run $pushRun -RequiredJobs @('PostgreSQL 16 CI', 'Critical package race tests')
Write-Host "Push CI passed: $($pushView.url)"

$dispatchStarted = [DateTimeOffset]::UtcNow.AddSeconds(-10)
Invoke-Native gh @('workflow', 'run', $Workflow, '--repo', $Repository, '--ref', 'master', '-f', "version=$Version")
$releaseRun = Find-WorkflowRun -Event 'workflow_dispatch' -HeadSha $head -NotBefore $dispatchStarted
$releaseView = Wait-And-ValidateRun -Run $releaseRun -RequiredJobs @('PostgreSQL 16 CI', 'Critical package race tests', 'Explicit release')
Write-Host "Release workflow passed: $($releaseView.url)"

$tagCommit = Get-NativeOutput gh @('api', "repos/$Repository/commits/$tag", '--jq', '.sha')
if ($tagCommit -ne $head) {
    throw "Published tag does not point to candidate commit: $tagCommit"
}
$release = Test-ReleaseAssets -Tag $tag -HeadSha $head
Write-Host "Published $tag at $head"
Write-Host "Release URL: $($release.url)"
Write-Host 'Production deployment was not performed.'
