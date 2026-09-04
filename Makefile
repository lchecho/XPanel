.PHONY: fmt vet test test-race build contract check

fmt:
	@test -z "$$(gofmt -l cmd internal tests 2>/dev/null)" || \
		{ echo "gofmt required"; gofmt -l cmd internal tests; exit 1; }

vet:
	go vet ./...

test:
	go test ./...

test-race:
	go test -race ./...

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -o bin/xpanel ./cmd/xpanel

contract:
	@if [ -z "$(XRAY_BIN)" ]; then \
		if [ "$(XPANEL_REQUIRE_CONTRACT)" = "1" ]; then \
			echo "XRAY_BIN is required for the release contract gate"; exit 1; \
		fi; \
		echo "WARNING: XRAY_BIN is not set; Xray real-process contract tests will skip"; \
	fi
	XRAY_BIN="$(XRAY_BIN)" XPANEL_REQUIRE_CONTRACT="$(XPANEL_REQUIRE_CONTRACT)" go test ./tests/contract/xray

check: fmt vet test test-race
	XPANEL_REQUIRE_CONTRACT=1 $(MAKE) contract
