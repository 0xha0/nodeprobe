# nodeprobe: `make` builds the binary for this machine; `make test` vets and runs the tests with the
# race detector; `make lint`, `make vuln` (govulncheck, pinned); `make release` the tarballs for linux
# amd64/arm64 and darwin arm64 into dist/, each with its sha256 (the workflow builds the image)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS := -trimpath -buildvcs=false

.PHONY: all build test lint vuln release clean

all: build

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o nodeprobe ./cmd/nodeprobe

test:
	go vet ./... && go test -race ./...

lint:
	golangci-lint run ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

release:
	rm -rf dist && mkdir -p dist
	for t in linux/amd64 linux/arm64 darwin/arm64; do \
	  os=$${t%/*}; arch=$${t#*/}; d=dist/nodeprobe-$(VERSION)-$$os-$$arch; mkdir -p $$d; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $$d/nodeprobe ./cmd/nodeprobe || exit 1; \
	  cp LICENSE README.md nodeprobe.example.yaml $$d/; \
	  (cd dist && tar --owner=0 --group=0 --numeric-owner --mtime=@0 --sort=name -czf nodeprobe-$(VERSION)-$$os-$$arch.tar.gz nodeprobe-$(VERSION)-$$os-$$arch) ; \
	  rm -rf $$d; \
	done
	cd dist && sha256sum *.tar.gz > sha256sums.txt && cat sha256sums.txt

clean:
	rm -rf nodeprobe dist
