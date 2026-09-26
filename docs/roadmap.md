# Nimbus Functions — Roadmap hasta el MVP

Objetivo del MVP: **create → deploy → invoke sync**, con timeout, límites básicos y logs en vivo.

Criterio de done: `docker compose up`, subir un zip, `POST` invoke por HTTP, recibir el JSON y ver logs en vivo — sin tocar Docker a mano.

## Ya

0. Stack documentado (`docs/stack.md`) + runtime Node en Docker (`examples/hello` + `runtime-node` + `scripts/run-hello.sh`)

## Slice 1 — Runner real ✅ HECHO

1. ✅ Script que baje un zip a un dir, monte `/var/task`, corra el container (`scripts/run-zip.sh`)
2. ✅ Timeout hard (`timeout` / kill) — configurable via `NIMBUS_TIMEOUT_SEC`
3. ✅ Límite de memoria básico (`docker --memory`) — configurable via `NIMBUS_MEMORY`
4. ✅ Capturar stdout/stderr del invoke con códigos de salida significativos

**Entregables:**
- `scripts/run-zip.sh` — script principal de invocación local
- `scripts/pack-hello.sh` — empaqueta `examples/hello` en zip
- `scripts/run-hello.sh` — demo de un comando (pack + run)
- `runtime-node/bootstrap.js` mejorado con context estilo Lambda

## Slice 2 — Artifacts ✅ HECHO

5. ✅ MinIO en Compose (`docker-compose.yml` + healthcheck + bucket auto)
6. ✅ Upload zip → object key versionado (`scripts/upload-artifact.sh`)
7. ✅ Runner descarga el artifact antes de ejecutar (`scripts/run-artifact.sh`)

**Entregables:**
- `docker-compose.yml` — MinIO con API `:9000` y consola `:9001`
- `scripts/upload-artifact.sh` — sube zip a `functions/{fnId}/versions/{version}/code.zip`
- `scripts/upload-hello.sh` — empaqueta + sube ejemplo hello
- `scripts/run-artifact.sh` — descarga de MinIO + ejecuta con run-zip.sh
- `scripts/run-hello-from-minio.sh` — demo completa del flujo
- `.env.example` — variables de entorno documentadas

## Slice 3 — Control plane ✅ HECHO

8. ✅ Postgres: tablas `functions`, `versions`, `invocations`
9. ✅ NestJS: `POST /functions`, `POST /functions/:id/deploy` (zip), `POST /functions/:id/invoke`
10. ✅ Nest guarda metadata en Postgres y artifacts en MinIO
11. ✅ Bridge temporal: invoke usa scripts de Slice 1-2 (run-artifact.sh) desde Nest

**Entregables:**
- `control-plane/` — Aplicación NestJS + TypeScript
- `control-plane/prisma/schema.prisma` — Esquema de base de datos
- `docker-compose.yml` — Postgres + MinIO + control-plane
- `scripts/smoke-test.sh` — Test end-to-end del flujo create → deploy → invoke

## Slice 4 — Compute plane ✅ HECHO

11. ✅ Go: `POST /executions` (StartExecution) + `POST /executions/:id/cancel` (Cancel) — HTTP
12. ✅ Nest → Go en cada invoke (reemplaza bridge shell temporal de Slice 3)
13. ✅ Go orquesta Docker + timeout + respuesta (mismo aislamiento que run-zip.sh)
14. ✅ Redis: lock distribuido con TTL para concurrency max 1 por función

**Entregables:**
- `compute-plane/` — Servicio Go con chi router
- `compute-plane/internal/executor/` — Descarga MinIO + Docker run
- `compute-plane/internal/concurrency/` — Redis locks (esquema `nimbus:lock:fn:{id}`)
- `docker-compose.yml` — Redis + compute-plane agregados
- `control-plane/src/functions/runner.service.ts` — Cliente HTTP hacia Go

## Slice 5 — Logs ✅ HECHO

15. ✅ Runner/Go empujan líneas a Redis stream
16. ✅ Nest expone `GET /invocations/:id/logs` (SSE)
17. ✅ `executionId` en todo el camino (invocation → execution → logs)

**Entregables:**
- `compute-plane/internal/logs/` — Paquete Go para Redis streams (write/read)
- `control-plane/src/redis/` — Módulo NestJS para conexión Redis
- `control-plane/src/invocations/` — Módulo con endpoint SSE para logs
- Esquema de keys: `nimbus:logs:{executionId}` (TTL 1h, max 1000 entries)
- Invoke response incluye `invocationId`, `executionId`, y `logsUrl`

## Slice 6 — DX local

18. `docker compose up` (Postgres, Redis, MinIO, Nest, Go)
19. `make hello` = create + deploy + invoke de punta a punta
20. README: cómo reproducir en 3 comandos

## Slice 7 — Consola mínima (opcional para MVP usable)

21. React: crear función, subir zip, invoke, ver logs
22. Si apura: Postman/curl alcanza y el front va post-MVP

## Fuera del MVP

- Auth productizada
- Multi-runtime
- Async / DLQ
- Firecracker / microVM
- Billing
- Grafana / OpenTelemetry completo
- VPC
- Provisioned concurrency
