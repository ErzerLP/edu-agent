# edu-agent Go CLI

`clients/cli-go` is an independent Go module for the online `edu-agent` client. It uses only the public HTTP/OpenAPI boundary and does not import server internals.

## Build

Go 1.26.6 is required.

```sh
make cli-test
make cli-vet
make cli-build
./clients/cli-go/bin/edu-agent version
```

`make cli-cross-build` compiles Linux, macOS, and Windows binaries for amd64 and arm64 with `CGO_ENABLED=0` and `-trimpath`. Cross-builds prove compilation only. `.github/workflows/cli-platform.yml` runs SHA-bound root-confinement, credential, hidden/line input, Ctrl-L, and clear checks on native Linux, macOS, and Windows runners and uploads one artifact per runner. Hidden input calls the production `ReadSecret`: Linux and macOS use a real PTY with an echo-mode check, while Windows uses ConPTY. A missing native mechanism fails the job; missing native artifacts remain a release blocker.

`make cli-release` writes binaries and `SHA256SUMS` under `clients/cli-go/dist/`. The release directory contains no configuration, credentials, or learning content.

## Commands In This Foundation

```text
edu-agent pair [--server URL] [--name NAME]
edu-agent device status
edu-agent device forget-local
edu-agent logout
edu-agent knowledge import <file-or-directory>
edu-agent clear
edu-agent version
```

Pairing codes are read without echo from a TTY or as one line from non-TTY stdin. There is no `--code` flag. Pairing output never includes the device token.

The teaching commands `goal`, `learn`, `assessment`, `route`, `progress`, `evidence`, and `reviews` are intentionally not implemented in this foundation because they depend on the pending authoritative session `work_item` contract.

## Local State

Ordinary configuration is stored under `os.UserConfigDir()/edu-agent/config.json`. It contains the server URL, device ID, display name, timeout, color setting, and explicit insecure HTTP selection. It does not contain the device token or learning content.

On Unix, the credential is a separate `0600` file under a `0700` directory. Security-sensitive reads use no-follow file handles and reject symlinks, non-regular files, and broad permissions. Markdown directory imports hold an open root handle and resolve every relative component with `openat` plus `O_NOFOLLOW`. On Windows, imports reject intermediate reparse points and require each final resolved file handle to remain strictly below the resolved root; the separate credential payload is protected with current-user DPAPI before it is written.

`EDU_AGENT_TOKEN` is a process-only override and is never persisted. It is accepted only when a complete local config/credential pair already exists and `EDU_AGENT_TOKEN_SERVER` plus `EDU_AGENT_TOKEN_DEVICE_ID` explicitly match that pair; it cannot replace a missing half or bypass a binding mismatch.

Pairing first writes a fail-closed pending journal, then saves the credential and atomically publishes ordinary configuration. The journal is removed only after both halves are durable. Any failed publication or compensation leaves startup blocked until `edu-agent device forget-local` removes config, credential, and journal. The command changes local state only; the remote device may remain valid and must be revoked from another paired device.

## Connection And Terminal Boundaries

The default server is `http://127.0.0.1:8080`. Plain HTTP to a non-loopback host is rejected unless explicitly approved with `--allow-insecure-http`; every such network command prints a warning. URLs with embedded credentials, query strings, or fragments are rejected. Redirects are disabled.

The default color mode is `never`. `edu-agent clear` clears only the visible application viewport in a TTY and redraws a neutral `>` prompt. It does not clear terminal scrollback, shell history, OS audit records, remote terminal logs, server events, projections, or credentials. Non-TTY clear emits no control sequence and returns a diagnostic error. The implementation does not execute `clear`, `cls`, a shell, or another external command.

This is an online client. Network failures do not create an offline business queue, and the CLI does not persist Markdown, answers, assessments, routes, progress, or session content.
