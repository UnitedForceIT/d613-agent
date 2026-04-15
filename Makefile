.PHONY: all agent connect clean

VERSION ?= dev
LDFLAGS  = -ldflags "-s -w -X main.version=$(VERSION)"

dist:
	mkdir -p dist

agent: dist
	GOOS=darwin  GOARCH=arm64 go build $(LDFLAGS) -o dist/d613-agent-darwin-arm64        ./cmd/agent/
	GOOS=darwin  GOARCH=amd64 go build $(LDFLAGS) -o dist/d613-agent-darwin-amd64        ./cmd/agent/
	GOOS=linux   GOARCH=amd64 go build $(LDFLAGS) -o dist/d613-agent-linux-amd64         ./cmd/agent/
	GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o dist/d613-agent-windows-amd64.exe   ./cmd/agent/

connect: dist
	GOOS=darwin  GOARCH=arm64 go build $(LDFLAGS) -o dist/d613-connect-darwin-arm64      ./cmd/connect/
	GOOS=darwin  GOARCH=amd64 go build $(LDFLAGS) -o dist/d613-connect-darwin-amd64      ./cmd/connect/
	GOOS=linux   GOARCH=amd64 go build $(LDFLAGS) -o dist/d613-connect-linux-amd64       ./cmd/connect/
	GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o dist/d613-connect-windows-amd64.exe ./cmd/connect/

all: agent connect

# Build just for the current platform (fast, for testing)
dev:
	go build -o dist/d613-agent   ./cmd/agent/
	go build -o dist/d613-connect ./cmd/connect/

clean:
	rm -rf dist/
