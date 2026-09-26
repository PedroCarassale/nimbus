# Nimbus Functions — Makefile
# Slice 6: DX local para MVP demo
#
# Targets principales:
#   make up      — Levanta todo el stack (Postgres, Redis, MinIO, control/compute planes)
#   make down    — Detiene y limpia el stack
#   make hello   — Demo completa: crea función hello, despliega, invoca y muestra resultado
#   make smoke   — Ejecuta smoke test completo
#   make logs    — Muestra logs de todos los servicios

.PHONY: up down hello smoke logs build clean wait-healthy help

# Variables
COMPOSE := docker compose
API_URL := http://localhost:3000
COMPUTE_URL := http://localhost:8080

# Colores para output
GREEN := \033[0;32m
YELLOW := \033[1;33m
RED := \033[0;31m
NC := \033[0m

help: ## Muestra esta ayuda
	@echo "Nimbus Functions — Comandos disponibles:"
	@echo ""
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  $(GREEN)%-15s$(NC) %s\n", $$1, $$2}'
	@echo ""
	@echo "Quickstart MVP:"
	@echo "  make up && make hello"

up: ## Levanta todo el stack (Postgres, Redis, MinIO, Nest, Go)
	@echo "$(YELLOW)>>> Levantando stack Nimbus...$(NC)"
	$(COMPOSE) up -d --build
	@echo "$(YELLOW)>>> Esperando que los servicios estén healthy...$(NC)"
	@$(MAKE) wait-healthy
	@echo "$(GREEN)>>> Stack listo! Control plane en $(API_URL)$(NC)"

down: ## Detiene y limpia el stack
	@echo "$(YELLOW)>>> Deteniendo stack...$(NC)"
	$(COMPOSE) down
	@echo "$(GREEN)>>> Stack detenido$(NC)"

down-clean: ## Detiene el stack y elimina volúmenes
	@echo "$(YELLOW)>>> Deteniendo stack y limpiando volúmenes...$(NC)"
	$(COMPOSE) down -v
	@echo "$(GREEN)>>> Stack detenido y volúmenes eliminados$(NC)"

build: ## Construye las imágenes sin levantar
	@echo "$(YELLOW)>>> Construyendo imágenes...$(NC)"
	$(COMPOSE) build

hello: ## Demo completa: create → deploy → invoke función hello
	@echo ""
	@echo "$(GREEN)╔════════════════════════════════════════════════════════════════╗$(NC)"
	@echo "$(GREEN)║     Nimbus Functions — Demo MVP: make hello                    ║$(NC)"
	@echo "$(GREEN)╚════════════════════════════════════════════════════════════════╝$(NC)"
	@echo ""
	@./scripts/hello-demo.sh

smoke: ## Ejecuta smoke test completo (create, deploy, invoke, logs)
	@echo "$(YELLOW)>>> Ejecutando smoke test...$(NC)"
	@./scripts/smoke-test.sh

logs: ## Muestra logs de todos los servicios (follow)
	$(COMPOSE) logs -f

logs-control: ## Muestra logs del control plane
	$(COMPOSE) logs -f control-plane

logs-compute: ## Muestra logs del compute plane
	$(COMPOSE) logs -f compute-plane

ps: ## Muestra estado de los servicios
	$(COMPOSE) ps

wait-healthy: ## Espera a que todos los servicios estén healthy
	@echo "Esperando control-plane..."
	@for i in $$(seq 1 60); do \
		if curl -sf $(API_URL)/health >/dev/null 2>&1; then \
			echo "$(GREEN)  ✓ Control plane listo$(NC)"; \
			break; \
		fi; \
		if [ $$i -eq 60 ]; then \
			echo "$(RED)  ✗ Timeout esperando control-plane$(NC)"; \
			exit 1; \
		fi; \
		sleep 1; \
	done
	@echo "Esperando compute-plane..."
	@for i in $$(seq 1 60); do \
		if curl -sf $(COMPUTE_URL)/health >/dev/null 2>&1; then \
			echo "$(GREEN)  ✓ Compute plane listo$(NC)"; \
			break; \
		fi; \
		if [ $$i -eq 60 ]; then \
			echo "$(RED)  ✗ Timeout esperando compute-plane$(NC)"; \
			exit 1; \
		fi; \
		sleep 1; \
	done

clean: ## Limpia artifacts generados (dist/)
	@echo "$(YELLOW)>>> Limpiando artifacts...$(NC)"
	rm -rf dist/
	@echo "$(GREEN)>>> Limpieza completada$(NC)"

restart: down up ## Reinicia todo el stack

# Targets de desarrollo
dev-control: ## Logs en vivo del control plane
	$(COMPOSE) logs -f control-plane

dev-compute: ## Logs en vivo del compute plane
	$(COMPOSE) logs -f compute-plane

.DEFAULT_GOAL := help
