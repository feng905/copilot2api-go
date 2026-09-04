DOCKER_VERSION = 0.0.2
FRONTEND_DIR = ./web
BACKEND_DIR = .

# Build metadata shown in the web console title ({short commit}-{build time}).
# Overridable from the environment: make linux COMMIT=abc1234
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date '+%Y%m%d-%H%M')
VERSION_LDFLAGS := -X copilot-go/buildinfo.Commit=$(COMMIT) -X copilot-go/buildinfo.BuildTime=$(BUILD_TIME)


echo:
	@echo "DOCKER_VERSION=$(DOCKER_VERSION)"

.PHONY: all web run

all: web run

web:
	@echo "Building frontend..."
	@cd $(FRONTEND_DIR) && bun install --frozen-lockfile && bun run build

run: web
	@echo "Starting backend dev server..."
	@cd $(BACKEND_DIR) && go run -ldflags "$(VERSION_LDFLAGS)" main.go

linux: web
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(VERSION_LDFLAGS)" -o copilot-go

mac: web
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(VERSION_LDFLAGS)" -o copilot-go

deploy:
	@echo "Building Docker image..."
	@docker build --platform linux/amd64 -f Dockerfile --build-arg COMMIT=$(COMMIT) --build-arg BUILD_TIME=$(BUILD_TIME) -t hub.metami.work/copilot-go/copilot-go:$(DOCKER_VERSION) -t hub.metami.work/copilot-go/copilot-go:latest .
	@echo "Pushing Docker image to registry..."
	@docker push hub.metami.work/copilot-go/copilot-go:$(DOCKER_VERSION)
	@docker push hub.metami.work/copilot-go/copilot-go:latest
	@echo "Deploy completed successfully!"

fast: linux
	@echo "Building Docker image..."
	@docker build --platform linux/amd64 -f Dockerfile.fast -t hub.metami.work/copilot-go/copilot-go:$(DOCKER_VERSION) -t hub.metami.work/copilot-go/copilot-go:latest .
	@echo "Pushing Docker image to registry..."
	@docker push hub.metami.work/copilot-go/copilot-go:$(DOCKER_VERSION)
	@docker push hub.metami.work/copilot-go/copilot-go:latest
	@echo "Deploy completed successfully!"
