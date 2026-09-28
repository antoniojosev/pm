BIN     ?= pm
DEST    ?= $(HOME)/.local/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test vet cover install clean

build: ## build the binary into ./pm
	go build -ldflags "-X github.com/antoniojosev/pm/internal/cli.Version=$(VERSION)" -o $(BIN) ./cmd/pm

test: ## run the test suite with the race detector
	go test -race ./...

vet: ## static checks
	go vet ./...
	@test -z "$$(gofmt -l cmd internal)" || { echo "gofmt needed:"; gofmt -l cmd internal; exit 1; }

cover: ## per-package coverage summary
	go test -race -cover ./...

install: build ## copy the binary into $(DEST)
	mkdir -p $(DEST)
	install -m 0755 $(BIN) $(DEST)/$(BIN)

clean:
	rm -f $(BIN)
