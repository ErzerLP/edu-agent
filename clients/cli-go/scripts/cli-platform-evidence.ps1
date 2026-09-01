$ErrorActionPreference = "Stop"
if (Get-Variable -Name PSNativeCommandUseErrorActionPreference -ErrorAction SilentlyContinue) {
    $PSNativeCommandUseErrorActionPreference = $false
}

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

function Get-GoTestSkipCount([object[]] $OutputLines) {
    $count = 0
    foreach ($line in @($OutputLines)) {
        $text = [string] $line
        if ($text -match '^\s*(--- SKIP:|=== SKIP(?:\s|:))') {
            $count++
        }
    }
    return $count
}

function Test-NativeCheckPassed([string] $CheckName, [int] $ExitCode, [int] $SkipCount) {
    if ($ExitCode -ne 0) {
        return $false
    }
    if ($CheckName -eq "workspace-securefile-handle-publication" -and $SkipCount -ne 0) {
        return $false
    }
    return $true
}

function Get-SafeFailureCategory([object[]] $OutputLines) {
    $joined = [string]::Join(" ", [string[]] @($OutputLines | ForEach-Object { [string] $_ }))
    if ([string]::IsNullOrWhiteSpace($joined)) {
        return "no-output"
    }
    if ($joined -match '(?i)panic:') {
        return "test-panic"
    }
    if ($joined -match '(?i)(timed out|timeout)') {
        return "timeout"
    }
    if ($joined -match '(?i)ConPTY') {
        return "conpty-failure"
    }
    if ($joined -match '(?i)console') {
        return "console-failure"
    }
    if ($joined -match '(?i)PTY') {
        return "pty-failure"
    }
    if ($joined -match '(?i)(exit status|exited|fatal|error)') {
        return "process-failure"
    }
    if ($joined -match '(?i)(--- FAIL:|\bFAIL\b)') {
        return "test-failure"
    }
    return "check-failure"
}

function Get-FailureAnnotationBody([string] $CheckName, [string] $Method, [int] $ExitCode, [object[]] $OutputLines) {
    if ($CheckName -eq "pair-hidden-input") {
        return "check=$CheckName method=$Method exit=$ExitCode failure_category=pair-hidden-input-failed raw_output=omitted"
    }
    if ($CheckName -eq "workspace-securefile-handle-publication" -and (Get-GoTestSkipCount $OutputLines) -ne 0) {
        return "check=$CheckName method=$Method exit=$ExitCode failure_category=required-test-skipped raw_output=omitted"
    }
    $category = Get-SafeFailureCategory $OutputLines
    return "check=$CheckName method=$Method exit=$ExitCode failure_category=$category raw_output=omitted"
}

function Write-FailureAnnotation([string] $CheckName, [string] $Method, [int] $ExitCode, [object[]] $OutputLines) {
    $title = ConvertTo-GitHubCommandValue "CLI native check failed" $true
    $body = Get-FailureAnnotationBody $CheckName $Method $ExitCode $OutputLines
    $message = ConvertTo-GitHubCommandValue $body
    Write-Output "::error title=$title::$message"
}

function Invoke-FailureAnnotationSanitizationSelfTest {
    $probe = "7Hq!M2z#V9p`$K4x@"
    $remainder = "R8c%T6n&W3j^F5s"
    $full = $probe + $remainder
    $pairBody = Get-FailureAnnotationBody "pair-hidden-input" "self-test-method" 1 @(
        "panic: $probe",
        "fatal: $remainder",
        "error: $full"
    )
    foreach ($fragment in @($probe, $remainder, $full)) {
        if ($pairBody.Contains($fragment)) {
            throw "pair hidden-input annotation retained raw fixture output"
        }
    }
    if ($pairBody -notmatch 'failure_category=pair-hidden-input-failed raw_output=omitted$') {
        throw "pair hidden-input annotation did not use the fixed no-raw category"
    }

    $otherFixture = "sensitive-output-fixture"
    $otherBody = Get-FailureAnnotationBody "clear" "self-test-method" 1 @("timed out: $otherFixture")
    if ($otherBody.Contains($otherFixture)) {
        throw "non-pair annotation retained raw output"
    }
    if ($otherBody -notmatch 'failure_category=timeout raw_output=omitted$') {
        throw "non-pair annotation did not retain a safe failure category"
    }
    $skipFixture = @(
        "=== RUN   TestWindowsRequiredFixture",
        "--- SKIP: TestWindowsRequiredFixture/symlink (0.00s)",
        "=== SKIP  TestWindowsRequiredFixture/junction"
    )
    $skipCount = Get-GoTestSkipCount $skipFixture
    if ($skipCount -ne 2) {
        throw "workspace skip counter did not recognize both Go skip markers"
    }
    if (Test-NativeCheckPassed "workspace-securefile-handle-publication" 0 1) {
        throw "workspace evidence accepted a required skipped test"
    }
    if (-not (Test-NativeCheckPassed "workspace-securefile-handle-publication" 0 0)) {
        throw "workspace evidence rejected a zero-skip successful test"
    }
    if (-not (Test-NativeCheckPassed "clear" 0 2)) {
        throw "zero-skip policy changed an unrelated platform check"
    }
    $unrelatedSkipBody = Get-FailureAnnotationBody "clear" "self-test-method" 1 $skipFixture
    if ($unrelatedSkipBody -notmatch 'failure_category=check-failure raw_output=omitted$') {
        throw "workspace skip category changed an unrelated platform annotation"
    }
    $skipBody = Get-FailureAnnotationBody "workspace-securefile-handle-publication" "self-test-method" 1 $skipFixture
    if ($skipBody -notmatch 'failure_category=required-test-skipped raw_output=omitted$') {
        throw "workspace skip failure did not retain a safe diagnostic category"
    }

    Write-Output "failure_annotation_sanitization=pass"
    Write-Output "workspace_skip_policy=pass"
}

if ($env:EDU_AGENT_EVIDENCE_SELF_TEST -eq "1") {
    Invoke-FailureAnnotationSanitizationSelfTest
    exit 0
}

$moduleRoot = Split-Path -Parent $PSScriptRoot
$platform = "$env:RUNNER_OS_NAME-$env:RUNNER_ARCH_NAME"
$outputDirectory = Join-Path $moduleRoot "platform-evidence/$platform"
$evidenceFile = Join-Path $outputDirectory "evidence.txt"
New-Item -ItemType Directory -Force -Path $outputDirectory | Out-Null

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
        $hiddenInputMethod = "linux-pty+xterm-readpassword+termios-echo-check+input-echo-probe+final-fragment-rejection"
        $clearMethod = "production-clearscreen+csi-output"
        $systemKeyMethod = "session-dbus+secret-service+libsecret"
    }
    "macOS" {
        $rootConfinementMethod = "root-handle+openat-o_nofollow"
        $hiddenInputMethod = "darwin-pty+xterm-readpassword+termios-echo-check+input-echo-probe+final-fragment-rejection"
        $clearMethod = "production-clearscreen+csi-output"
        $systemKeyMethod = "keychain-generic-password+stdin-secret"
    }
    "Windows" {
        $rootConfinementMethod = "retained-root-parent-handles+ntcreatefile-no-follow+ntsetinformationfile-rename"
        $hiddenInputMethod = "windows-conpty+xterm-readpassword+input-echo-probe+final-fragment-rejection"
        $clearMethod = "windows-conpty+production-clearscreen+vt-clear+forced-vt-unavailable+fillconsole-cursor-fallback"
        $systemKeyMethod = "dpapi-current-user+user-only-acl"
    }
    default {
        throw "unsupported native evidence runner: $env:RUNNER_OS_NAME"
    }
}

$env:EDU_AGENT_NATIVE_KEYBACKEND_TEST = "1"

$checks = @(
    @{ Name = "importer-root-confinement"; Package = "./internal/importer"; Pattern = "^TestReadDocumentRejectsDeterministicIntermediateDirectorySwap$"; Method = $rootConfinementMethod },
    @{ Name = "offline-root-confinement"; Package = "./internal/offline"; Pattern = "^TestSymlinkAndRootEscapeAreRejected$"; Method = $rootConfinementMethod },
    @{ Name = "offline-lease-contention"; Package = "./internal/offline"; Pattern = "^TestLeaseContentionAndSharedReaders$"; Method = "native-filesystem-lock+shared-exclusive-lease" },
    @{ Name = "offline-atomic-rewrap"; Package = "./internal/offline"; Pattern = "^TestRewrapPassphrasePreservesSealedObjects$"; Method = "native-filesystem-atomic-replace+sealed-object-preservation" },
    @{ Name = "offline-system-key-migrate-purge"; Package = "./internal/offline"; Pattern = "^TestNativeSystemKeyMigrationAndPurgeCleanup$"; Method = $systemKeyMethod },
    @{ Name = "credential-round-trip-cleanup"; Package = "./internal/credentials"; Pattern = "^TestPlatformCredentialRoundTripCleanup$"; Method = "native-platform-credential-store" },
    @{ Name = "pair-hidden-input"; Package = "./internal/terminal"; Pattern = "^TestPlatformPairSecretInput$"; Method = $hiddenInputMethod },
    @{ Name = "pair-line-input"; Package = "./internal/terminal"; Pattern = "^TestPlatformPairLineInput$"; Method = "production-readsecret-non-tty-line" },
    @{ Name = "ctrl-l"; Package = "./internal/terminal"; Pattern = "^TestPlatformControlL$"; Method = "native-go-test" },
    @{ Name = "clear"; Package = "./internal/terminal"; Pattern = "^TestPlatformClear$"; Method = $clearMethod }
)

if ($env:RUNNER_OS_NAME -eq "Windows") {
    $checks += @(
        @{ Name = "workspace-securefile-handle-publication"; Package = "./internal/securefile"; Pattern = "^TestWindows(HandleRelativeCreateReplaceAndCleanup|RejectsReparseAndInvalidPaths|ReplacePreservesProtectedDACL|RootAndParentHandlesPinNamespace|HardlinkAliasesShareFileIdentity)$"; Method = $rootConfinementMethod }
    )
}

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
        $skipCount = 0
        if ($check.Name -eq "workspace-securefile-handle-publication") {
            $skipCount = Get-GoTestSkipCount $output
            Add-Evidence "skipped[$($check.Name)]=$skipCount"
        }
        Add-Evidence "output[$($check.Name)]=captured_lines=$($output.Count) raw_output=omitted"
        if (Test-NativeCheckPassed $check.Name $exitCode $skipCount) {
            Add-Evidence "result[$($check.Name)]=pass"
        } else {
            $effectiveExitCode = $exitCode
            if ($effectiveExitCode -eq 0) {
                $effectiveExitCode = 1
            }
            Add-Evidence "result[$($check.Name)]=fail exit_code=$effectiveExitCode"
            Write-FailureAnnotation $check.Name $check.Method $effectiveExitCode $output
            $failed = $true
        }
    }
} finally {
    Pop-Location
}

if ($failed) {
    exit 1
}
