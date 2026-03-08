.PHONY: build test test-race

build:
	go build -o udl ./cmd/udl

test:
	go test ./... -count=1

test-race:
	go test -race ./... -count=1
