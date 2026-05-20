BINARY := agent-bridge
PKG := ./cmd/agent-bridge
INSTALL_DIR ?= $(HOME)/.local/bin

.PHONY: build test vet install clean snapshot

build:
	go build -o $(BINARY) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

install: build
	mkdir -p $(INSTALL_DIR)
	install -m 0755 $(BINARY) $(INSTALL_DIR)/$(BINARY)

snapshot:
	rm -rf dist
	mkdir -p dist
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -o dist/$(BINARY) $(PKG)
	tar -C dist -czf dist/$(BINARY)_darwin_amd64.tar.gz $(BINARY)
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o dist/$(BINARY) $(PKG)
	tar -C dist -czf dist/$(BINARY)_darwin_arm64.tar.gz $(BINARY)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o dist/$(BINARY) $(PKG)
	tar -C dist -czf dist/$(BINARY)_linux_amd64.tar.gz $(BINARY)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o dist/$(BINARY) $(PKG)
	tar -C dist -czf dist/$(BINARY)_linux_arm64.tar.gz $(BINARY)
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o dist/$(BINARY).exe $(PKG)
	cd dist && zip -q $(BINARY)_windows_amd64.zip $(BINARY).exe
	GOOS=windows GOARCH=arm64 CGO_ENABLED=0 go build -o dist/$(BINARY).exe $(PKG)
	cd dist && zip -q $(BINARY)_windows_arm64.zip $(BINARY).exe
	rm -f dist/$(BINARY) dist/$(BINARY).exe
	cd dist && shasum -a 256 $(BINARY)_* > checksums.txt

clean:
	rm -rf dist $(BINARY) $(BINARY).exe
