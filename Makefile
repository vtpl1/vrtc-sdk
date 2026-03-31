MODULE := github.com/vtpl1/vrtc-sdk

.PHONY: all prerequisite fmt lint build test clean

all: fmt lint build

prerequisite:
	@go get -tool github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	@go get -tool mvdan.cc/gofumpt@latest

fmt:
	go tool gofumpt -l -w -extra .

lint:
	go tool golangci-lint run --fix ./...

build:
	go build ./...

test:
	go test -race -count=1 ./...

update:
	go get -u ./...
	go mod tidy

clean:
