MODULE := github.com/vtpl1/vrtc-sdk

.PHONY: all prerequisite fmt lint build test update bump clean

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

bump:
	@set -eu; \
	part="$${PART:-patch}"; \
	current="$$(sed -nE 's/^\*\*Version:\*\* (v[0-9]+\.[0-9]+\.[0-9]+).*/\1/p' README.md | head -n 1)"; \
	if [ -z "$$current" ]; then \
		echo "could not find current version in README.md"; \
		exit 1; \
	fi; \
	next="$$(echo "$$current" | awk -F. -v part="$$part" 'BEGIN{OFS="."} { \
		gsub(/^v/, "", $$1); \
		major=$$1; minor=$$2; patch=$$3; \
		if (part == "major") { major++; minor=0; patch=0 } \
		else if (part == "minor") { minor++; patch=0 } \
		else if (part == "patch") { patch++ } \
		else { exit 2 } \
		print "v" major, minor, patch \
	}')"; \
	if [ -z "$$next" ]; then \
		echo "invalid version bump part '$$part' (expected patch|minor|major)"; \
		exit 1; \
	fi; \
	sed -i "s/$$current/$$next/g" README.md; \
	echo "bumped version $$current -> $$next"

clean:
