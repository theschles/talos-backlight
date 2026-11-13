PYTHON ?= python3
GOVULNCHECK_VERSION := v1.9.0

.PHONY: build test format lint typecheck audit-python audit-go audit check
build:
	mkdir -p build/release
	cd controller && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath -ldflags="-s -w" -o ../build/controller-amd64 .
	$(PYTHON) package-controller.py build/controller-amd64 build/cache build/release

test:
	cd controller && go test -race ./...
	$(PYTHON) -m unittest discover -s tests -v

format:
	$(PYTHON) -m ruff check --select I --fix .
	$(PYTHON) -m ruff format .
	gofmt -w controller/*.go

lint:
	$(PYTHON) -m ruff format --check .
	$(PYTHON) -m ruff check .
	@files="$$(gofmt -l controller/*.go)" && test -z "$$files" || { printf 'Go files need formatting or contain syntax errors:\n%s\n' "$$files"; exit 1; }
	cd controller && go vet ./...

typecheck:
	$(PYTHON) -m mypy

audit-python:
	@cache="$$(mktemp -d)" && trap 'rm -rf "$$cache"' 0 && \
		$(PYTHON) -m pip_audit --cache-dir "$$cache" -r requirements-dev.txt

audit-go:
	GOBIN="$(CURDIR)/build/tools" go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	cd controller && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 ../build/tools/govulncheck ./...

audit: audit-python audit-go

check: lint typecheck test audit
