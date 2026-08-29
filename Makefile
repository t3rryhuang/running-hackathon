BINARY := runhack
COMMIT := $(shell git rev-parse HEAD 2>/dev/null)
BUILT_AT := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
# Stamped into the binary and reported by GET /version, so a deployment can be
# checked against the repo without shell access to the box.
LDFLAGS := -s -w -X main.commit=$(COMMIT) -X main.buildTime=$(BUILT_AT)

.PHONY: build native run test vet fmt clean deploy

# Deployment target: Raspberry Pi (linux/arm64), pure-Go SQLite so no CGO.
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BINARY)-arm64 .

# Local build for testing on this machine.
native:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

# Gated, idempotent deploy to the Pi. See DEPLOY.md for prerequisites.
deploy:
	./scripts/deploy.sh

run: native
	./$(BINARY)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

clean:
	rm -f $(BINARY) $(BINARY)-arm64
