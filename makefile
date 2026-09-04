DOCKER_VERSION = 0.0.1
FRONTEND_DIR = ./web
BACKEND_DIR = .


echo:
	@echo "DOCKER_VERSION=$(DOCKER_VERSION)"

.PHONY: all web run

all: web run

web:
	@echo "Building frontend..."
	@cd $(FRONTEND_DIR) && bun install --frozen-lockfile && bun run build

run: web
	@echo "Starting backend dev server..."
	@cd $(BACKEND_DIR) && go run main.go

linux: web
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o copilot-go

deploy:
	@echo "Building Docker image..."
	@docker build --platform linux/amd64 -f Dockerfile -t hub.metami.work/copilot-go/copilot-go:$(DOCKER_VERSION) -t hub.metami.work/copilot-go/copilot-go:latest .
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