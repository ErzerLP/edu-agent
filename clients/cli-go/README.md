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

## Commands

```text
edu-agent pair [--server URL] [--name NAME]
edu-agent device status
edu-agent device forget-local
edu-agent logout
edu-agent knowledge import <file-or-directory>
edu-agent goal set <text>
edu-agent learn
edu-agent assessment show|confirm|override|void
edu-agent route [--history] [--limit N] [--cursor CURSOR]
edu-agent progress [--all]
edu-agent evidence [--node ID] [--limit N] [--cursor CURSOR]
edu-agent reviews [--due-before RFC3339] [--limit N] [--cursor CURSOR]
edu-agent clear
edu-agent version
```

Pairing codes are read without echo from a TTY or as one line from non-TTY stdin. There is no `--code` flag. Pairing output never includes the device token.

`learn` resumes exclusively from the server-provided `SessionView.work_item`. Its interactive commands are `:ask`, `:answer`, `:quiz`, `:resume`, `:assessment`, `:progress`, `:route`, `:reviews`, `:clear`, `:end`, `:complete`, `:quit`, and `:help`. `:answer` starts a multiline block terminated by a line containing only `.`. Plain text is an answer only while awaiting a response, and is a follow-up question only while showing a free answer. `:quit` exits the client without ending the server session.

The CLI uses the server's allowed actions and assessment decisions. Provisional feedback is not shown as accepted evidence and must be confirmed, overridden, or voided before feedback can be acknowledged. Objective activities use the deterministic server assessment path; open activities request a frozen proposal. A successful mutation is immediately followed by a fresh session read. A version conflict refreshes the work item but never automatically replays an answer or decision.

Free answers are explicitly non-scoring. Enter on a free answer calls `resume_focus`; converting a free answer to a quiz follows the normal attempt and assessment flow, then still requires an explicit resume after feedback.

## Local State

Ordinary configuration is stored under `os.UserConfigDir()/edu-agent/config.json`. It contains the server URL, device ID, display name, timeout, color setting, and explicit insecure HTTP selection. It does not contain the device token or learning content.

On Unix, the credential is a separate `0600` file under a `0700` directory. Security-sensitive reads use no-follow file handles and reject symlinks, non-regular files, and broad permissions. Markdown directory imports hold an open root handle and resolve every relative component with `openat` plus `O_NOFOLLOW`. On Windows, imports reject intermediate reparse points and require each final resolved file handle to remain strictly below the resolved root; the separate credential payload is protected with current-user DPAPI before it is written.

`EDU_AGENT_TOKEN` is a process-only override and is never persisted. It is accepted only when a complete local config/credential pair already exists and `EDU_AGENT_TOKEN_SERVER` plus `EDU_AGENT_TOKEN_DEVICE_ID` explicitly match that pair; it cannot replace a missing half or bypass a binding mismatch.

Pairing first writes a fail-closed pending journal, then saves the credential and atomically publishes ordinary configuration. The journal is removed only after both halves are durable. Any failed publication or compensation leaves startup blocked until `edu-agent device forget-local` removes config, credential, and journal. The command changes local state only; the remote device may remain valid and must be revoked from another paired device.

## Connection And Terminal Boundaries

The default server is `http://127.0.0.1:8080`. Plain HTTP to a non-loopback host is rejected unless explicitly approved with `--allow-insecure-http`; every such network command prints a warning. URLs with embedded credentials, query strings, or fragments are rejected. Redirects are disabled.

The default color mode is `never`. `edu-agent clear`, interactive `:clear`, and Ctrl-L clear only the visible application viewport in a TTY and redraw a neutral `>` prompt. They do not clear terminal scrollback, shell history, OS audit records, remote terminal logs, server events, projections, or credentials. Non-TTY clear emits no control sequence and returns a diagnostic error. The implementation does not execute `clear`, `cls`, a shell, or another external command.

Text entered directly in a shell command, including `goal set` text, may be retained by shell history. Interactive `learn` keeps answers and free questions out of argv and does not create a persistent input history.

This is an online client. Network failures do not create an offline business queue, and the CLI does not persist Markdown, goals, activities, attempts, answers, assessments, free questions, free answers, routes, evidence, progress, cursors, or pending operations. Proposal input is `go-cli-context-v1` and contains only authoritative work-item records plus canonical retrieval IDs, ranges, slices, and hashes returned by the server.
