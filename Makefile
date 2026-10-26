.PHONY: help install install-dev lint format typecheck test cov run clean docker \
	up up-gpu up-observability infra down ps logs models nuke gateway-test gateway-lint

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

TORCH ?= cpu  # PyTorch build: cpu or cu128

install: ## Install runtime package (TORCH=cpu|cu128)
	uv sync --locked --no-default-groups --group $(TORCH)

install-dev: ## Install with dev tools + pre-commit (TORCH=cpu|cu128)
	uv sync --locked --no-default-groups --group dev --group $(TORCH)
	uv run pre-commit install

lint: ## Run ruff linter
	uv run ruff check .

format: ## Auto-format
	uv run ruff format .
	uv run ruff check --fix .

typecheck: ## Run mypy
	uv run mypy

test: ## Run test suite
	uv run pytest

cov: ## Tests with coverage
	uv run pytest --cov=mhai --cov-report=term-missing

run: ## Run a quick smoke test
	uv run mhai run --query "Test query" --urgency routine

clean: ## Remove build artifacts
	rm -rf build dist *.egg-info .pytest_cache .mypy_cache .ruff_cache htmlcov .coverage
	find . -type d -name __pycache__ -exec rm -rf {} +

docker: ## Build container image (TORCH=cpu|cu128)
	docker build --build-arg TORCH=$(TORCH) -t mhai:$(TORCH) .

COMPOSE ?= docker compose
GPU := -f docker-compose.yml -f docker-compose.gpu.yml

.env:
	cp .env.example .env
	@echo "Created .env from .env.example; set POSTGRES_PASSWORD before sharing the stack."

up: .env ## Start the stack: postgres, redis, ollama (+ model pull), mhai
	$(COMPOSE) up -d --build

up-gpu: .env ## Start the stack on an NVIDIA GPU
	$(COMPOSE) $(GPU) up -d --build

up-observability: .env ## Start the stack plus the OpenTelemetry collector
	$(COMPOSE) --profile observability up -d --build

infra: .env ## Start only postgres, redis and ollama (run the app with uv)
	$(COMPOSE) up -d postgres redis ollama ollama-init

down: ## Stop the stack (keeps data volumes)
	$(COMPOSE) --profile observability down

ps: ## Show service status
	$(COMPOSE) ps

logs: ## Follow logs (SERVICE=name to filter)
	$(COMPOSE) logs -f $(SERVICE)

models: ## Pull OLLAMA_MODEL into the ollama volume again
	$(COMPOSE) run --rm ollama-init

nuke: ## Stop the stack and DELETE its volumes (database, models, caches)
	@printf "Delete all MHAI volumes? [y/N] " && read ok && [ "$$ok" = y ]
	$(COMPOSE) --profile observability down -v

gateway-test: ## Run the Go gateway tests
	cd gateway && go test -race ./...

gateway-lint: ## Vet and staticcheck the Go gateway
	cd gateway && go vet ./... && go run honnef.co/go/tools/cmd/staticcheck@2025.1 ./...
