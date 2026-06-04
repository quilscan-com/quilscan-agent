.PHONY: build sign test clean

BINARY := quilscan-agent
BUILD_DIR := dist

build:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o $(BUILD_DIR)/$(BINARY)-linux-amd64 ./cmd/agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o $(BUILD_DIR)/$(BINARY)-linux-arm64 ./cmd/agent
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o $(BUILD_DIR)/$(BINARY)-darwin-arm64 ./cmd/agent

sign:
	set -a; . $$HOME/.config/quilscan/keys/dev-node-signing.env; set +a; go run ./scripts/sign-agent-artifacts.go \
		$(BUILD_DIR)/$(BINARY)-linux-amd64 \
		$(BUILD_DIR)/$(BINARY)-linux-arm64 \
		$(BUILD_DIR)/$(BINARY)-darwin-arm64

test:
	go test -race -v ./...

clean:
	rm -rf $(BUILD_DIR)
