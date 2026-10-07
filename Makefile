.PHONY: help install install-dev lint format typecheck test cov run clean docker

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
	uv run ruff check mhai tests

format: ## Auto-format
	uv run ruff format mhai tests
	uv run ruff check --fix mhai tests

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

docker: ## Build container image
	docker build -t mhai:latest .
