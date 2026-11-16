# Nimbus Functions — Roadmap Pedro (compute plane + infra)

Roadmap del compute plane: ejecución, aislamiento, concurrencia, colas y observabilidad. El control plane y la consola están en [`roadmap-federico.md`](roadmap-federico.md), que también tiene el resumen de lo hecho, el flujo de un invoke y el contrato Nest ↔ Go.

En orden sugerido. Cada paso es un PR chico contra `main`.

### Estado actual
```
compute-plane/
  cmd/server/main.go            # servidor HTTP (chi) en :8080
  internal/handlers/            # POST /executions, POST /executions/:id/cancel, GET /health
  internal/fnconfig/            # timeout, memoria, CPU, handler y envVars por invocación
  internal/executor/            # baja el zip de MinIO, lo extrae y corre docker
  internal/warm/                # pool de containers reutilizables
  internal/concurrency/         # semáforo + cola FIFO en Redis (N por función, tope global)
  internal/async/               # cola durable, reintentos y DLQ
  internal/metrics/             # GET /metrics de Prometheus
  internal/trace/               # traceparent W3C y export OTLP
  internal/logs/                # escribe stdout/stderr en nimbus:logs:{executionId}
  internal/minio/               # cliente S3
```
- Cada función admite N ejecuciones a la vez (`NIMBUS_FN_CONCURRENCY`, default 2). El resto espera en una cola FIFO de Redis, con tope (`NIMBUS_FN_QUEUE_MAX`) y 429 si se llena. Hay un tope global de ejecuciones por proceso (`NIMBUS_MAX_CONTAINERS`). Detalle en [`concurrencia.md`](concurrencia.md).
- Un container ya iniciado se reutiliza para la misma función, versión y config efectiva. Si está frío, idle o vino sucio (timeout, crash, OOM), se arranca uno nuevo. Detalle en [`warm.md`](warm.md).
- El invoke sync sigue igual. Con `?async=true` o `X-Invocation-Type: Event` Nest responde 202 y un worker del compute plane ejecuta el mismo camino (semáforo, cola, config, warm, logs). Reintentos con backoff y DLQ en Redis. Detalle en [`async.md`](async.md).
- `GET /metrics` expone invocaciones, duración, cold/warm, cola, 429, reintentos y DLQ. Las trazas salen por OTLP. Grafana y Jaeger están en el perfil `obs`. Detalle en [`observabilidad.md`](observabilidad.md).

### Paso 0 — Base estable
- [x] Correr `make up` + `make hello` con Docker real y arreglar lo que rompa.
- [x] Tests del compute plane (Go) + CI en GitHub Actions.

### Paso 1 — Concurrencia real y cola
- [x] Permitir N ejecuciones simultáneas por función (configurable).
- [x] Cola en Redis: si se llega al máximo, la invocación espera turno.
- [x] Límite de cola y rechazo claro (`429`) cuando se llena.
- [x] Límite global de containers por máquina.

### Paso 2 — Configuración por función (lado Go)
- [x] Recibir y aplicar `timeoutSec`, `memoryMb`, `envVars` y `handler` desde el contrato.
- [x] Límite de CPU (`--cpus`).

Detalle de campos, defaults y límites: [`configuracion.md`](configuracion.md).

### Paso 3 — Warm containers
- [x] Reusar containers ya iniciados para la misma función, versión y config efectiva.
- [x] Pool con tiempo de vida y limpieza de containers inactivos.
- [x] Medir cold start vs warm start.

Detalle del protocolo, las variables y los números: [`warm.md`](warm.md).

### Paso 4 — Invocación async + reintentos + DLQ
- [x] Invocación async: responde enseguida con un id y ejecuta en segundo plano.
- [x] Reintentos con backoff cuando falla.
- [x] DLQ: las invocaciones que fallan todos los reintentos quedan guardadas para revisar.

Detalle de la cola, las variables y la operación de la DLQ: [`async.md`](async.md).

### Paso 5 — Observabilidad
- [x] Métricas en Prometheus: invocaciones, errores, duración, cold starts, tamaño de la cola.
- [x] Dashboards en Grafana.
- [x] Trazas con OpenTelemetry de punta a punta (Nest → Go → container).

Detalle de métricas, puertos y variables: [`observabilidad.md`](observabilidad.md).

### Paso 6 — Aislamiento fuerte y multi-runtime (más adelante)
- [ ] Firecracker / microVM en lugar de Docker.
- [ ] Runtimes nuevos: Python, Go.

### Paso 7 — Deploy real (más adelante)
- [ ] S3 real en lugar de MinIO.
- [ ] Varios workers de compute en distintas máquinas, con scheduler.

## Contrato Nest ↔ Go

Cualquier campo nuevo (config por función, async, etc.) se define primero en `docs/` y es compatible hacia atrás. Detalle en [`roadmap-federico.md`](roadmap-federico.md#6-contrato-nest--go).
