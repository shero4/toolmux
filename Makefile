.PHONY: build test run

build:
	go build -o toolmux ./cmd/toolmux

test:
	go vet ./...
	go test ./...

run:
	go run ./cmd/toolmux
