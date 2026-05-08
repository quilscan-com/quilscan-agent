.PHONY: build test clean

BINARY := quilscan-agent
BUILD_DIR := dist

build:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o $(BUILD_DIR)/$(BINARY)-linux-amd64 ./cmd/agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o $(BUILD_DIR)/$(BINARY)-linux-arm64 ./cmd/agent
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o $(BUILD_DIR)/$(BINARY)-darwin-arm64 ./cmd/agent

test:
	go test -race -v ./...

clean:
	rm -rf $(BUILD_DIR)
