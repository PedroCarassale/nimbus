# AI Decision Log — Nimbus Functions

Log de auditoría de decisiones técnicas asistidas por IA.
Formato pedido por la cátedra (AI-DECISIONS.md).

---

## Stack MVP y cortes de arquitectura

**Problema abordado:** Definir las piezas de una plataforma tipo Lambda (control plane, compute, artifacts, runtime, logs) y el stack concreto del MVP sin over-engineering.

**Prompt / Herramienta utilizada:** Conversación de diseño con Grok Bot (nimbus) en Cursor; sin generación de código de plataforma en ese momento. Herramienta: asistente de chat.

**Código / Arquitectura generada:** Documento `docs/stack.md` — NestJS + Postgres + Redis + MinIO + Go/Docker + runtime Node fijo + SSE; front React diferible; Firecracker/auth/billing fuera del MVP.

**Validación y Corrección Humana:** Validado por Pedro: se acordó el stack y el orden (runtime antes que API/DB). Product copy en español. No se aceptó scaffold completo automático al inicio; primero guía arquitectónica.

---

## Runtime Node mínimo (hello en Docker)

**Problema abordado:** Tener el primer contrato ejecutable runtime ↔ aislamiento sin control plane: handler de usuario + bootstrap + imagen Docker + script local.

**Prompt / Herramienta utilizada:** Cloud agent de Cursor sobre `PedroCarassale/nimbus` — pedido de `examples/hello`, `runtime-node/bootstrap.js`, Dockerfile y `scripts/run-hello.sh` con timeout.

**Código / Arquitectura generada:** PR #1 — bootstrap carga handler y event JSON, imprime `{ ok, result|error }`; imagen `node:22-alpine`; script `run-hello.sh` con timeout 5s. Prueba reportada: JSON `hola` con event de muestra.

**Validación y Corrección Humana:** Merge a `main` por pedido de Pedro tras revisar el PR. Alcance acotado (sin Nest/Go/MinIO). Pendiente en slices siguientes: zip real, memoria cgroup, y no asumir que este bootstrap es el protocolo final tipo Lambda Runtime API.

---

## Roadmap hasta el MVP

**Problema abordado:** Ordenar el trabajo restante hasta un MVP demostrable (create → deploy → invoke sync).

**Prompt / Herramienta utilizada:** Grok Bot (nimbus) — armado de roadmap en chat y persistencia en `docs/roadmap.md`.

**Código / Arquitectura generada:** `docs/roadmap.md` con slices 1–7 (runner, artifacts, control plane, compute, logs, DX, consola opcional) y exclusiones explícitas.

**Validación y Corrección Humana:** Pedro pidió guardar el roadmap en el repo. Criterio de done acordado: compose + zip + invoke HTTP + logs sin Docker manual. Ajustable si el alcance del MVP cambia con Federico.

---

## Slice 1 — Runner Local con Zip, Timeout y Memoria

**Problema abordado:** Implementar un runner local real que ejecute funciones Node.js desde archivos zip, con soporte para timeout configurable, límites de memoria, y captura de stdout/stderr con códigos de salida significativos.

**Prompt / Herramienta utilizada:** Cursor Cloud Agent (Claude)

**Código / Arquitectura generada:**

1. **Runtime Node.js mejorado (`runtime-node/`)**
   - `Dockerfile`: Imagen Alpine con Node.js 20, configura paths para handler y evento, usa ENTRYPOINT
   - `bootstrap.js`: Entrypoint mejorado que carga el módulo del usuario, ejecuta el handler con event/context (estilo Lambda), y devuelve JSON. Soporta evento opcional.

2. **Ejemplo Hello mejorado (`examples/hello/`)**
   - `index.js`: Handler con saludo personalizable, timestamp y context
   - `event.json`: Evento de ejemplo

3. **Scripts nuevos (`scripts/`)**
   - `run-zip.sh`: Script principal que:
     - Construye la imagen Docker si no existe
     - Extrae el zip a directorio temporal (con cleanup automático via trap)
     - Ejecuta con `docker run --rm --memory --pids-limit --network=none --read-only`
     - Aplica timeout via comando `timeout`
     - Códigos de salida: 0 (éxito), 124 (timeout), 2 (uso), 3 (build), 4 (unzip)
   - `pack-hello.sh`: Empaqueta `examples/hello` en `dist/hello.zip`
   - `run-hello.sh`: Wrapper que empaqueta y ejecuta en un comando (reemplaza versión anterior)

4. **Documentación**
   - `docs/roadmap.md`: Slice 1 marcado como completado
   - `README.md`: Actualizado con instrucciones de uso zip

**Validación y Corrección Humana:** Validado por Pedro (2026-09-21): revisó el checklist del slice 1 (run-hello/run-zip, timeout, memoria, aislamiento network=none + read-only, contrato handler event/context, Node 20, docs/roadmap). Sin correcciones adicionales pedidas en esa validación.

---

## Slice 2 — Artifacts con MinIO (upload versionado + run-artifact)

**Problema abordado:** Dejar de depender solo de paths locales de zip. Almacenar zips de funciones versionados en MinIO (compatible S3) y permitir que el runner descargue el artifact antes de ejecutar, reutilizando el aislamiento Docker del Slice 1.

**Prompt / Herramienta utilizada:** Cursor Cloud Agent (Claude) — pedido explícito de implementar Slice 2 según especificación detallada en el issue/prompt.

**Código / Arquitectura generada:**

1. **Docker Compose (`docker-compose.yml`)**
   - Servicio MinIO con API `:9000` y consola `:9001`
   - Healthcheck integrado (`mc ready local`)
   - Sidecar `minio-init` que crea bucket `nimbus-artifacts` al iniciar
   - Volume persistente `nimbus-minio-data`
   - Credenciales configurables via env (default: `minioadmin`)

2. **Scripts de artifacts (`scripts/`)**
   - `upload-artifact.sh`: Sube zip a key `functions/{fnId}/versions/{version}/code.zip`. Version = timestamp si no se especifica. Soporta AWS CLI y `mc`.
   - `upload-hello.sh`: Wrapper que empaqueta hello y lo sube como función "hello"
   - `run-artifact.sh`: Descarga artifact de MinIO a temp dir, ejecuta con `run-zip.sh`, cleanup automático. Soporta `latest` para buscar última versión.
   - `run-hello-from-minio.sh`: Demo completa que levanta MinIO, sube hello, y lo ejecuta desde MinIO
   - `minio-init.sh`: Script alternativo para inicializar bucket manualmente

3. **Configuración**
   - `.env.example`: Variables de MinIO y runner documentadas
   - Variables: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_ENDPOINT_URL`, `MINIO_BUCKET`

4. **Documentación**
   - `README.md`: Actualizado con instrucciones de MinIO y nuevos scripts
   - `docs/roadmap.md`: Slice 2 marcado como completado

---

## Slice 3 — Control Plane (NestJS + Postgres + APIs)

**Problema abordado:** Implementar el control plane que maneja la metadata de funciones y orquesta invocaciones. El control plane provee APIs REST para crear funciones, desplegar código (zip → MinIO), e invocar funciones. Debe almacenar metadata en Postgres y reutilizar el runner de Slice 1-2 como bridge temporal hasta que exista el compute plane Go (Slice 4).

**Prompt / Herramienta utilizada:** Cursor Cloud Agent (Claude) — pedido de implementar Slice 3 según especificación en issue/prompt con requisitos de: Postgres en compose, tablas functions/versions/invocations, NestJS con endpoints POST /functions, POST /functions/:id/deploy, POST /functions/:id/invoke, bridge temporal que ejecuta scripts existentes.

**Código / Arquitectura generada:**

1. **Docker Compose actualizado (`docker-compose.yml`)**
   - Servicio Postgres 16 Alpine con healthcheck
   - Servicio `control-plane` que depende de Postgres + MinIO
   - Monta `/scripts` read-only y Docker socket para bridge temporal
   - Volumen persistente `nimbus-postgres-data`

2. **Aplicación NestJS (`control-plane/`)**
   - Estructura modular: `functions/`, `prisma/`, `minio/`
   - `PrismaService`: conexión a Postgres con cliente generado
   - `MinioService`: upload/download de artifacts con AWS SDK v3
   - `FunctionsService`: CRUD de funciones, versiones e invocaciones
   - `RunnerService`: bridge temporal que ejecuta `run-artifact.sh` desde Nest (comentado para reemplazo en Slice 4)
   - `FunctionsController`: endpoints REST con validación via class-validator

3. **Schema de base de datos (`prisma/schema.prisma`)**
   - Tabla `functions`: id (UUID), name (unique), timestamps
   - Tabla `versions`: id, function_id (FK), version tag, artifact_key, created_at
   - Tabla `invocations`: id, function_id, version_id, status (enum), request_id, input/output JSON, error, duration_ms
   - Migración SQL inicial con índices para consultas frecuentes

4. **Dockerfile multi-stage**
   - Builder: compila TypeScript + genera cliente Prisma
   - Runtime: Node 20 Alpine + bash + curl + aws-cli + docker-cli (para bridge)
   - Ejecuta migraciones al iniciar

5. **Scripts y documentación**
   - `scripts/smoke-test.sh`: test E2E que crea función → deploys → invoke
   - `README.md`: actualizado con endpoints y ejemplos curl
   - `docs/roadmap.md`: Slice 3 marcado como completado
   - `.env.example`: variables de Postgres agregadas

---

## Slice 4 — Compute Plane (Go + Redis + Docker)

**Problema abordado:** Separar la ejecución de funciones del control plane. El control plane (NestJS) debe delegar la ejecución real al compute plane (Go), que maneja Docker, timeouts, y concurrencia. Esto permite escalar los planos de forma independiente y evita que NestJS dependa de Docker CLI y scripts bash.

**Prompt / Herramienta utilizada:** Cursor Cloud Agent (Claude) — pedido de implementar Slice 4 según especificación: servicio Go con HTTP API, Redis para concurrencia max 1 por función, y reemplazo del bridge temporal de Slice 3.

**Código / Arquitectura generada:**

1. **Servicio Go (`compute-plane/`)**
   - `cmd/server/main.go`: servidor HTTP con chi router, graceful shutdown
   - `internal/handlers/`: endpoints `POST /executions` (StartExecution), `POST /executions/:id/cancel` (Cancel)
   - `internal/executor/`: descarga artifact de MinIO, extrae zip, ejecuta Docker con aislamiento (network=none, read-only, memory limit, timeout)
   - `internal/concurrency/`: lock distribuido con Redis (SetNX + TTL) — max 1 ejecución concurrente por función
   - `internal/minio/`: cliente MinIO para descarga de artifacts
   - `Dockerfile`: multi-stage build, binario estático, runtime Alpine con docker-cli

2. **Esquema de keys Redis**
   - `nimbus:lock:fn:{functionId}` — lock exclusivo por función (SetNX con TTL 5min)
   - `nimbus:exec:{executionId}` — metadata de ejecución activa (HSET)

3. **Integración Nest → Go**
   - `control-plane/src/functions/runner.service.ts`: cliente HTTP hacia compute-plane (reemplaza bridge shell)
   - Simplificación del Dockerfile de control-plane (ya no necesita docker-cli, aws-cli, bash)

4. **Docker Compose actualizado**
   - Servicio `redis` (Redis 7 Alpine con persistencia AOF)
   - Servicio `compute-plane` (depende de redis + minio, monta docker.sock + runtime-node)
   - Control-plane depende de compute-plane healthy

5. **Contrato HTTP Nest ↔ Go**
   - Request: `{executionId, functionId, version, event, timeoutSec, memoryMb, handler}`
   - Response: `{executionId, success, output, error, exitCode, durationMs}`
   - HTTP 429 cuando la función está ocupada (concurrencia max alcanzada)
