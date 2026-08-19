.PHONY: fmt test test-race vet build check

fmt:
	gofmt -w server/cmd/edu-agentd/*.go server/internal/*/*.go server/internal/*/*/*.go server/internal/*/*/*/*.go server/migrations/*.go contracttests/fakellm/*.go

test:
	cd server && go test ./...

test-race:
	cd server && go test -race ./...

vet:
	cd server && go vet ./...

build:
	cd server && go build ./cmd/edu-agentd

check: test test-race vet build
