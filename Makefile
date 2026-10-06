VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build build-linux test vet fmt

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/sbctl ./cmd/sbctl

build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/sbctl-linux-amd64 ./cmd/sbctl
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/sbctl-linux-arm64 ./cmd/sbctl

test: vet
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w $$(git ls-files '*.go')
