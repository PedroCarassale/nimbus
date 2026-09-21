# Nimbus Functions — Roadmap hasta el MVP

Objetivo del MVP: **create → deploy → invoke sync**, con timeout, límites básicos y logs en vivo.

Criterio de done: `docker compose up`, subir un zip, `POST` invoke por HTTP, recibir el JSON y ver logs en vivo — sin tocar Docker a mano.

## Ya

0. Stack documentado (`docs/stack.md`) + runtime Node en Docker (`examples/hello` + `runtime-node` + `scripts/run-hello.sh`)

## Slice 1 — Runner real

1. Script/binario que baje un zip a un dir, monte `/var/task`, corra el container
2. Timeout hard (`timeout` / kill)
3. Límite de memoria básico (`docker --memory`)
4. Capturar stdout/stderr del invoke

## Slice 2 — Artifacts

5. MinIO en Compose
6. Upload zip → object key versionado
7. Runner descarga el artifact antes de ejecutar

## Slice 3 — Control plane

8. Postgres: tablas `functions`, `versions`, `invocations`
9. NestJS: `POST /functions`, `POST /functions/:id/deploy` (zip), `POST /functions/:id/invoke`
10. Nest guarda metadata; no ejecuta el código del usuario

## Slice 4 — Compute plane

11. Go: `StartExecution` / `Cancel` (HTTP o gRPC corto)
12. Nest → Go en cada invoke
13. Go orquesta Docker + timeout + respuesta
14. Redis: cola simple + concurrency max (1 por función al principio)

## Slice 5 — Logs

15. Runner/Go empujan líneas a Redis stream
16. Nest expone `GET /invocations/:id/logs` (SSE)
17. `requestId` en todo el camino

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
