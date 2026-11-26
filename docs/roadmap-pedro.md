# Nimbus Functions — Roadmap Pedro (compute plane + infra)

Roadmap del compute plane: ejecución, aislamiento, concurrencia, colas y observabilidad. El control plane y la consola están en [`roadmap-federico.md`](roadmap-federico.md), que también tiene el resumen de lo hecho, el flujo de un invoke y el contrato Nest ↔ Go.

En orden sugerido. Cada paso es un PR chico contra `main`.

### Estado actual
```
compute-plane/
  cmd/server/main.go            # servidor HTTP (chi) en :8080
  internal/handlers/            # POST /executions, POST /executions/:id/cancel, GET /health
  internal/fnconfig/            # timeout, memoria, CPU, handler y envVars por invocación
  internal/executor/            # baja el zip de S3, lo extrae y corre docker
  internal/warm/                # pool de containers reutilizables
  internal/concurrency/         # semáforo + cola FIFO en Redis (N por función, tope global)
  internal/async/               # cola SQS o Redis, reintentos y DLQ
  internal/metrics/             # GET /metrics de Prometheus
  internal/trace/               # traceparent W3C y export OTLP
  internal/logs/                # escribe stdout/stderr en nimbus:logs:{executionId}
  internal/objects/             # cliente S3 (LocalStack o AWS)
```
- Cada función admite N ejecuciones a la vez (`NIMBUS_FN_CONCURRENCY`, default 2). El resto espera en una cola FIFO de Redis, con tope (`NIMBUS_FN_QUEUE_MAX`) y 429 si se llena. Hay un tope global de ejecuciones por proceso (`NIMBUS_MAX_CONTAINERS`). Detalle en [`concurrencia.md`](concurrencia.md).
- Un container ya iniciado se reutiliza para la misma función, versión y config efectiva. Si está frío, idle o vino sucio (timeout, crash, OOM), se arranca uno nuevo. Detalle en [`warm.md`](warm.md).
- El invoke sync sigue igual. Con `?async=true` o `X-Invocation-Type: Event` Nest responde 202 y un worker del compute plane ejecuta el mismo camino (semáforo, cola, config, warm, logs). En el compose la cola es SQS y la DLQ es la de SQS; `NIMBUS_ASYNC_BACKEND=redis` deja el stream. Detalle en [`async.md`](async.md) y [`localstack-aws.md`](localstack-aws.md).
- `GET /metrics` expone invocaciones, duración, cold/warm, cola, 429, reintentos y DLQ. Las trazas salen por OTLP. Grafana y Jaeger están en el perfil `obs`. Detalle en [`observabilidad.md`](observabilidad.md).
- Cancelar una ejecución que ya corre mata el container (frío y warm). El warm no vuelve al pool. Un async cancelado no se reintenta ni va a la DLQ; la fila queda en `ERROR` con `Ejecución cancelada` porque Prisma no tiene `CANCELLED`. Las keys `nimbus:slots:machine:{host}` de un proceso muerto se borran cuando sus leases vencen.
- Hay tres runtimes: `nodejs20` (default), `python3.12` y `go1.x` (binario estático en el zip). `POST /executions` acepta `runtime`. La clave del pool warm lo incluye. Nest todavía no lo persiste ni lo reenvía. Detalle en [`runtimes.md`](runtimes.md).
- El aislamiento por defecto sigue siendo Docker. `NIMBUS_ISOLATION=firecracker` usa microVMs con el mismo contrato (one-shot, warm, timeout, memoria, logs, cancel). Detalle en [`firecracker.md`](firecracker.md).

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

### Paso 6 — Aislamiento fuerte y multi-runtime
- [x] Firecracker / microVM como backend alternativo (`NIMBUS_ISOLATION=firecracker`, default `docker`). Detalle en [`firecracker.md`](firecracker.md).
- [x] Runtimes nuevos: Python (`python3.12`) y Go (`go1.x`, binario estático). Detalle en [`runtimes.md`](runtimes.md).

### Paso 7 — Deploy real (más adelante)
- [x] S3 real / LocalStack. El compose usa `localstack/localstack` (S3 + SQS). Con `S3_ENDPOINT` vacío el mismo cliente habla con Amazon S3. La cola async puede ir a SQS con DLQ nativa. Detalle en [`localstack-aws.md`](localstack-aws.md).
- [ ] Varios workers de compute en distintas máquinas, con scheduler.

## Contrato Nest ↔ Go

Cualquier campo nuevo (config por función, async, etc.) se define primero en `docs/` y es compatible hacia atrás. Detalle en [`roadmap-federico.md`](roadmap-federico.md#6-contrato-nest--go).
