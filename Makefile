# wotrlay Makefile
# Provides convenient commands for building, testing, and releasing

.PHONY: help build build-local test test-all docker docker-build docker-run docker-stop docker-clean release patch minor major

# Default target
.DEFAULT_GOAL := help

# Variables
BINARY_NAME=wotrlay
DOCKER_IMAGE=ghcr.io/contextvm/wotrlay
VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT=$(shell git rev-parse HEAD 2>/dev/null || echo "unknown")
BUILD_TIME=$(shell date -u '+%Y-%m-%d_%H:%M:%S')
LDFLAGS=-ldflags "-X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildTime=$(BUILD_TIME)"

## help: Show this help message
help:
	@echo "wotrlay - Web-of-Trust Nostr Relay"
	@echo ""
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' | sed -e 's/^/ /'

## build: Build the binary for local platform
build:
	@echo "Building $(BINARY_NAME) v$(VERSION)..."
	@go build $(LDFLAGS) -o $(BINARY_NAME) .
	@echo "Build complete: ./$(BINARY_NAME)"

## build-local: Build and run locally
build-local: build
	@echo "Starting $(BINARY_NAME) locally..."
	@./$(BINARY_NAME)

## test: Run unit tests
test:
	@echo "Running tests..."
	@go test -v ./...

## test-all: Run tests with coverage
test-all:
	@echo "Running tests with coverage..."
	@go test -v -coverprofile=coverage.out ./...
	@go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

## docker: Build and run with Docker Compose
docker:
	@echo "Building and starting Docker container..."
	@docker compose up -d --build
	@echo "Container started. Check logs with: docker compose logs -f"

## docker-build: Build Docker image
docker-build:
	@echo "Building Docker image $(DOCKER_IMAGE):$(VERSION)..."
	@docker build -t $(DOCKER_IMAGE):$(VERSION) -t $(DOCKER_IMAGE):latest .

## docker-run: Run Docker container
docker-run:
	@echo "Running Docker container..."
	@docker run -d --name $(BINARY_NAME) -p 3334:3334 -v wotrlay_data:/app/badger $(DOCKER_IMAGE):latest
	@echo "Container started. Check logs with: docker logs -f $(BINARY_NAME)"

## docker-stop: Stop Docker container
docker-stop:
	@echo "Stopping Docker container..."
	@docker stop $(BINARY_NAME) || true
	@docker rm $(BINARY_NAME) || true
	@echo "Container stopped"

## docker-clean: Clean up Docker resources
docker-clean: docker-stop
	@echo "Cleaning up Docker resources..."
	@docker compose down -v || true
	@docker rmi $(DOCKER_IMAGE):latest || true
	@echo "Cleanup complete"

## docker-push: Build and push Docker image to registry
docker-push: docker-build
	@echo "Pushing Docker image to registry..."
	@docker push $(DOCKER_IMAGE):$(VERSION)
	@docker push $(DOCKER_IMAGE):latest
	@echo "Image pushed: $(DOCKER_IMAGE):$(VERSION)"

## clean: Clean build artifacts
clean:
	@echo "Cleaning build artifacts..."
	@rm -f $(BINARY_NAME)
	@rm -f coverage.out coverage.html
	@echo "Clean complete"

## version: Show current version
version:
	@echo "Current version: $(VERSION)"
	@echo "Commit: $(COMMIT)"
	@echo "Build time: $(BUILD_TIME)"

# Release targets
CURRENT_VERSION := $(shell git describe --tags --abbrev=0 2>/dev/null || echo "v0.1.0")
NEXT_PATCH := $(shell echo $(CURRENT_VERSION) | awk -F. '{print $$1"."$$2"."$$3+1}')
NEXT_MINOR := $(shell echo $(CURRENT_VERSION) | awk -F. '{print $$1"."$$2+1".0"}')
NEXT_MAJOR := $(shell echo $(CURRENT_VERSION) | awk -F. '{print $$1+1".0.0"}')

## release: Create and push a new tag (use VERSION=v1.2.3)
release:
	@if [ -z "$(VERSION)" ]; then echo "Usage: make release VERSION=v1.2.3"; exit 1; fi
	@echo "Creating release $(VERSION)..."
	@git tag -a $(VERSION) -m "Release $(VERSION)"
	@git push origin $(VERSION)
	@echo "Release $(VERSION) created and pushed"

## patch: Create and push a patch release (e.g., v1.2.3 -> v1.2.4)
patch:
	@echo "Creating patch release $(NEXT_PATCH)..."
	@git tag -a $(NEXT_PATCH) -m "Release $(NEXT_PATCH)"
	@git push origin $(NEXT_PATCH)
	@echo "Patch release $(NEXT_PATCH) created and pushed"

## minor: Create and push a minor release (e.g., v1.2.3 -> v1.3.0)
minor:
	@echo "Creating minor release $(NEXT_MINOR)..."
	@git tag -a $(NEXT_MINOR) -m "Release $(NEXT_MINOR)"
	@git push origin $(NEXT_MINOR)
	@echo "Minor release $(NEXT_MINOR) created and pushed"

## major: Create and push a major release (e.g., v1.2.3 -> v2.0.0)
major:
	@echo "Creating major release $(NEXT_MAJOR)..."
	@git tag -a $(NEXT_MAJOR) -m "Release $(NEXT_MAJOR)"
	@git push origin $(NEXT_MAJOR)
	@echo "Major release $(NEXT_MAJOR) created and pushed"

## release-docker: Create release and push Docker image
release-docker: release docker-push
	@echo "Release complete! Docker image pushed to registry."

## dev: Set up development environment
dev:
	@echo "Setting up development environment..."
	@go mod download
	@go mod tidy
	@echo "Development environment ready"

## fmt: Format code
fmt:
	@echo "Formatting code..."
	@go fmt ./...
	@echo "Code formatted"

## vet: Run go vet
vet:
	@echo "Running go vet..."
	@go vet ./...
	@echo "Vet complete"

## lint: Run linter (requires golangci-lint)
lint:
	@echo "Running linter..."
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed. Install with: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest"; \
	fi

## check: Run all checks (fmt, vet, test)
check: fmt vet test
	@echo "All checks passed!"

## install: Install binary to GOPATH
install: build
	@echo "Installing $(BINARY_NAME) to GOPATH..."
	@go install .
	@echo "Installed: $$(which $(BINARY_NAME))"
