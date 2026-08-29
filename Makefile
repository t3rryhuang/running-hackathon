BINARY := runhack

.PHONY: build native run test vet fmt clean

# Deployment target: Raspberry Pi (linux/arm64), pure-Go SQLite so no CGO.
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o $(BINARY)-arm64 .

# Local build for testing on this machine.
native:
	CGO_ENABLED=0 go build -o $(BINARY) .

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
