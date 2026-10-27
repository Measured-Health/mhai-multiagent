.PHONY: help install install-dev lint format typecheck test cov run clean docker \
	up up-gpu up-observability infra down ps logs models nuke core-test core-lint

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

TORCH ?= cpu  # PyTorch build: cpu or cu128

# The Python service is a uv project in its own directory (ADR 0002).
PY := cd services/intelligence &&

install: ## Install runtime package (TORCH=cpu|cu128)
	$(PY) uv sync --locked --no-default-groups --group $(TORCH)

install-dev: ## Install with dev tools + pre-commit (TORCH=cpu|cu128)
	$(PY) uv sync --locked --no-default-groups --group dev --group $(TORCH)
	$(PY) uv run pre-commit install

lint: ## Run ruff linter
	$(PY) uv run ruff check .

format: ## Auto-format
	$(PY) uv run ruff format .
	$(PY) uv run ruff check --fix .

typecheck: ## Run mypy
	$(PY) uv run mypy

test: ## Run test suite
	$(PY) uv run pytest

cov: ## Tests with coverage
	$(PY) uv run pytest --cov=intelligence --cov-report=term-missing

run: ## Run a quick smoke test
	$(PY) uv run intelligence run --query "Test query" --urgency routine

clean: ## Remove build artifacts
	$(PY) rm -rf build dist *.egg-info .pytest_cache .mypy_cache .ruff_cache htmlcov .coverage
	find . -type d -name __pycache__ -exec rm -rf {} +

docker: ## Build container image (TORCH=cpu|cu128)
	docker build --build-arg TORCH=$(TORCH) -t mhai-intelligence:$(TORCH) services/intelligence

COMPOSE ?= docker compose
GPU := -f docker-compose.yml -f docker-compose.gpu.yml

.env:
	cp .env.example .env
	@echo "Created .env from .env.example; set POSTGRES_PASSWORD before sharing the stack."

up: .env ## Start the stack: postgres, redis, ollama (+ model pull), intelligence
	$(COMPOSE) up -d --build

up-gpu: .env ## Start the stack on an NVIDIA GPU
	$(COMPOSE) $(GPU) up -d --build

up-observability: .env ## Start the stack plus the OpenTelemetry collector
	$(COMPOSE) --profile observability up -d --build

infra: .env ## Start only postgres (+ core migrations), redis and ollama (run the app with uv)
	$(COMPOSE) up -d --build postgres core-migrate redis ollama ollama-init

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

core-test: ## Run the core service (Go) tests
	cd services/core && go test -race ./...

core-lint: ## Vet and staticcheck the core service (Go)
	cd services/core && go vet ./... && go run honnef.co/go/tools/cmd/staticcheck@2025.1 ./...
