$ErrorActionPreference = "Stop"
if (Get-Variable -Name PSNativeCommandUseErrorActionPreference -ErrorAction SilentlyContinue) {
    $PSNativeCommandUseErrorActionPreference = $false
}

function ConvertTo-GitHubCommandValue([string] $Value, [bool] $Property = $false) {
    $escaped = $Value.Replace("%", "%25").Replace("`r", "%0D").Replace("`n", "%0A")
    if ($Property) {
        $escaped = $escaped.Replace(":", "%3A").Replace(",", "%2C")
    }
    return $escaped
}

function Write-Utf8Lines([string] $Path, [System.Management.Automation.AllowEmptyCollection()] [string[]] $Lines = @()) {
    $encoding = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllLines($Path, [string[]] @($Lines), $encoding)
}

function Add-Evidence([string] $Line) {
    $Line | Tee-Object -FilePath $script:evidenceFile -Append
}

function Get-ExpectedGoIdentity([string] $RunnerOS, [string] $RunnerArch) {
    $expectedOS = switch ($RunnerOS) {
        "Linux" { "linux" }
        "macOS" { "darwin" }
        "Windows" { "windows" }
        default { throw "unsupported native evidence runner: $RunnerOS" }
    }
    $expectedArch = switch ($RunnerArch.ToUpperInvariant()) {
        "X64" { "amd64" }
        "AMD64" { "amd64" }
        "ARM64" { "arm64" }
        default { throw "unsupported native evidence architecture: $RunnerArch" }
    }
    return [PSCustomObject]@{ GOOS = $expectedOS; GOARCH = $expectedArch }
}

function Get-PowerShellHostIdentity {
    $os = if ([Runtime.InteropServices.RuntimeInformation]::IsOSPlatform([Runtime.InteropServices.OSPlatform]::Windows)) {
        "windows"
    } elseif ([Runtime.InteropServices.RuntimeInformation]::IsOSPlatform([Runtime.InteropServices.OSPlatform]::Linux)) {
        "linux"
    } elseif ([Runtime.InteropServices.RuntimeInformation]::IsOSPlatform([Runtime.InteropServices.OSPlatform]::OSX)) {
        "darwin"
    } else {
        "unknown"
    }
    $arch = switch ([Runtime.InteropServices.RuntimeInformation]::ProcessArchitecture.ToString().ToUpperInvariant()) {
        "X64" { "amd64" }
        "ARM64" { "arm64" }
        default { "unknown" }
    }
    return [PSCustomObject]@{ GOOS = $os; GOARCH = $arch }
}

function Measure-GoTestEvidence([object[]] $Events, [string[]] $ExpectedTests, [string] $SuccessMarker, [string] $LogText) {
    $failures = New-Object System.Collections.Generic.List[string]
    $executed = New-Object System.Collections.Generic.List[string]
    $skipped = New-Object System.Collections.Generic.List[string]

    foreach ($event in @($Events)) {
        $testName = [string] $event.Test
        $action = [string] $event.Action
        if (-not [string]::IsNullOrWhiteSpace($testName) -and $action -in @("run", "pass", "fail", "skip")) {
            $executed.Add("$action`:$testName")
        }
        if (-not [string]::IsNullOrWhiteSpace($testName) -and $action -eq "skip") {
            $skipped.Add($testName)
        }
    }

    foreach ($expected in $ExpectedTests) {
        $runCount = @($Events | Where-Object { ([string] $_.Test) -eq $expected -and ([string] $_.Action) -eq "run" }).Count
        $passCount = @($Events | Where-Object { ([string] $_.Test) -eq $expected -and ([string] $_.Action) -eq "pass" }).Count
        $failCount = @($Events | Where-Object { ([string] $_.Test) -eq $expected -and ([string] $_.Action) -eq "fail" }).Count
        if ($runCount -ne 1) {
            $failures.Add("expected-test-run-count:$expected`:$runCount")
        }
        if ($passCount -ne 1) {
            $failures.Add("expected-test-pass-count:$expected`:$passCount")
        }
        if ($failCount -ne 0) {
            $failures.Add("expected-test-failed:$expected`:$failCount")
        }
        $expectedSkips = @($Events | Where-Object {
            $name = [string] $_.Test
            ([string] $_.Action) -eq "skip" -and ($name -eq $expected -or $name.StartsWith($expected + "/"))
        })
        if ($expectedSkips.Count -ne 0) {
            $failures.Add("expected-test-skipped:$expected`:$($expectedSkips.Count)")
        }
    }

    if (-not [string]::IsNullOrWhiteSpace($SuccessMarker) -and -not $LogText.Contains($SuccessMarker)) {
        $failures.Add("helper-success-marker-missing:$SuccessMarker")
    }

    return [PSCustomObject]@{
        Passed = $failures.Count -eq 0
        Failures = [string[]] @($failures)
        Executed = [string[]] @($executed)
        Skipped = [string[]] @($skipped)
    }
}

function Write-FailureAnnotation([string] $CheckName, [string[]] $Failures, [int] $ExitCode) {
    $title = ConvertTo-GitHubCommandValue "CLI native check failed" $true
    $category = if ($Failures.Count -gt 0) { $Failures[0].Split(":")[0] } elseif ($ExitCode -ne 0) { "go-test-exit" } else { "evidence-validation" }
    $body = ConvertTo-GitHubCommandValue "check=$CheckName exit=$ExitCode failure_category=$category raw_output=artifact-only"
    Write-Output "::error title=${title}::$body"
}

function Invoke-EvidenceSelfTest {
    $happy = @(
        [PSCustomObject]@{ Action = "run"; Test = "TestRequired" },
        [PSCustomObject]@{ Action = "output"; Test = "TestRequired"; Output = "HELPER_SUCCESS`n" },
        [PSCustomObject]@{ Action = "pass"; Test = "TestRequired" }
    )
    $happyResult = Measure-GoTestEvidence $happy @("TestRequired") "HELPER_SUCCESS" "HELPER_SUCCESS"
    if (-not $happyResult.Passed) {
        throw "self-test rejected complete run/pass evidence"
    }
    $missingPass = Measure-GoTestEvidence @([PSCustomObject]@{ Action = "run"; Test = "TestRequired" }) @("TestRequired") "" ""
    if ($missingPass.Passed) {
        throw "self-test accepted a zero-pass expected test"
    }
    $skipped = Measure-GoTestEvidence @(
        [PSCustomObject]@{ Action = "run"; Test = "TestRequired" },
        [PSCustomObject]@{ Action = "skip"; Test = "TestRequired/fixture" },
        [PSCustomObject]@{ Action = "pass"; Test = "TestRequired" }
    ) @("TestRequired") "" ""
    if ($skipped.Passed) {
        throw "self-test accepted a skipped required fixture"
    }
    $markerMissing = Measure-GoTestEvidence $happy @("TestRequired") "MISSING_MARKER" "HELPER_SUCCESS"
    if ($markerMissing.Passed) {
        throw "self-test accepted a missing helper success marker"
    }
    $identity = Get-ExpectedGoIdentity "Windows" "X64"
    if ($identity.GOOS -ne "windows" -or $identity.GOARCH -ne "amd64") {
        throw "self-test runner identity mapping failed"
    }
    Write-Output "evidence_self_test=pass"
    Write-Output "zero_match_zero_skip_marker_policy=pass"
}

if ($env:EDU_AGENT_EVIDENCE_SELF_TEST -eq "1") {
    Invoke-EvidenceSelfTest
    exit 0
}

if ([string]::IsNullOrWhiteSpace($env:CANDIDATE_SHA)) {
    throw "CANDIDATE_SHA is required"
}
if ([string]::IsNullOrWhiteSpace($env:RUNNER_OS_NAME) -or [string]::IsNullOrWhiteSpace($env:RUNNER_ARCH_NAME)) {
    throw "RUNNER_OS_NAME and RUNNER_ARCH_NAME are required"
}

$moduleRoot = Split-Path -Parent $PSScriptRoot
$repositoryRoot = (Resolve-Path (Join-Path $moduleRoot "../..")).Path
$candidateSHA = $env:CANDIDATE_SHA.Trim().ToLowerInvariant()
if ($candidateSHA -notmatch '^[0-9a-f]{40}$') {
    throw "CANDIDATE_SHA must be a full 40-character commit SHA"
}
$checkoutSHAOutput = @(& git -C $repositoryRoot rev-parse --verify HEAD 2>&1)
if ($LASTEXITCODE -ne 0 -or $checkoutSHAOutput.Count -ne 1) {
    throw "git rev-parse HEAD failed"
}
$checkoutSHA = ([string] $checkoutSHAOutput[0]).Trim().ToLowerInvariant()
if ($checkoutSHA -ne $candidateSHA) {
    throw "candidate SHA does not match checked-out HEAD"
}

$expectedIdentity = Get-ExpectedGoIdentity $env:RUNNER_OS_NAME $env:RUNNER_ARCH_NAME
$hostIdentity = Get-PowerShellHostIdentity
$goIdentity = @(& go env GOOS GOARCH GOHOSTOS GOHOSTARCH 2>&1)
if ($LASTEXITCODE -ne 0 -or $goIdentity.Count -ne 4) {
    throw "go env identity query failed"
}
$goos = ([string] $goIdentity[0]).Trim()
$goarch = ([string] $goIdentity[1]).Trim()
$gohostos = ([string] $goIdentity[2]).Trim()
$gohostarch = ([string] $goIdentity[3]).Trim()
if ($hostIdentity.GOOS -ne $expectedIdentity.GOOS -or $hostIdentity.GOARCH -ne $expectedIdentity.GOARCH) {
    throw "PowerShell host identity does not match runner identity"
}
if ($goos -ne $expectedIdentity.GOOS -or $gohostos -ne $expectedIdentity.GOOS -or $goarch -ne $expectedIdentity.GOARCH -or $gohostarch -ne $expectedIdentity.GOARCH) {
    throw "Go target/host identity does not match runner identity"
}

$goVersionOutput = @(& go version 2>&1)
if ($LASTEXITCODE -ne 0 -or $goVersionOutput.Count -ne 1) {
    throw "go version failed"
}
$goVersion = ([string] $goVersionOutput[0]).Trim()
$platform = "$env:RUNNER_OS_NAME-$env:RUNNER_ARCH_NAME"
$outputDirectory = Join-Path $moduleRoot "platform-evidence/$candidateSHA/$platform"
if (Test-Path -LiteralPath $outputDirectory) {
    Remove-Item -LiteralPath $outputDirectory -Recurse -Force
}
New-Item -ItemType Directory -Force -Path $outputDirectory | Out-Null
$script:evidenceFile = Join-Path $outputDirectory "evidence.txt"
Write-Utf8Lines $script:evidenceFile @()

Add-Evidence "candidate_sha=$candidateSHA"
Add-Evidence "checkout_sha=$checkoutSHA"
Add-Evidence "runner_os=$env:RUNNER_OS_NAME"
Add-Evidence "runner_arch=$env:RUNNER_ARCH_NAME"
Add-Evidence "powershell_host_os=$($hostIdentity.GOOS)"
Add-Evidence "powershell_host_arch=$($hostIdentity.GOARCH)"
Add-Evidence "goos=$goos"
Add-Evidence "goarch=$goarch"
Add-Evidence "gohostos=$gohostos"
Add-Evidence "gohostarch=$gohostarch"
Add-Evidence "go_version=$goVersion"
Add-Evidence "evidence_mode=fresh-json-exact-run-pass-zero-skip"

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
        $systemKeyMethod = "dpapi-current-user+protected-user-only-dacl"
    }
}

$env:EDU_AGENT_NATIVE_KEYBACKEND_TEST = "1"
$checks = @(
    @{ Name = "agent-session-keybackend-round-trip-delete"; Package = "./internal/keybackend"; Expected = @("TestNativePlatformSecretRoundTripCleanup"); Method = $systemKeyMethod },
    @{ Name = "agent-session-encrypted-round-trip-delete"; Package = "./internal/agentsession"; Expected = @("TestStoreRoundTripIndexRebuildDirtyRecoveryAndDeletion"); Method = "per-session-aead+wrapped-key+atomic-index-rebuild+delete" },
    @{ Name = "agent-session-single-writer-lock"; Package = "./internal/agentsession"; Expected = @("TestAgentSessionProcessSingleWriter"); Method = "native-process-exclusive-session-lock"; Marker = "AGENT_SESSION_LOCK_HELPER_SUCCESS" },
    @{ Name = "agent-session-atomic-recovery"; Package = "./internal/agentsession"; Expected = @("TestCreateReconcilesPublicationAndNeverDeletesUnconfirmedEnvelope", "TestSaveAndDirtyMarkerReconcilePublicationFaults"); Method = "native-filesystem-atomic-publication+dirty-recovery" },
    @{ Name = "agent-session-tamper"; Package = "./internal/agentsession"; Expected = @("TestContainerRejectsWrongKeyTamperTruncationAndRecordSwap", "TestRecordSwapIsIsolatedAsCorrupt"); Method = "aead-tamper+truncate+record-swap-rejection" },
    @{ Name = "agent-session-privacy-clear"; Package = "./internal/agentsession"; Expected = @("TestNativePlatformAgentSessionPrivacyClear", "TestClearReconcilesNativeSecretAndRejectsOldGenerationRegistration", "TestStoreQuotaDoesNotEvictAndPrivacyClearInvalidatesOpenWriter"); Method = "native-key-rotation+cryptographic-clear+generation-fence" },
    @{ Name = "filelock-cross-process"; Package = "./internal/filelock"; Expected = @("TestFileLockProcessExclusion"); Method = "native-process-file-lock"; Marker = "FILELOCK_PROCESS_HELPER_SUCCESS" },
    @{ Name = "importer-root-confinement"; Package = "./internal/importer"; Expected = @("TestReadDocumentRejectsDeterministicIntermediateDirectorySwap"); Method = $rootConfinementMethod },
    @{ Name = "offline-root-confinement"; Package = "./internal/offline"; Expected = @("TestSymlinkAndRootEscapeAreRejected"); Method = $rootConfinementMethod },
    @{ Name = "offline-lease-contention"; Package = "./internal/offline"; Expected = @("TestLeaseContentionAndSharedReaders"); Method = "native-filesystem-lock+shared-exclusive-lease" },
    @{ Name = "offline-key-migration-crash-recovery"; Package = "./internal/offline"; Expected = @("TestKeyMigrationCrashRecoveryAtEveryDurableBoundary"); Method = "native-filesystem-atomic-key-migration-recovery" },
    @{ Name = "offline-system-key-migrate-purge"; Package = "./internal/offline"; Expected = @("TestNativeSystemKeyMigrationAndPurgeCleanup"); Method = $systemKeyMethod },
    @{ Name = "credential-round-trip-cleanup"; Package = "./internal/credentials"; Expected = @("TestPlatformCredentialRoundTripCleanup"); Method = "native-platform-credential-store" },
    @{ Name = "pair-hidden-input"; Package = "./internal/terminal"; Expected = @("TestPlatformPairSecretInput"); Method = $hiddenInputMethod; CaptureRaw = $false },
    @{ Name = "pair-line-input"; Package = "./internal/terminal"; Expected = @("TestPlatformPairLineInput"); Method = "production-readsecret-non-tty-line" },
    @{ Name = "ctrl-l"; Package = "./internal/terminal"; Expected = @("TestPlatformControlL"); Method = "native-go-test" },
    @{ Name = "clear"; Package = "./internal/terminal"; Expected = @("TestPlatformClear"); Method = $clearMethod }
)

if ($env:RUNNER_OS_NAME -eq "Windows") {
    $checks += @(
        @{ Name = "agent-session-windows-private-storage"; Package = "./internal/agentsession"; Expected = @("TestAgentSessionWindowsPrivateACLAndAtomicReplace", "TestAgentSessionWindowsTightensBroadRootACLAndFailsClosedOnBroadRecord", "TestAgentSessionWindowsRejectsReparseRootAndParent"); Method = "protected-current-user-dacl+atomic-replace+reparse-rejection" },
        @{ Name = "agent-session-windows-dpapi-acl-reparse"; Package = "./internal/keybackend"; Expected = @("TestWindowsSecretBroadACLReadFailsClosedAndReplacementTightens", "TestWindowsSecretRejectsReparseParentAndTarget"); Method = "dpapi-current-user+protected-current-user-dacl+atomic-replace+reparse-rejection" },
        @{ Name = "filelock-windows-reparse"; Package = "./internal/filelock"; Expected = @("TestFileLockWindowsRejectsFileAndDirectoryReparse"); Method = "lockfileex+open-reparse-point+file-and-directory-reparse-fixtures" },
        @{ Name = "securefile-windows-private-publication"; Package = "./internal/securefile"; Expected = @("TestWindowsPrivateACLHelpersTightenAndRejectReparse", "TestWindowsHandleRelativeCreateReplaceAndCleanup", "TestWindowsRejectsReparseAndInvalidPaths", "TestWindowsReplacePreservesProtectedDACL", "TestWindowsRootAndParentHandlesPinNamespace", "TestWindowsHardlinkAliasesShareFileIdentity"); Method = $rootConfinementMethod }
    )
} else {
    $checks += @(
        @{ Name = "agent-session-unix-symlink-root"; Package = "./internal/agentsession"; Expected = @("TestStoreRejectsSymlinkedRootAndProfileQuotaDoesNotPublish"); Method = $rootConfinementMethod },
        @{ Name = "agent-session-unix-storage-mode"; Package = "./internal/agentsession"; Expected = @("TestAgentSessionUnixStorageModes"); Method = "unix-directory-0700+files-0600" },
        @{ Name = "filelock-unix-symlink-mode"; Package = "./internal/filelock"; Expected = @("TestFileLockUnixRejectsSymlinkAndUsesPrivateMode"); Method = "flock+o_nofollow+0600" },
        @{ Name = "securefile-unix-link-confinement"; Package = "./internal/securefile"; Expected = @("TestRootDeleteIsConfinedAndDoesNotFollowSymlinks", "TestRootReadDirAndSnapshotDoNotFollowSymlinks"); Method = $rootConfinementMethod }
    )
}

$failed = $false
$expectedLines = New-Object System.Collections.Generic.List[string]
$executedLines = New-Object System.Collections.Generic.List[string]
$skippedLines = New-Object System.Collections.Generic.List[string]
$manifestChecks = New-Object System.Collections.Generic.List[object]

Push-Location $moduleRoot
try {
    foreach ($check in $checks) {
        $expectedTests = [string[]] @($check.Expected)
        foreach ($expected in $expectedTests) {
            $expectedLines.Add("$($check.Name)`:$expected")
        }
        $escapedTests = @($expectedTests | ForEach-Object { [regex]::Escape($_) })
        $pattern = "^(" + [string]::Join("|", $escapedTests) + ")$"
        $commandText = "go test $($check.Package) -run '$pattern' -count=1 -json"
        Add-Evidence "method[$($check.Name)]=$($check.Method)"
        Add-Evidence "command[$($check.Name)]=$commandText"
        Add-Evidence "expected[$($check.Name)]=$([string]::Join(',', $expectedTests))"

        $rawLines = @(& go test $check.Package -run $pattern -count=1 -json 2>&1 | ForEach-Object { [string] $_ })
        $exitCode = $LASTEXITCODE
        $events = New-Object System.Collections.Generic.List[object]
        $parseFailures = New-Object System.Collections.Generic.List[string]
        $logLines = New-Object System.Collections.Generic.List[string]
        foreach ($line in $rawLines) {
            if ([string]::IsNullOrWhiteSpace($line)) {
                continue
            }
            try {
                $event = $line | ConvertFrom-Json -ErrorAction Stop
                $events.Add($event)
                if (-not [string]::IsNullOrEmpty([string] $event.Output)) {
                    $logLines.Add([string] $event.Output)
                }
            } catch {
                $parseFailures.Add("non-json-go-test-output")
            }
        }
        $logText = [string]::Join("", [string[]] @($logLines))
        $marker = if ($check.ContainsKey("Marker")) { [string] $check.Marker } else { "" }
        $eventArray = $events.ToArray()
        $measurement = Measure-GoTestEvidence -Events $eventArray -ExpectedTests $expectedTests -SuccessMarker $marker -LogText $logText
        $failures = New-Object System.Collections.Generic.List[string]
        foreach ($failure in @($parseFailures)) { $failures.Add($failure) }
        foreach ($failure in @($measurement.Failures)) { $failures.Add($failure) }
        if ($exitCode -ne 0) { $failures.Add("go-test-exit:$exitCode") }

        foreach ($executed in @($measurement.Executed)) {
            $executedLines.Add("$($check.Name)`:$executed")
        }
        foreach ($skipped in @($measurement.Skipped)) {
            $skippedLines.Add("$($check.Name)`:$skipped")
        }

        $safeName = $check.Name -replace '[^A-Za-z0-9_.-]', '_'
        $captureRaw = -not $check.ContainsKey("CaptureRaw") -or [bool] $check.CaptureRaw
        $jsonName = "raw-$safeName.jsonl"
        $logName = "raw-$safeName.log"
        if ($captureRaw) {
            Write-Utf8Lines (Join-Path $outputDirectory $jsonName) ([string[]] @($rawLines))
            Write-Utf8Lines (Join-Path $outputDirectory $logName) ([string[]] @($logLines))
            Add-Evidence "raw[$($check.Name)]=$jsonName,$logName"
        } else {
            $jsonName = "omitted-sensitive-input-check"
            $logName = "omitted-sensitive-input-check"
            Add-Evidence "raw[$($check.Name)]=omitted-sensitive-input-check"
        }

        $passed = $failures.Count -eq 0
        Add-Evidence "executed[$($check.Name)]=$([string]::Join(',', @($measurement.Executed)))"
        Add-Evidence "skipped[$($check.Name)]=$([string]::Join(',', @($measurement.Skipped)))"
        if ($passed) {
            Add-Evidence "result[$($check.Name)]=pass"
        } else {
            Add-Evidence "result[$($check.Name)]=fail exit_code=$exitCode failures=$([string]::Join(',', @($failures)))"
            Write-FailureAnnotation $check.Name ([string[]] @($failures)) $exitCode
            $failed = $true
        }
        $manifestChecks.Add([PSCustomObject]@{
            name = $check.Name
            package = $check.Package
            method = $check.Method
            pattern = $pattern
            expected = $expectedTests
            executed = [string[]] @($measurement.Executed)
            skipped = [string[]] @($measurement.Skipped)
            success_marker = $marker
            exit_code = $exitCode
            passed = $passed
            failures = [string[]] @($failures)
            raw_json = $jsonName
            raw_log = $logName
        })
    }
} finally {
    Pop-Location
}

Write-Utf8Lines (Join-Path $outputDirectory "expected-tests.txt") ([string[]] @($expectedLines))
Write-Utf8Lines (Join-Path $outputDirectory "executed-tests.txt") ([string[]] @($executedLines))
Write-Utf8Lines (Join-Path $outputDirectory "skipped-tests.txt") ([string[]] @($skippedLines))
$manifest = [ordered]@{
    schema_version = 1
    candidate_sha = $candidateSHA
    checkout_sha = $checkoutSHA
    runner_os = $env:RUNNER_OS_NAME
    runner_arch = $env:RUNNER_ARCH_NAME
    powershell_host_os = $hostIdentity.GOOS
    powershell_host_arch = $hostIdentity.GOARCH
    goos = $goos
    goarch = $goarch
    gohostos = $gohostos
    gohostarch = $gohostarch
    go_version = $goVersion
    expected_tests = [string[]] @($expectedLines)
    executed_events = [string[]] @($executedLines)
    skipped_tests = [string[]] @($skippedLines)
    checks = [object[]] @($manifestChecks)
    passed = -not $failed
}
$manifestJSON = $manifest | ConvertTo-Json -Depth 10
Write-Utf8Lines (Join-Path $outputDirectory "manifest.json") @($manifestJSON)

if ($failed) {
    exit 1
}
