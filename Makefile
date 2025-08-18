MODULE_NAME := sensor-replay
MODULE_BINARY := bin/module
GO_FILES := $(shell find . -name '*.go' -not -path './vendor/*')

.PHONY: all build clean test lint setup module update

all: clean build module

$(MODULE_BINARY): $(GO_FILES) go.mod
	@echo "Building module binary..."
	@mkdir -p bin
	go build -o $(MODULE_BINARY) cmd/module/main.go

build: $(MODULE_BINARY)

module.tar.gz: meta.json $(MODULE_BINARY)
	@echo "Creating module archive..."
	@if [ -f $(MODULE_BINARY) ]; then \
		strip $(MODULE_BINARY) 2>/dev/null || true; \
	fi
	tar czf $@ meta.json $(MODULE_BINARY)
	@echo "Module archive created: $@"

module: module.tar.gz

clean:
	@echo "Cleaning build artifacts..."
	@rm -rf bin/
	@rm -f module.tar.gz
	@rm -f coverage.out

test:
	@echo "Running tests..."
	go test -v -cover ./...

lint:
	@echo "Running linter..."
	@if command -v golangci-lint &> /dev/null; then \
		golangci-lint run; \
	else \
		gofmt -s -w .; \
		go vet ./...; \
	fi

setup:
	@echo "Setting up module dependencies..."
	go mod download
	go mod tidy

update:
	@echo "Updating dependencies..."
	go get -u go.viam.com/rdk@latest
	go get -u go.viam.com/utils@latest
	go mod tidy

run-local: build
	@echo "Running module locally..."
	$(MODULE_BINARY)

debug: 
	@echo "Building debug version..."
	go build -gcflags="all=-N -l" -o $(MODULE_BINARY) cmd/module/main.go

# Cross-compilation targets
build-linux-arm64:
	GOOS=linux GOARCH=arm64 go build -o bin/module-linux-arm64 cmd/module/main.go

build-linux-amd64:
	GOOS=linux GOARCH=amd64 go build -o bin/module-linux-amd64 cmd/module/main.go

build-darwin-arm64:
	GOOS=darwin GOARCH=arm64 go build -o bin/module-darwin-arm64 cmd/module/main.go

build-all: build-linux-arm64 build-linux-amd64 build-darwin-arm64