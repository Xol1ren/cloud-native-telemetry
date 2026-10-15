.PHONY: build test lint docker-up docker-down clean

AGENT_DIR := collector-agent
CP_DIR := control-plane
VENV := $(CP_DIR)/.venv
COMPOSE := docker compose -f deployments/docker-compose.yml

build: ## Build Go agent binary and Docker images
	cd $(AGENT_DIR) && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/collector-agent .
	docker build -t cloudpulse-collector $(AGENT_DIR)
	docker build -t cloudpulse-control-plane $(CP_DIR)

$(VENV)/bin/pytest: $(CP_DIR)/requirements-dev.txt
	python3 -m venv $(VENV)
	$(VENV)/bin/pip install -q -r $(CP_DIR)/requirements-dev.txt

test: $(VENV)/bin/pytest ## Run Go and Python test suites
	cd $(AGENT_DIR) && go vet ./... && go test -race ./...
	cd $(CP_DIR) && .venv/bin/python -m pytest -q

lint: $(VENV)/bin/pytest
	cd $(AGENT_DIR) && test -z "$$(gofmt -l .)"
	cd $(CP_DIR) && .venv/bin/ruff check . && .venv/bin/ruff format --check .

docker-up:
	$(COMPOSE) up --build -d

docker-down:
	$(COMPOSE) down -v

clean:
	rm -rf $(AGENT_DIR)/bin $(VENV) $(CP_DIR)/.pytest_cache $(CP_DIR)/.ruff_cache
	find . -name __pycache__ -type d -prune -exec rm -rf {} +

