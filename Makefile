GO ?= go
BINARY ?= bin/quotacore
SBOM ?= dist/sbom.cdx.json

.PHONY: build binary test race vet lint check-docs license-check sbom ci

build:
	$(GO) build ./...

binary:
	CGO_ENABLED=0 $(GO) build -o $(BINARY) ./cmd/quotacore

test:
	$(GO) test ./...

# Separate from `test` because -race needs cgo, and a machine with no C compiler
# can still run every other gate. CI runs this, where gcc is present.
race:
	CGO_ENABLED=1 $(GO) test -race ./...

vet:
	$(GO) vet ./...

lint:
	@unformatted="$$(gofmt -l .)"; if [ -n "$$unformatted" ]; then printf 'gofmt is required on:\n%s\n' "$$unformatted"; exit 1; fi
	$(GO) vet ./...

check-docs:
	@if command -v pwsh >/dev/null 2>&1; then \
		pwsh -NoProfile -File tools/check-docs.ps1 -Strict; \
	elif command -v powershell >/dev/null 2>&1; then \
		powershell -NoProfile -File tools/check-docs.ps1 -Strict; \
	else \
		printf 'neither pwsh nor powershell was found, so the specification cannot be checked\n' >&2; exit 1; \
	fi

license-check: binary
	@if command -v pwsh >/dev/null 2>&1; then \
		pwsh -NoProfile -File tools/license-scan.ps1 -Binary $(BINARY); \
	elif command -v powershell >/dev/null 2>&1; then \
		powershell -NoProfile -File tools/license-scan.ps1 -Binary $(BINARY); \
	else \
		printf 'neither pwsh nor powershell was found, so no licence can be checked\n' >&2; exit 1; \
	fi

sbom: binary
	@if command -v pwsh >/dev/null 2>&1; then \
		pwsh -NoProfile -File tools/license-scan.ps1 -Binary $(BINARY) -SbomPath $(SBOM); \
	elif command -v powershell >/dev/null 2>&1; then \
		powershell -NoProfile -File tools/license-scan.ps1 -Binary $(BINARY) -SbomPath $(SBOM); \
	else \
		printf 'neither pwsh nor powershell was found, so no SBOM can be produced\n' >&2; exit 1; \
	fi

ci: check-docs lint test license-check
