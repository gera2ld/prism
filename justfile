set shell := ["bash", "-cu"]
set dotenv-load

default:
    @just --list

# Build the Prism binary into ./build/
build:
    mkdir -p build
    go build -o build/prism .

# Run all tests
test:
    go test ./...

# Run tests with the race detector
test-race:
    go test -race ./...

# Run only integration tests (PocketBase-backed store)
test-integration:
    go test ./internal/store -v

# Run only proxy tests
test-proxy:
    go test ./internal/gateway -v

# Run go vet
vet:
    go vet ./...

# Run staticcheck (incl. SA1019 deprecated-use checks)
lint:
    go tool staticcheck ./...

# Format all Go files
fmt:
    gofmt -w .

# Check formatting without writing
fmt-check:
    @out=$(gofmt -l .); if [ -n "$out" ]; then echo "unformatted:"; echo "$out"; exit 1; fi

# Build, vet, lint, and test everything
check: build vet lint test fmt-check

# Remove build artifacts and local data
clean:
    rm -rf build pb_data

# Start the server (admin UI at /_/): just serve [http-addr]
serve http_addr="127.0.0.1:8090":
    go run . serve --http '{{http_addr}}'

# Run any CLI command with .env-provided secrets: just run route export routes.csv
run *args:
    go run . {{args}}
