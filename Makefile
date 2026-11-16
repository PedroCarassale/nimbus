# Nimbus Functions — Makefile
# Slice 6: DX local para MVP demo
# Slice 7: Consola web React
#
# Targets principales:
#   make up      — Levanta todo el stack (Postgres, Redis, MinIO, control/compute planes, console)
#   make down    — Detiene y limpia el stack
#   make hello   — Demo completa: crea función hello, despliega, invoca y muestra resultado
#   make smoke   — Ejecuta smoke test completo
#   make logs    — Muestra logs de todos los servicios
#   make console — Abre la consola web en el navegador

.PHONY: up down hello smoke load config-check warm-check logs build clean wait-healthy help

# Variables
COMPOSE := docker compose
API_URL := http://localhost:3000
COMPUTE_URL := http://localhost:8080
CONSOLE_URL := http://localhost:8088
WORK_DIR := /tmp/nimbus-work

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
	@mkdir -p $(WORK_DIR)
	@chmod 1777 $(WORK_DIR)
	@echo "$(YELLOW)>>> Construyendo imagen de runtime nimbus-node...$(NC)"
	docker build -t nimbus-node ./runtime-node
	$(COMPOSE) up -d --build
	@echo "$(YELLOW)>>> Esperando que los servicios estén healthy...$(NC)"
	@$(MAKE) wait-healthy
	@echo "$(GREEN)>>> Stack listo! Control plane en $(API_URL)$(NC)"
	@echo "$(GREEN)>>> Consola en $(CONSOLE_URL)$(NC)"

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

smoke: ## Ejecuta smoke test (create, deploy, invoke, logs, error, timeout)
	@echo "$(YELLOW)>>> Ejecutando smoke test...$(NC)"
	@./scripts/smoke-test.sh

load: ## 6 invokes en paralelo: 2 corren, 3 esperan, 1 recibe 429
	@echo "$(YELLOW)>>> Prueba de concurrencia...$(NC)"
	@./scripts/load-concurrency.sh

config-check: ## Prueba timeout, memoria, env, handler y cpus contra el compute plane
	@echo "$(YELLOW)>>> Prueba de configuración por función...$(NC)"
	@./scripts/config-check.sh

warm-check: ## Mide cold start vs warm start y el descarte por idle, timeout y OOM
	@echo "$(YELLOW)>>> Prueba de containers warm...$(NC)"
	@./scripts/warm-check.sh

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
	@for i in $$(seq 1 180); do \
		if curl -sf $(API_URL)/health >/dev/null 2>&1; then \
			echo "$(GREEN)  ✓ Control plane listo$(NC)"; \
			break; \
		fi; \
		if [ $$i -eq 180 ]; then \
			echo "$(RED)  ✗ Timeout esperando control-plane$(NC)"; \
			$(COMPOSE) logs --no-color control-plane compute-plane minio || true; \
			exit 1; \
		fi; \
		sleep 1; \
	done
	@echo "Esperando compute-plane..."
	@for i in $$(seq 1 120); do \
		if curl -sf $(COMPUTE_URL)/health >/dev/null 2>&1; then \
			echo "$(GREEN)  ✓ Compute plane listo$(NC)"; \
			break; \
		fi; \
		if [ $$i -eq 120 ]; then \
			echo "$(RED)  ✗ Timeout esperando compute-plane$(NC)"; \
			$(COMPOSE) logs --no-color compute-plane || true; \
			exit 1; \
		fi; \
		sleep 1; \
	done
	@echo "Esperando consola..."
	@for i in $$(seq 1 90); do \
		if curl -sf $(CONSOLE_URL)/ >/dev/null 2>&1; then \
			echo "$(GREEN)  ✓ Consola lista$(NC)"; \
			break; \
		fi; \
		if [ $$i -eq 90 ]; then \
			echo "$(RED)  ✗ Timeout esperando consola$(NC)"; \
			$(COMPOSE) logs --no-color console || true; \
			exit 1; \
		fi; \
		sleep 1; \
	done

clean: ## Limpia artifacts generados (dist/)
	@echo "$(YELLOW)>>> Limpiando artifacts...$(NC)"
	rm -rf dist/
	@echo "$(GREEN)>>> Limpieza completada$(NC)"

restart: down up ## Reinicia todo el stack

console: ## Abre la consola web (requiere stack up)
	@echo "$(GREEN)>>> Consola Nimbus disponible en: http://localhost:8088$(NC)"
	@echo ">>> Alternativa dev: cd console && npm run dev (http://localhost:5173)"
	@if command -v xdg-open >/dev/null 2>&1; then \
		xdg-open http://localhost:8088 2>/dev/null || true; \
	elif command -v open >/dev/null 2>&1; then \
		open http://localhost:8088 2>/dev/null || true; \
	fi

console-dev: ## Inicia consola en modo desarrollo (hot reload)
	@echo "$(YELLOW)>>> Iniciando consola en modo desarrollo...$(NC)"
	@echo ">>> Asegúrate de que el stack esté corriendo (make up)"
	cd console && npm run dev

# Targets de desarrollo
dev-control: ## Logs en vivo del control plane
	$(COMPOSE) logs -f control-plane

dev-compute: ## Logs en vivo del compute plane
	$(COMPOSE) logs -f compute-plane

.DEFAULT_GOAL := help
