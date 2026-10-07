VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build build-linux test vet fmt

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/supavise ./cmd/supavise

build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/supavise-linux-amd64 ./cmd/supavise
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/supavise-linux-arm64 ./cmd/supavise

test: vet
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w $$(git ls-files '*.go')
