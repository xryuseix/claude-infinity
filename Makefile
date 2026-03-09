BINARY_NAME := claude2
GO := go
GOFLAGS :=
LDFLAGS :=

.PHONY: all build lint fmt vet clean install test

all: fmt lint build test

build:
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY_NAME) .

lint: vet
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "[WARN] golangci-lint が見つかりません。vet のみ実行しました。"; \
	fi

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

test:
	$(GO) test -v ./...

clean:
	rm -f $(BINARY_NAME)

install: build
	cp $(BINARY_NAME) /usr/local/bin/
