param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('bootstrap', 'mod-verify', 'compose-up', 'compose-down', 'gateway', 'test', 'test-race', 'build', 'check-env', 'fmt', 'fmt-check', 'lint', 'vuln', 'ci-lint', 'secret-scan', 'check', 'migrate-validate', 'migrate-up', 'migrate-down', 'migrate-version')]
    [string]$Action
)

$ErrorActionPreference = 'Stop'
$ProjectRoot = Split-Path -Parent $PSScriptRoot
$InstalledGoBin = 'C:\Program Files\Go\bin'

if (-not (Get-Command go -ErrorAction SilentlyContinue) -and (Test-Path -LiteralPath (Join-Path $InstalledGoBin 'go.exe'))) {
    $env:Path = "$InstalledGoBin;$env:Path"
}

function Require-Command {
    param([string]$Name)
    if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) {
        throw "Required command '$Name' is not installed or not available in PATH."
    }
}

function Invoke-Checked {
    param(
        [Parameter(Mandatory = $true)]
        [string]$FilePath,
        [Parameter(ValueFromRemainingArguments = $true)]
        [string[]]$Arguments
    )

    & $FilePath @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "Command '$FilePath $($Arguments -join ' ')' failed with exit code $LASTEXITCODE."
    }
}

function Import-LocalEnv {
    $EnvFile = Join-Path $ProjectRoot '.env'
    if (-not (Test-Path -LiteralPath $EnvFile -PathType Leaf)) {
        throw "Local environment file '$EnvFile' is missing. Copy .env.example to .env first."
    }

    foreach ($Line in Get-Content -LiteralPath $EnvFile -Encoding utf8) {
        $Trimmed = $Line.Trim()
        if ($Trimmed.Length -eq 0 -or $Trimmed.StartsWith('#')) { continue }
        $Parts = $Trimmed.Split('=', 2)
        if ($Parts.Count -ne 2 -or $Parts[0].Trim() -notmatch '^[A-Z][A-Z0-9_]*$') {
            throw 'Invalid .env entry. Expected KEY=VALUE with an uppercase key.'
        }
        $Name = $Parts[0].Trim()
        if (-not (Test-Path -Path "Env:$Name")) {
            Set-Item -Path "Env:$Name" -Value $Parts[1]
        }
    }
}

function Test-Formatting {
    $Output = & go 'tool' 'golangci-lint' 'fmt' '--diff'
    if ($LASTEXITCODE -ne 0) {
        throw "Formatting command failed with exit code $LASTEXITCODE."
    }
    $Rendered = $Output -join [Environment]::NewLine
    if (-not [string]::IsNullOrWhiteSpace($Rendered)) {
        Write-Output $Rendered
        throw 'Go files are not formatted. Run the fmt action and review the changes.'
    }
}

function Test-LocalSecrets {
    $Rules = @(
        @{ Name = 'private-key'; Pattern = '-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP )?PRIVATE KEY-----' },
        @{ Name = 'openai-key'; Pattern = '\bsk-(?:proj-)?[A-Za-z0-9_-]{20,}\b' },
        @{ Name = 'anthropic-key'; Pattern = '\bsk-ant-[A-Za-z0-9_-]{20,}\b' },
        @{ Name = 'aws-access-key'; Pattern = '\b(?:AKIA|ASIA)[0-9A-Z]{16}\b' },
        @{ Name = 'github-token'; Pattern = '\bgh[pousr]_[A-Za-z0-9]{30,}\b' }
    )
    $TextExtensions = @('.go', '.md', '.json', '.yaml', '.yml', '.toml', '.sql', '.ps1', '.mod', '.sum', '.example')
    $TextNames = @('Makefile', '.gitignore', '.gitattributes', '.editorconfig')
    $GitPrefix = (Join-Path $ProjectRoot '.git') + [IO.Path]::DirectorySeparatorChar
    $LocalEnv = Join-Path $ProjectRoot '.env'
    $Findings = @()

    foreach ($File in Get-ChildItem -LiteralPath $ProjectRoot -Recurse -File -Force) {
        if ($File.FullName.StartsWith($GitPrefix, [StringComparison]::OrdinalIgnoreCase)) { continue }
        if ($File.FullName.Equals($LocalEnv, [StringComparison]::OrdinalIgnoreCase)) { continue }
        if ($File.Length -gt 5MB) { continue }
        if ($File.Extension -notin $TextExtensions -and $File.Name -notin $TextNames) { continue }

        $Content = Get-Content -LiteralPath $File.FullName -Raw -Encoding utf8
        foreach ($Rule in $Rules) {
            if ([regex]::IsMatch($Content, $Rule.Pattern)) {
                $Relative = $File.FullName.Substring($ProjectRoot.Length).TrimStart('\', '/')
                $Findings += "$Relative [$($Rule.Name)]"
            }
        }
    }

    if ($Findings.Count -gt 0) {
        throw "Potential secrets detected (values redacted): $($Findings -join '; ')"
    }
    Write-Output 'Local high-risk secret pattern scan passed.'
}

switch ($Action) {
    'check-env' {
        Require-Command git
        Require-Command go
        Require-Command docker
        Invoke-Checked git '--version'
        Invoke-Checked go 'version'
        Invoke-Checked docker '--version'
    }
    'bootstrap' {
        Require-Command go
        Push-Location $ProjectRoot
        try { Invoke-Checked go 'mod' 'download' } finally { Pop-Location }
    }
    'mod-verify' {
        Require-Command go
        Push-Location $ProjectRoot
        try { Invoke-Checked go 'mod' 'verify' } finally { Pop-Location }
    }
    'compose-up' {
        Require-Command docker
        Push-Location $ProjectRoot
        try { Invoke-Checked docker 'compose' '--env-file' '.env' '-f' 'deploy/compose/compose.yaml' 'up' '-d' } finally { Pop-Location }
    }
    'compose-down' {
        Require-Command docker
        Push-Location $ProjectRoot
        try { Invoke-Checked docker 'compose' '--env-file' '.env' '-f' 'deploy/compose/compose.yaml' 'down' } finally { Pop-Location }
    }
    'gateway' {
        Require-Command go
        Import-LocalEnv
        Push-Location $ProjectRoot
        try { Invoke-Checked go 'run' './cmd/gateway' } finally { Pop-Location }
    }
    'test' {
        Require-Command go
        Push-Location $ProjectRoot
        try { Invoke-Checked go 'test' '-count=1' './...' } finally { Pop-Location }
    }
    'test-race' {
        Require-Command go
        Push-Location $ProjectRoot
        try { Invoke-Checked go 'test' '-race' '-count=1' './...' } finally { Pop-Location }
    }
    'build' {
        Require-Command go
        Push-Location $ProjectRoot
        try { Invoke-Checked go 'build' '-buildvcs=false' './...' } finally { Pop-Location }
    }
    'fmt' {
        Require-Command go
        Push-Location $ProjectRoot
        try { Invoke-Checked go 'tool' 'golangci-lint' 'fmt' } finally { Pop-Location }
    }
    'fmt-check' {
        Require-Command go
        Push-Location $ProjectRoot
        try { Test-Formatting } finally { Pop-Location }
    }
    'lint' {
        Require-Command go
        Push-Location $ProjectRoot
        try {
            Invoke-Checked go 'vet' './...'
            Invoke-Checked go 'tool' 'golangci-lint' 'config' 'verify'
            Invoke-Checked go 'tool' 'golangci-lint' 'run' '--timeout=5m' './...'
        } finally { Pop-Location }
    }
    'vuln' {
        Require-Command go
        Push-Location $ProjectRoot
        try { Invoke-Checked go 'tool' 'govulncheck' './...' } finally { Pop-Location }
    }
    'ci-lint' {
        Require-Command go
        Push-Location $ProjectRoot
        try { Invoke-Checked go 'tool' 'actionlint' '.github/workflows/ci.yml' } finally { Pop-Location }
    }
    'secret-scan' {
        Push-Location $ProjectRoot
        try { Test-LocalSecrets } finally { Pop-Location }
    }
    'check' {
        Require-Command go
        Push-Location $ProjectRoot
        try {
            Invoke-Checked go 'mod' 'verify'
            Test-Formatting
            Invoke-Checked go 'vet' './...'
            Invoke-Checked go 'tool' 'golangci-lint' 'config' 'verify'
            Invoke-Checked go 'tool' 'golangci-lint' 'run' '--timeout=5m' './...'
            Invoke-Checked go 'test' '-count=1' './...'
            Invoke-Checked go 'build' '-buildvcs=false' './...'
            Invoke-Checked go 'tool' 'govulncheck' './...'
            Invoke-Checked go 'run' './cmd/migrate' 'validate' '--path' 'migrations'
            Invoke-Checked go 'tool' 'actionlint' '.github/workflows/ci.yml'
            Test-LocalSecrets
        } finally { Pop-Location }
    }
    'migrate-validate' {
        Require-Command go
        Push-Location $ProjectRoot
        try { Invoke-Checked go 'run' './cmd/migrate' 'validate' '--path' 'migrations' } finally { Pop-Location }
    }
    'migrate-up' {
        Require-Command go
        Import-LocalEnv
        Push-Location $ProjectRoot
        try { Invoke-Checked go 'run' './cmd/migrate' 'up' '--path' 'migrations' } finally { Pop-Location }
    }
    'migrate-down' {
        Require-Command go
        Import-LocalEnv
        Push-Location $ProjectRoot
        try { Invoke-Checked go 'run' './cmd/migrate' 'down' '--path' 'migrations' '--steps' '1' '--confirm-development' } finally { Pop-Location }
    }
    'migrate-version' {
        Require-Command go
        Import-LocalEnv
        Push-Location $ProjectRoot
        try { Invoke-Checked go 'run' './cmd/migrate' 'version' '--path' 'migrations' } finally { Pop-Location }
    }
}
