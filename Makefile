.PHONY: build test clean lint vet tidy run

VERSION ?= dev
BIN     := bin/network-ultra-server
export NU_BUILD_VERSION := $(VERSION)

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.buildVersion=$${NU_BUILD_VERSION}" -o $(BIN) ./cmd/server

run: build
	./$(BIN) -config ./config.local.toml

test:
	go test -race ./...

vet:
	go vet ./...

lint:
	go vet ./...
	gofmt -l . | tee /dev/stderr | wc -l | grep -q '^0$$'

tidy:
	go mod tidy

clean:
	rm -rf bin dist

cross:
	mkdir -p dist
	@printf '%s\n' "$${NU_BUILD_VERSION}" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$$' || (echo "VERSION=<canonical-semver> is required for cross/release builds" >&2; exit 1)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -buildid=network-ultra-server/$${NU_BUILD_VERSION}/linux-amd64 -X main.buildVersion=$${NU_BUILD_VERSION}" -o dist/network-ultra-server-linux-amd64 ./cmd/server
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -buildid=network-ultra-server/$${NU_BUILD_VERSION}/linux-arm64 -X main.buildVersion=$${NU_BUILD_VERSION}" -o dist/network-ultra-server-linux-arm64 ./cmd/server
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -buildid=network-ultra-server/$${NU_BUILD_VERSION}/windows-amd64 -X main.buildVersion=$${NU_BUILD_VERSION}" -o dist/network-ultra-server-windows-amd64.exe ./cmd/server
	@test "$$(go tool buildid dist/network-ultra-server-linux-amd64)" = "network-ultra-server/$${NU_BUILD_VERSION}/linux-amd64"
	@test "$$(go tool buildid dist/network-ultra-server-linux-arm64)" = "network-ultra-server/$${NU_BUILD_VERSION}/linux-arm64"
	@test "$$(go tool buildid dist/network-ultra-server-windows-amd64.exe)" = "network-ultra-server/$${NU_BUILD_VERSION}/windows-amd64"
	@test "$$(dist/network-ultra-server-linux-amd64 -version)" = "network-ultra-server $${NU_BUILD_VERSION}"
	cd dist && for f in network-ultra-server-linux-amd64 network-ultra-server-linux-arm64 network-ultra-server-windows-amd64.exe; do sha256sum "$$f" > "$$f.sha256"; done
