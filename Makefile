.PHONY: help install install-dev lint format typecheck test cov run clean docker

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

install: ## Install runtime package
	pip install -e .

install-dev: ## Install with dev extras + pre-commit
	pip install -e ".[dev]"
	pre-commit install

lint: ## Run ruff linter
	ruff check src tests

format: ## Auto-format
	ruff format src tests
	ruff check --fix src tests

typecheck: ## Run mypy
	mypy

test: ## Run test suite
	pytest

cov: ## Tests with coverage
	pytest --cov=mhai --cov-report=term-missing

run: ## Run a quick smoke test
	mhai run --query "Test query" --urgency routine

clean: ## Remove build artifacts
	rm -rf build dist *.egg-info .pytest_cache .mypy_cache .ruff_cache htmlcov .coverage
	find . -type d -name __pycache__ -exec rm -rf {} +

docker: ## Build container image
	docker build -t mhai:latest .
