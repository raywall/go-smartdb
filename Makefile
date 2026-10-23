BIN := bin/jsondb
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)

.PHONY: build run test clean

build:
	go build -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN) ./cmd/jsondb

run: build
	./$(BIN) -path ./exemplos/dados -watch

test:
	go test ./...

clean:
	rm -rf bin
