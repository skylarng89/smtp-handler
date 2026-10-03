BINARY := bin/smtp-handler
PKG    := ./cmd/smtp-handler

.PHONY: all build ui test test-race lint vet vuln fuzz clean

all: build

ui:
	cd web && npm ci && npm run build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o $(BINARY) $(PKG)

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run

vuln:
	govulncheck ./...

fuzz:
	go test ./internal/payload -run '^$$' -fuzz FuzzDecode -fuzztime 30s
	go test ./internal/payload -run '^$$' -fuzz FuzzParseAddress -fuzztime 30s

clean:
	rm -rf bin
