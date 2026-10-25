VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)

.PHONY: build run test clean

build:
	@go build -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN) ./cmd/smartdb

run: 
	@go run ./cmd/smartdb/main.go -path ./exemplos/dados -user admin -password admin -watch

test:
	@go test ./...

clean:
	@rm -rf bin
