.PHONY: fmt server-fmt cli-fmt test server-test cli-test test-race server-test-race cli-test-race \
	vet server-vet cli-vet build server-build cli-build cli-check cli-cross-build cli-platform-evidence cli-release \
	cli-m1-blackbox cli-m1-blackbox-run postgres-candidate postgres-candidate-resume postgres-candidate-shard check

fmt: server-fmt cli-fmt

server-fmt:
	cd server && go fmt ./...

cli-fmt:
	cd clients/cli-go && go fmt ./...

test: server-test cli-test

server-test:
	cd server && go test ./...

cli-test:
	cd clients/cli-go && go test ./...

test-race: server-test-race cli-test-race

server-test-race:
	cd server && go test -race ./...

cli-test-race:
	cd clients/cli-go && go test -race ./...

vet: server-vet cli-vet

server-vet:
	cd server && go vet ./...

cli-vet:
	cd clients/cli-go && go vet ./...

build: server-build cli-build

server-build:
	cd server && go build -o edu-agentd ./cmd/edu-agentd

cli-build:
	mkdir -p clients/cli-go/bin
	cd clients/cli-go && CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$${CLI_VERSION:-dev} -X main.commit=$${CLI_COMMIT:-unknown}" -o bin/edu-agent ./cmd/edu-agent

cli-check: cli-test cli-test-race cli-vet cli-build

cli-m1-blackbox:
	cd contracttests/fakellm && go test ./...
	cd contracttests/cli-m1 && go test ./responseproxy ./cmd/response-loss-proxy
	@test -n "$$TEST_DATABASE_URL" || { echo "cli-m1-blackbox requires TEST_DATABASE_URL after independent fixture contracts pass" >&2; exit 2; }
	cd contracttests/cli-m1 && go test -p=1 -count=1 -v ./blackbox

cli-m1-blackbox-run:
	@test -n "$$CLI_BLACKBOX_RUN" || { echo "cli-m1-blackbox-run requires CLI_BLACKBOX_RUN" >&2; exit 2; }
	cd contracttests/fakellm && go test ./...
	cd contracttests/cli-m1 && go test ./responseproxy ./cmd/response-loss-proxy
	@test -n "$$TEST_DATABASE_URL" || { echo "cli-m1-blackbox-run requires TEST_DATABASE_URL" >&2; exit 2; }
	cd contracttests/cli-m1 && go test -p=1 -count=1 -v -run "$$CLI_BLACKBOX_RUN" ./blackbox

postgres-candidate:
	scripts/test-postgres-candidate.sh

postgres-candidate-resume:
	scripts/test-postgres-candidate.sh --resume

postgres-candidate-shard:
	@test -n "$$POSTGRES_SHARD" || { echo "postgres-candidate-shard requires POSTGRES_SHARD" >&2; exit 2; }
	scripts/test-postgres-candidate.sh --resume --shard "$$POSTGRES_SHARD"

cli-platform-evidence:
	pwsh -NoProfile -File clients/cli-go/scripts/cli-platform-evidence.ps1

cli-cross-build:
	rm -rf clients/cli-go/bin/cross
	mkdir -p clients/cli-go/bin/cross
	cd clients/cli-go && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-X main.version=$${CLI_VERSION:-dev} -X main.commit=$${CLI_COMMIT:-unknown}" -o bin/cross/edu-agent-linux-amd64 ./cmd/edu-agent
	cd clients/cli-go && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-X main.version=$${CLI_VERSION:-dev} -X main.commit=$${CLI_COMMIT:-unknown}" -o bin/cross/edu-agent-linux-arm64 ./cmd/edu-agent
	cd clients/cli-go && CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-X main.version=$${CLI_VERSION:-dev} -X main.commit=$${CLI_COMMIT:-unknown}" -o bin/cross/edu-agent-darwin-amd64 ./cmd/edu-agent
	cd clients/cli-go && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-X main.version=$${CLI_VERSION:-dev} -X main.commit=$${CLI_COMMIT:-unknown}" -o bin/cross/edu-agent-darwin-arm64 ./cmd/edu-agent
	cd clients/cli-go && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-X main.version=$${CLI_VERSION:-dev} -X main.commit=$${CLI_COMMIT:-unknown}" -o bin/cross/edu-agent-windows-amd64.exe ./cmd/edu-agent
	cd clients/cli-go && CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -trimpath -ldflags "-X main.version=$${CLI_VERSION:-dev} -X main.commit=$${CLI_COMMIT:-unknown}" -o bin/cross/edu-agent-windows-arm64.exe ./cmd/edu-agent

cli-release:
	rm -rf clients/cli-go/dist
	mkdir -p clients/cli-go/dist
	cd clients/cli-go && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-X main.version=$${CLI_VERSION:-dev} -X main.commit=$${CLI_COMMIT:-unknown}" -o dist/edu-agent-linux-amd64 ./cmd/edu-agent
	cd clients/cli-go && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-X main.version=$${CLI_VERSION:-dev} -X main.commit=$${CLI_COMMIT:-unknown}" -o dist/edu-agent-linux-arm64 ./cmd/edu-agent
	cd clients/cli-go && CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-X main.version=$${CLI_VERSION:-dev} -X main.commit=$${CLI_COMMIT:-unknown}" -o dist/edu-agent-darwin-amd64 ./cmd/edu-agent
	cd clients/cli-go && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-X main.version=$${CLI_VERSION:-dev} -X main.commit=$${CLI_COMMIT:-unknown}" -o dist/edu-agent-darwin-arm64 ./cmd/edu-agent
	cd clients/cli-go && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-X main.version=$${CLI_VERSION:-dev} -X main.commit=$${CLI_COMMIT:-unknown}" -o dist/edu-agent-windows-amd64.exe ./cmd/edu-agent
	cd clients/cli-go && CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -trimpath -ldflags "-X main.version=$${CLI_VERSION:-dev} -X main.commit=$${CLI_COMMIT:-unknown}" -o dist/edu-agent-windows-arm64.exe ./cmd/edu-agent
	cd clients/cli-go/dist && if command -v sha256sum >/dev/null 2>&1; then sha256sum edu-agent-* > SHA256SUMS; else shasum -a 256 edu-agent-* > SHA256SUMS; fi

check: test test-race vet build
