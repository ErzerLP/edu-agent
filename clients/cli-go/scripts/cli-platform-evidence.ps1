$ErrorActionPreference = "Stop"
if (Get-Variable -Name PSNativeCommandUseErrorActionPreference -ErrorAction SilentlyContinue) {
    $PSNativeCommandUseErrorActionPreference = $false
}

$moduleRoot = Split-Path -Parent $PSScriptRoot
$platform = "$env:RUNNER_OS_NAME-$env:RUNNER_ARCH_NAME"
$outputDirectory = Join-Path $moduleRoot "platform-evidence/$platform"
$evidenceFile = Join-Path $outputDirectory "evidence.txt"
New-Item -ItemType Directory -Force -Path $outputDirectory | Out-Null

function Add-Evidence([string] $Line) {
    $Line | Tee-Object -FilePath $evidenceFile -Append
}

function ConvertTo-GitHubCommandValue([string] $Value, [bool] $Property = $false) {
    $escaped = $Value.Replace("%", "%25").Replace("`r", "%0D").Replace("`n", "%0A")
    if ($Property) {
        $escaped = $escaped.Replace(":", "%3A").Replace(",", "%2C")
    }
    return $escaped
}

function Write-FailureAnnotation([string] $CheckName, [string] $Method, [int] $ExitCode) {
    $title = ConvertTo-GitHubCommandValue "CLI native check failed" $true
    $message = ConvertTo-GitHubCommandValue "check=$CheckName method=$Method exit_code=$ExitCode"
    Write-Output "::error title=$title::$message"
}

if ([string]::IsNullOrWhiteSpace($env:CANDIDATE_SHA)) {
    throw "CANDIDATE_SHA is required"
}

$goVersion = (& go version).Trim()
$goos = (& go env GOOS).Trim()
$goarch = (& go env GOARCH).Trim()
Add-Evidence "candidate_sha=$env:CANDIDATE_SHA"
Add-Evidence "runner_os=$env:RUNNER_OS_NAME"
Add-Evidence "runner_arch=$env:RUNNER_ARCH_NAME"
Add-Evidence "goos=$goos"
Add-Evidence "goarch=$goarch"
Add-Evidence "go_version=$goVersion"

switch ($env:RUNNER_OS_NAME) {
    "Linux" {
        $rootConfinementMethod = "root-handle+openat-o_nofollow"
        $hiddenInputMethod = "unix-pty+xterm-readpassword+termios-echo-check"
    }
    "macOS" {
        $rootConfinementMethod = "root-handle+openat-o_nofollow"
        $hiddenInputMethod = "unix-pty+xterm-readpassword+termios-echo-check"
    }
    "Windows" {
        $rootConfinementMethod = "resolved-root+reparse-rejection+final-handle-boundary"
        $hiddenInputMethod = "windows-conpty+xterm-readpassword"
    }
    default {
        throw "unsupported native evidence runner: $env:RUNNER_OS_NAME"
    }
}

$checks = @(
    @{ Name = "importer-root-confinement"; Package = "./internal/importer"; Pattern = "^TestReadDocumentRejectsDeterministicIntermediateDirectorySwap$"; Method = $rootConfinementMethod },
    @{ Name = "credential-round-trip-cleanup"; Package = "./internal/credentials"; Pattern = "^TestPlatformCredentialRoundTripCleanup$"; Method = "native-platform-credential-store" },
    @{ Name = "pair-hidden-input"; Package = "./internal/terminal"; Pattern = "^TestPlatformPairSecretInput$"; Method = $hiddenInputMethod },
    @{ Name = "pair-line-input"; Package = "./internal/terminal"; Pattern = "^TestPlatformPairLineInput$"; Method = "production-readsecret-non-tty-line" },
    @{ Name = "ctrl-l"; Package = "./internal/terminal"; Pattern = "^TestPlatformControlL$"; Method = "native-go-test" },
    @{ Name = "clear"; Package = "./internal/terminal"; Pattern = "^TestPlatformClear$"; Method = "native-console-or-terminal" }
)

$failed = $false
Push-Location $moduleRoot
try {
    foreach ($check in $checks) {
        $package = $check.Package
        $pattern = $check.Pattern
        $command = "go test $package -run '$pattern' -count=1 -v"
        Add-Evidence "method[$($check.Name)]=$($check.Method)"
        Add-Evidence "command[$($check.Name)]=$command"
        $output = @(& go test $package -run $pattern -count=1 -v 2>&1)
        $exitCode = $LASTEXITCODE
        Add-Evidence "output[$($check.Name)]=captured_lines=$($output.Count) raw_output=omitted"
        if ($exitCode -eq 0) {
            Add-Evidence "result[$($check.Name)]=pass"
        } else {
            Add-Evidence "result[$($check.Name)]=fail exit_code=$exitCode"
            Write-FailureAnnotation $check.Name $check.Method $exitCode
            $failed = $true
        }
    }
} finally {
    Pop-Location
}

if ($failed) {
    exit 1
}
