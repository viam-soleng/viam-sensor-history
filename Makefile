MODULE_BINARY := bin/module

$(MODULE_BINARY): go.mod *.go
	go build -o $(MODULE_BINARY)

lint:
	gofmt -s -w .

update:
	go get go.viam.com/rdk@latest
	go mod tidy

test:
	go test ./...

module.tar.gz: meta.json $(MODULE_BINARY)
	strip $(MODULE_BINARY)
	tar czf $@ meta.json $(MODULE_BINARY)

all: test module.tar.gz

setup:
	go mod tidy