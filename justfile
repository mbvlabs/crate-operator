# Crate Operator Agent development commands
set dotenv-load

# Default recipe - show available commands
default:
    @just --list

# Generate Go code from OpenAPI spec
generate:
    go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest --config oapi-codegen.yaml api/openapi.yaml

# Run the agent
run:
    go run .

# Build the agent
build:
    go build -o bin/crate-operator .

# Run tests
test:
    go test ./...

# Clean build artifacts
clean:
    rm -rf bin/
