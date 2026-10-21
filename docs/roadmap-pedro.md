# Nimbus Functions — Roadmap Pedro (compute plane + infra)

Roadmap del compute plane: ejecución, aislamiento, concurrencia, colas y observabilidad. El control plane y la consola están en [`roadmap-federico.md`](roadmap-federico.md), que también tiene el resumen de lo hecho, el flujo de un invoke y el contrato Nest ↔ Go.

En orden sugerido. Cada paso es un PR chico contra `main`.

### Estado actual
```
compute-plane/
  cmd/server/main.go            # servidor HTTP (chi) en :8080
  internal/handlers/            # POST /executions, POST /executions/:id/cancel, GET /health
  internal/executor/            # baja el zip de MinIO, lo extrae y corre docker run
  internal/concurrency/         # lock en Redis: nimbus:lock:fn:{functionId} (máx. 1 a la vez)
  internal/logs/                # escribe stdout/stderr en nimbus:logs:{executionId}
  internal/minio/               # cliente S3
```
- Si una función ya se está ejecutando, la segunda invocación no espera en una cola.
- Cada invoke crea un container nuevo (cold start siempre).
- Todas las invocaciones son sincrónicas.

### Paso 0 — Base estable
- [ ] Correr `make up` + `make hello` con Docker real y arreglar lo que rompa.
- [ ] Tests del compute plane (Go) + CI en GitHub Actions.

### Paso 1 — Concurrencia real y cola
- [ ] Permitir N ejecuciones simultáneas por función (configurable).
- [ ] Cola en Redis: si se llega al máximo, la invocación espera turno.
- [ ] Límite de cola y rechazo claro (`429`) cuando se llena.
- [ ] Límite global de containers por máquina.

### Paso 2 — Configuración por función (lado Go)
- [ ] Recibir y aplicar `timeoutSec`, `memoryMb`, `envVars` y `handler` desde el contrato.
- [ ] Límite de CPU (`--cpus`).

### Paso 3 — Warm containers
- [ ] Reusar containers ya iniciados para la misma función y versión.
- [ ] Pool con tiempo de vida y limpieza de containers inactivos.
- [ ] Medir cold start vs warm start.

### Paso 4 — Invocación async + reintentos + DLQ
- [ ] Invocación async: responde enseguida con un id y ejecuta en segundo plano.
- [ ] Reintentos con backoff cuando falla.
- [ ] DLQ: las invocaciones que fallan todos los reintentos quedan guardadas para revisar.

### Paso 5 — Observabilidad
- [ ] Métricas en Prometheus: invocaciones, errores, duración, cold starts, tamaño de la cola.
- [ ] Dashboards en Grafana.
- [ ] Trazas con OpenTelemetry de punta a punta (Nest → Go → container).

### Paso 6 — Aislamiento fuerte y multi-runtime (más adelante)
- [ ] Firecracker / microVM en lugar de Docker.
- [ ] Runtimes nuevos: Python, Go.

### Paso 7 — Deploy real (más adelante)
- [ ] S3 real en lugar de MinIO.
- [ ] Varios workers de compute en distintas máquinas, con scheduler.

## Contrato Nest ↔ Go

Cualquier campo nuevo (config por función, async, etc.) se define primero en `docs/` y es compatible hacia atrás. Detalle en [`roadmap-federico.md`](roadmap-federico.md#6-contrato-nest--go).
