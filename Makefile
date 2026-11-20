# Windows builds (cross-compiles from macOS/Linux; also works with GNU make on Windows).
# Runtime requirement: ffmpeg.exe on PATH or passed via -ffmpeg.

BINARY  := nvrfetch
DIST    := dist
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS := -trimpath

export CGO_ENABLED := 0

.PHONY: all windows windows-amd64 windows-arm64 test vet clean

all: windows

windows: windows-amd64 windows-arm64

windows-amd64: export GOOS := windows
windows-amd64: export GOARCH := amd64
windows-amd64:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-windows-amd64.exe .

windows-arm64: export GOOS := windows
windows-arm64: export GOARCH := arm64
windows-arm64:
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-windows-arm64.exe .

test: vet
	go test -race ./...

vet:
	go vet ./...
	GOOS=windows go vet ./...

clean:
	go clean
	rm -rf $(DIST)
