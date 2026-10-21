.PHONY: run build windows mac linux source source-windows installer test tidy

run:
	cd controller && go run ./cmd/civitone

build: mac windows linux source-windows

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
SRCFLAGS = -ldflags "-X github.com/Retty-Software/civitone/controller/internal/srcapp.Version=$(VERSION)"

mac:
	cd controller && CGO_ENABLED=0 go build -o ../dist/civitone ./cmd/civitone

windows:
	cd controller && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o ../dist/civitone.exe ./cmd/civitone

linux:
	cd controller && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ../dist/civitone-linux ./cmd/civitone

# Civitone Source, the app that sends a computer's audio to the server.
source:
	cd controller && CGO_ENABLED=0 go build $(SRCFLAGS) -o ../dist/civitone-source ./cmd/civitone-source

source-windows:
	cd controller && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(SRCFLAGS) -o ../dist/civitone-source.exe ./cmd/civitone-source

# Windows installer: GUI build (no console window) wrapped by NSIS.
# Uses makensis if installed (apt install nsis), else NSIS in a Debian container.
installer:
	cd controller && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "-H windowsgui -X github.com/Retty-Software/civitone/controller/internal/srcapp.Version=$(VERSION)" -o ../dist/civitone-source-gui.exe ./cmd/civitone-source
	if command -v makensis >/dev/null; then \
	  cd installer && makensis -V2 -DVERSION=$(VERSION) -DSRCEXE=../dist/civitone-source-gui.exe -DOUTFILE=../dist/CivitoneSource-Setup.exe civitone-source.nsi; \
	else \
	  docker run --rm -v "$(CURDIR)":/w -w /w/installer debian:trixie-slim sh -c \
	    'apt-get update -qq >/dev/null && apt-get install -y -qq nsis >/dev/null && makensis -V2 -DVERSION=$(VERSION) -DSRCEXE=../dist/civitone-source-gui.exe -DOUTFILE=../dist/CivitoneSource-Setup.exe civitone-source.nsi'; \
	fi

test:
	cd controller && go test ./...

tidy:
	cd controller && go mod tidy
