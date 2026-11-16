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

---

## Slice 5 — Live Logs (SSE via Redis Streams)

**Problema abordado:** Permitir a los usuarios ver logs en tiempo real de funciones en ejecución. El compute plane (Go) debe transmitir stdout/stderr a un Redis stream mientras ejecuta Docker, y el control plane (NestJS) debe exponer un endpoint SSE que los clientes puedan consumir para ver logs en vivo.

**Prompt / Herramienta utilizada:** Cursor Cloud Agent (Claude) — pedido de implementar Slice 5 según especificación: Go pushes log lines to Redis stream, Nest exposes SSE endpoint, executionId correlaciona todo el flujo.

**Código / Arquitectura generada:**

1. **Paquete de logs Go (`compute-plane/internal/logs/`)**
   - `stream.go`: StreamWriter para escribir logs a Redis, StreamReader para leer
   - Usa Redis XADD para escritura incremental, XREAD BLOCK para suscripción
   - Cada línea de stdout/stderr se serializa como JSON con timestamp, stream, sequence

2. **Executor modificado (`compute-plane/internal/executor/`)**
   - `runDockerWithStreaming`: reemplaza buffer completo por pipes + goroutines
   - Lee stdout/stderr línea por línea con bufio.Scanner
   - Escribe cada línea al StreamWriter en tiempo real
   - Marca fin de ejecución con entrada `_end` en el stream

3. **Módulo Redis NestJS (`control-plane/src/redis/`)**
   - `RedisService`: conexión ioredis, métodos para leer streams y suscribirse
   - AsyncGenerator para suscripción a logs con XREAD BLOCK

4. **Módulo Invocations (`control-plane/src/invocations/`)**
   - `GET /invocations/:id/logs`: endpoint SSE con Server-Sent Events
   - Soporta `follow=true` (streaming en vivo) y `follow=false` (snapshot)
   - Eventos: `connected`, `log`, `end`, `error`

5. **Esquema de keys Redis**
   - `nimbus:logs:{executionId}` — stream de logs por ejecución
   - TTL: 1 hora después de completar
   - MaxLen: 1000 entries (aproximado)

6. **Flujo de IDs**
   - `invocationId`: ID en Postgres, retornado por invoke
   - `executionId`: UUID generado por Nest, enviado a Go, usado como key de stream
   - Invocation almacena `executionId` para correlación

---

## Slice 6 — DX Local / make hello

**Problema abordado:** Simplificar la experiencia de desarrollo local para que cualquier desarrollador pueda probar el MVP completo de Nimbus con comandos mínimos, sin necesidad de ejecutar pasos manuales de configuración, migraciones, o creación de buckets.

**Prompt / Herramienta utilizada:** Cursor Cloud Agent (Claude) — pedido de implementar Slice 6 según especificación: Makefile con targets up/down/hello, asegurar compose one-shot reliable, README con 3 comandos.

**Código / Arquitectura generada:**

1. **Makefile con targets principales**
   - `make up`: levanta stack con `docker compose up -d --build` + espera healthchecks
   - `make down`: detiene stack, `make down-clean`: elimina volúmenes
   - `make hello`: ejecuta demo completa (create → deploy → invoke → muestra resultado)
   - `make smoke`: ejecuta smoke-test.sh existente
   - `make logs`: muestra logs en follow mode
   - `make help`: documentación inline de todos los targets

2. **Script hello-demo.sh**
   - Verifica que el stack esté corriendo
   - Crea función "hello" (o reutiliza existente)
   - Empaqueta examples/hello en zip
   - Despliega via API
   - Invoca y muestra resultado formateado
   - Incluye hints de logs SSE y comandos para invocar de nuevo

3. **Healthcheck de MinIO corregido**
   - Cambio de `mc ready local` a `curl -f http://localhost:9000/minio/health/live`
   - `mc` no está disponible en la imagen base de MinIO, curl sí

4. **README.md actualizado**
   - Nueva sección "MVP Demo — 3 Comandos" al inicio
   - Diagrama ASCII de arquitectura
   - Comandos útiles y ejemplo de logs SSE
   - Mantiene documentación detallada existente

---

## Slice 7 — Consola React Mínima

**Problema abordado:** Proveer una interfaz web para que usuarios puedan interactuar con Nimbus sin usar curl. La consola debe permitir crear funciones, subir código (zip), invocar funciones con un editor JSON de eventos, y ver logs en tiempo real via SSE.

**Prompt / Herramienta utilizada:** Cursor Cloud Agent (Claude) — pedido de implementar consola React mínima con Vite, integrada con las APIs existentes del control-plane, UI en español, sin librerías de UI externas.

**Código / Arquitectura generada:**

1. **Aplicación React (`console/`)**
   - Vite + React 19 + TypeScript como base
   - `src/api.ts`: Cliente HTTP para todas las APIs del control-plane
   - `src/types.ts`: Tipos TypeScript para funciones, versiones, invocaciones, logs
   - `src/pages/FunctionsList.tsx`: Listar y crear funciones
   - `src/pages/FunctionDetail.tsx`: Deploy (upload zip), invoke (editor JSON), ver resultado
   - `src/components/LogsPanel.tsx`: Panel de logs SSE en tiempo real
   - `src/components/JsonEditor.tsx`: Editor JSON con validación
   - CSS puro con variables CSS, tema oscuro, responsive

2. **Integración con Docker Compose**
   - `Dockerfile`: Build multi-stage (Node para build, nginx para runtime)
   - `nginx.conf`: Proxy `/api/` a control-plane, SPA fallback, cache de assets
   - Servicio `console` en docker-compose.yml escuchando en puerto 8088

3. **DX local**
   - Proxy de Vite configurable para desarrollo (`npm run dev` en puerto 5173)
   - `make console`: Abre la consola en el navegador
   - `make console-dev`: Inicia Vite en modo desarrollo con hot reload

4. **Documentación**
   - `console/README.md`: Instrucciones de desarrollo y build
   - `README.md`: Actualizado con consola en arquitectura y comandos
   - `docs/roadmap.md`: Slice 7 marcado como completado

---

## Roadmaps post-MVP por persona (control plane / compute plane)

**Problema abordado:** Con el MVP terminado, hacía falta dividir el trabajo restante entre Pedro y Federico para avanzar en paralelo, y darle a Federico un resumen de lo construido, ya que no había visto el código.

**Prompt / Herramienta utilizada:** Grok Bot (asistente de Pedro), a pedido de Pedro: armar un roadmap por persona con el resumen del MVP y subirlo al repo.

**Código / Arquitectura generada:**

1. `docs/roadmap-federico.md`: qué es Nimbus, qué está hecho (slices 0–7), flujo de un invoke, estructura y API actual del control plane, observaciones del código actual (nombre único global, sin auth, timeout/memoria fijos, estado `RUNNING` tardío), roadmap del control plane + consola en pasos (auth, config por función, versiones/alias, historial, URL pública, cron, pulido), contrato Nest ↔ Go y reglas del repo.
2. `docs/roadmap-pedro.md`: estado actual de `compute-plane/` y roadmap del compute plane + infra (concurrencia y cola, config del lado Go, warm containers, async + reintentos + DLQ, observabilidad, Firecracker/multi-runtime, deploy real).
3. `docs/roadmap.md`: sección "Después del MVP" con links a los dos roadmaps.

División: el compute plane es la parte más compleja (procesos, aislamiento, concurrencia); el control plane concentra el producto que ve el usuario. El punto de integración es el contrato `POST /executions`, que se cambia de forma compatible hacia atrás y documentada en `docs/`.

---

## Paso 0 — Base estable del compute plane

**Problema abordado:** El stack del MVP (`make up`, `make hello`, logs SSE) no se había ejecutado de punta a punta con Docker. El compute plane no podía ver el código del usuario desde el daemon, no interpretaba el JSON multilínea del runtime, el timeout no devolvía el exit code 124 del contrato, el healthcheck hacía HEAD contra una ruta que solo acepta GET, y las imágenes oficiales de MinIO ya no se pueden bajar.

**Prompt / Herramienta utilizada:** Cursor Cloud Agent (Grok 4.7), sobre el pedido de Paso 0 del roadmap de Pedro: Docker real, `make up` / `make hello` / `make smoke`, logs SSE, casos de timeout y error, tests Go y GitHub Actions.

**Código / Arquitectura generada:**

1. **Ejecución (compute plane)**
   - `NIMBUS_WORK_DIR=/tmp/nimbus-work` se monta con el mismo path en el contenedor y en el host, porque `docker run -v` lo resuelve el daemon.
   - El resultado del handler se parsea aunque el bootstrap lo imprima en varias líneas.
   - El timeout lo aplica el contexto de Go, mata el container con `docker rm -f` y responde exit code 124.
   - El cliente de artifacts crea el bucket si todavía no existe y guarda el zip bajo el directorio compartido.
   - CLI de Docker 27 (Alpine 3.21) para hablar con Engine 29.
   - Tests con miniredis de locks, streams de logs, handlers HTTP y helpers del executor.

2. **Infra**
   - Compose usa Silo (`pgsty/silo`) y `pgsty/mc` en lugar de `minio/minio` y `minio/mc`, con digest fijo.
   - `make up` construye `nimbus-node`, crea el directorio de trabajo y espera también a la consola.
   - `make smoke` exige logs SSE (`follow=false` y `curl -N`), un handler que lanza error y un timeout.
   - GitHub Actions: build/vet/test de Go, build de Nest, build de la consola y job de compose smoke.

3. **Ajustes mínimos fuera del compute plane, necesarios para el flujo**
   - Control plane: OpenSSL 3 y `binaryTargets` de Prisma para que `migrate` arranque en Alpine; el `XREAD` de ioredis se normaliza porque `call()` devuelve un solo stream y el SSE en vivo no cerraba.
   - El contrato `POST /executions` no cambia.

**Validación y Corrección Humana:** Riesgos que quedan en el diseño: el compute plane monta el socket de Docker del host; `/tmp/nimbus-work` contiene el código de las funciones en el host; el timeout cuenta desde que arranca `docker run`, incluido el cold start del container.

---

## Paso 1 — Concurrencia real y cola

**Problema abordado:** El compute plane tomaba un lock Redis `nimbus:lock:fn:{functionId}` y la segunda invocación de la misma función no esperaba: competía unos segundos y, si el lock seguía tomado, respondía 429. Hacía falta N ejecuciones simultáneas, una cola con límite, un tope de containers por máquina, y que cancelar también saque de la cola a quien todavía no arrancó.

**Prompt / Herramienta utilizada:** Cursor Cloud Agent (Grok 4.7), pedido de Paso 1 del roadmap de Pedro, apilado sobre la rama de Paso 0.

**Código / Arquitectura generada:**

1. **Admisión en Redis (compute plane)**
   - El lock `SET NX` se reemplaza por un semáforo: sorted set `nimbus:slots:fn:{functionId}` con lease (score = vencimiento). El proceso renueva el lease hasta `Release`. Si el proceso muere, la próxima admisión borra los scores vencidos.
   - La cola es otro sorted set, `nimbus:queue:fn:{functionId}`, ordenado por un ticket `INCR` (FIFO). Solo la cabeza toma un slot cuando hay lugar. Quien espera refresca un `heartbeat`; si deja de hacerlo durante la espera máxima, se lo saca de la cola.
   - El tope global es `nimbus:slots:machine:{hostname}` contra `NIMBUS_MAX_CONTAINERS`. Cuenta ejecuciones con slot, antes de `docker run`.
   - La decisión (entrar, encolar o rechazar) es un script Lua.
   - Env: `NIMBUS_FN_CONCURRENCY` (default 2), `NIMBUS_FN_QUEUE_MAX` (32), `NIMBUS_FN_QUEUE_WAIT_SEC` (30), `NIMBUS_MAX_CONTAINERS` (8), `NIMBUS_SLOT_LEASE_SEC` (30).
   - `POST /executions` acepta `maxConcurrency` opcional. La respuesta suma `queuedMs`.
   - Cola llena o espera vencida: HTTP 429 con `reason` `queue_full` o `queue_timeout`.
   - `POST /executions/:id/cancel` saca al queued de la cola (el request bloqueado responde 409). Una ejecución que ya corre queda marcada y conserva el slot hasta el release.

2. **Control plane, cambio chico**
   - `runner.service.ts` propaga el 429.
   - `functions.controller.ts` lo responde como `429 Too Many Requests`, y guarda la invocación en `ERROR`.

3. **Prueba**
   - Tests con miniredis (N en paralelo, FIFO, cola llena, timeout de espera, lease vencido, cabeza de cola muerta, tope global, cancel de queued) y `go test -race` en CI.
   - `make load` dispara 6 invokes de `examples/slow` con concurrencia 2 y cola 3.

El contrato queda en `docs/concurrencia.md`.

**Validación y Corrección Humana:** Riesgos que quedan en el diseño: el tope global es por proceso (hostname), no por un conteo de `docker ps`; un container huérfano puede seguir vivo después de que el lease venció. Cancelar una ejecución que ya corre no mata el container, solo deja el flag. `maxConcurrency` en el request aplica a esa admisión: dos requests en vuelo con valores distintos pueden llegar al mayor de los dos, siempre bajo `NIMBUS_MAX_CONTAINERS`. El slot vencido se recupera en la próxima admisión, con una ventana de `NIMBUS_SLOT_LEASE_SEC`.

---

## Paso 2 — Configuración por función en el compute plane

**Problema abordado:** `POST /executions` ya recibía `timeoutSec`, `memoryMb` y `handler`, pero el handler era solo el nombre del export sobre `index.js` (`index.handler` no resolvía un archivo), no había variables de entorno ni CPU, y un valor absurdo (timeout 0 o memoria enorme) se aceptaba o se reemplazaba en silencio. La config por función del control plane la sigue guardando Federico; Go tiene que aplicar el contrato cuando el body la trae.

**Prompt / Herramienta utilizada:** Cursor Cloud Agent (Grok 4.7), pedido de Paso 2 del roadmap de Pedro, apilado sobre la rama de Paso 1.

**Código / Arquitectura generada:**

1. **Contrato en el compute plane**
   - `internal/fnconfig` valida cada request. Campos opcionales: `timeoutSec` (1..300, default `NIMBUS_TIMEOUT_SEC` o 5), `memoryMb` (64..2048, default `NIMBUS_MEMORY_MB` o 128), `cpus` (0.1..4, default `NIMBUS_CPUS`; vacío = sin `--cpus`), `handler` y `envVars`.
   - `0` o ausente usa el default, igual que antes. Cualquier otro valor fuera de rango responde 400 con `error` y `field`, antes de tomar un slot.
   - `envVars`: hasta 32 variables, nombre `[A-Za-z_][A-Za-z0-9_]*`, valor hasta 4096 bytes, total 16 KiB, sin saltos de línea. Un nombre `NIMBUS_*` se rechaza. Los valores no se escriben en el log del proceso: el `docker run` queda como `CLAVE=***`.
   - El handler es `archivo.export`. `handler` sigue siendo el export `handler` de `index.js`. `index.handler` y `src/app.main` eligen archivo y export. El bootstrap deja de fijar el archivo en `index.js` cuando viene `NIMBUS_HANDLER`.

2. **Docker**
   - `--memory` y `--memory-swap` con el mismo tope, `--cpus` si hay cuota, y las variables de la plataforma después de las del usuario.
   - El container no usa `--rm`. Al terminar, Go lee `OOMKilled` y lo borra. Si el cgroup lo mató (137) o Node abortó por el heap (134), la respuesta dice que superó `memoryMb`.
   - El timeout de 124 no cambia: el plazo incluye el arranque del container. El deadline HTTP del server cubre 300s más la espera de cola.

3. **Control plane**
   - Sin campos Prisma y sin cambios en Nest. `runner.service.ts` sigue mandando timeout, memoria y handler desde el entorno, y no manda `envVars` ni `cpus`. El body que tiene que pasar a mandar está en `docs/configuracion.md`.

4. **Prueba**
   - Tests de Go de los límites y de los argumentos de `docker run`, con `-race`.
   - `make config-check` pega directo a `:8080` después de subir el zip por Nest: 400, defaults, `process.env`, log sin el secreto, `src/app.main`, timeout 2s sobre un sleep de 5s, OOM y `docker inspect` de `--cpus`.

**Validación y Corrección Humana:** Riesgos que quedan en el diseño: los valores de `envVars` viajan en el argv de `docker run` y `docker inspect` los muestra mientras el container existe. El log del compute plane los redacta; si el handler los imprime, quedan en su stdout y en el stream de logs. `getRemainingTimeInMillis` sigue en 0. El timeout cuenta el cold start. Nest todavía no persiste ni reenvía la config por función, así que `make hello` sigue con los defaults del entorno.

## Paso 3 — Containers warm en el compute plane

**Problema abordado:** Cada `POST /executions` hacía `docker run` de un container nuevo. El tiempo de la invocación incluía siempre el arranque de Node, aunque la función, la versión y la config efectiva no hubieran cambiado. Había que reutilizar containers ya prendidos sin aflojar el aislamiento del slice 1, sin mezclar logs de dos ejecuciones, y sin volver a usar un container que hizo timeout, se cayó o lo mató el OOM.

**Prompt / Herramienta utilizada:** Cursor Cloud Agent (Grok 4.7), pedido de Paso 3 del roadmap de Pedro, apilado sobre la rama de Paso 2.

**Código / Arquitectura generada:**

1. **Protocolo dentro de `runtime-node`**
   - Con `NIMBUS_WARM=1` el bootstrap carga el handler una vez y se queda leyendo una línea JSON por stdin. Responde por stdout líneas `{type:ready}`, `{type:log}`, `{type:done}` o `{type:fatal}`.
   - `console.log` / `console.error` se reemiten como frames de log. El protocolo escribe con `fs.writeSync(1)` para no envolverse a sí mismo.
   - Se eligió stdin/stdout y no HTTP ni un socket unix: el container sigue con `--network=none` y sin un mount extra de permisos. Un `docker run -i` deja un solo proceso.
   - Una excepción del handler se reporta en `done` con `ok:false` y el proceso sigue. El estado de módulo entre invokes del mismo container se conserva, como en Lambda.
   - Escribir directo al fd 1 desde el código del usuario desincroniza el protocolo: ese container se descarta.

2. **Pool en el compute plane**
   - `internal/warm` guarda slots en memoria del proceso. La clave es función + versión + handler + `memoryMb` + `cpus` + hash de `envVars`. El timeout no entra en la clave: cada invoke corta con su propio deadline de Go.
   - Tope por función = `NIMBUS_FN_CONCURRENCY` (default 2), acotado por `NIMBUS_MAX_CONTAINERS` (default 8). Los containers ociosos cuentan para ese tope. Si no entra otro slot, se desaloja el ocioso más viejo (LRU). Si todos están ocupados, esa invocación usa un container efímero que se borra al terminar.
   - Un ocioso vive `NIMBUS_WARM_IDLE_SEC` (default 60). Un reaper lo borra. `NIMBUS_WARM=0|false|off|no` apaga el pool y vuelve al `docker run` de un solo uso.
   - Timeout (124), crash, OOM o un frame que no cierra dejan el slot marcado y el container se borra con `docker rm -f`. No se reutiliza.
   - Durante el invoke el container se renombra a `nimbus-{executionId}` para que `docker inspect` de la prueba de CPU lo encuentre, y después vuelve al nombre del pool.
   - Labels `nimbus.warm=1`, `nimbus.functionId` y `nimbus.version`. Al arrancar se hace `docker rm -f` de los que quedaron con ese label. Al apagar el proceso se destruyen los del pool.
   - El código descomprimido vive en `NIMBUS_WORK_DIR/warm-code/` y se monta en `/var/task:ro`. El container warm corre `--user node`, con `--read-only`, `--tmpfs /tmp` y `HOME=/tmp`. El camino frío no cambia de usuario.
   - La respuesta de `POST /executions` suma `coldStart` e `initMs`. `initMs` mide desde `docker start` hasta el frame `ready` (mínimo 1 si el reloj da 0). En un acierto warm, `initMs` es 0. En el camino frío de un solo uso, `coldStart` es true e `initMs` es 0 porque ese camino no separa el arranque. Un 429 de cola no lleva esos campos. Nest no los reenvía.

3. **Aislamiento que se mantiene**
   - `--memory` y `--memory-swap` iguales, `--pids-limit=256`, `--network=none`, filesystem de solo lectura, límites de CPU si vinieron en la config. Los logs de cada invoke siguen yendo a `nimbus:logs:{executionId}`.

4. **Prueba**
   - Tests de Go del pool con reloj inyectado y sesiones falsas: reuso, clave, descarte, TTL, LRU, tope por función, efímero, shutdown, carrera. `go test -race ./...`.
   - `make warm-check` pega a `:8080`: series cold/warm, streams de log separados, `docker ps` vacío después del idle, timeout y OOM que obligan un cold siguiente.
   - En esta máquina la mediana de `durationMs` fue 166 ms en cold y 41 ms en warm; la mediana de `initMs` del cold fue 118 ms. La primera invocación, con descarga del zip, tardó 163 ms.

**Validación y Corrección Humana:** Riesgos que quedan en el diseño: el estado de módulo (y cualquier cosa en `/tmp`) se filtra entre invokes del mismo container. Un `write` directo al fd 1 rompe el framing y obliga a tirar el container. Los valores de `envVars` siguen visibles en `docker inspect` mientras el container vive. `NIMBUS_TIMEOUT_SEC` dentro del container es el del invoke que lo creó; el corte real lo hace Go. Cancelar una ejecución que ya corre sigue sin matar el container. Los ociosos cuentan para `NIMBUS_MAX_CONTAINERS`. El tmpfs de `/tmp` entra en la cuenta de memoria del cgroup. El camino frío sigue arrancando como root de la imagen. Nest no reenvía `coldStart` ni `initMs`.

---

## Paso 4 — Invocación async, reintentos y DLQ

**Problema abordado:** Todas las invocaciones bloqueaban al cliente hasta que el container terminaba. No había reintento si el handler fallaba o el proceso moría a mitad, ni un lugar donde quedara lo que agotó los intentos. La fila de Postgres tiene que terminar con el estado, el output y la cantidad de intentos, y el invoke síncrono tiene que seguir respondiendo igual.

**Prompt / Herramienta utilizada:** Cursor Cloud Agent (Grok 4.7), pedido de Paso 4 del roadmap de Pedro, apilado sobre la rama de Paso 3.

**Código / Arquitectura generada:**

1. **Cola durable en el compute plane**
   - `POST /async/jobs` valida la config (un 400 no se encola y no va a la DLQ) y hace `XADD` a `nimbus:async:jobs`. El mismo `invocationId` no entra dos veces.
   - Workers del proceso leen con un consumer group. `XAUTOCLAIM` recupera mensajes cuyo consumer murió. Un lock con renovación impide que la misma invocación corra dos veces a la vez. Si el lock venció, ese intento se cuenta y el siguiente arranca.
   - El worker llama al mismo `dispatch` que `POST /executions`: semáforo, cola FIFO, config, pool warm y logs.
   - Un fallo reintentable (handler, timeout, crash, 429) se confirma y se guarda en el sorted set `nimbus:async:retry` con el vencimiento del backoff. El backoff es exponencial con tope y jitter. Default: 3 intentos (el primero más dos reintentos).
   - Al agotar los intentos, la entrada va a la DLQ (hash + stream) con payload, último error, intentos y timestamps. `GET /dlq` la lista. `POST /dlq/:id/redrive` la vuelve a encolar como intento 1.
   - Cada resultado se publica en `nimbus:async:results` con un `seq` por invocación.

2. **Control plane, cambio chico**
   - `POST /functions/:id/invoke?async=true` o `X-Invocation-Type: Event` crea la fila y responde 202 con el id. Sin el flag, la respuesta sigue siendo 200 cuando termina.
   - El sync marca `RUNNING` antes de llamar a Go. El async lo marca cuando el worker arranca el intento.
   - Columna opcional `invocations.attempts`. `GET /invocations/:id` la devuelve.
   - Un consumidor de Nest aplica el stream de resultados. Go no escribe Postgres.

3. **Prueba**
   - Tests de Go con miniredis: backoff, retry diferido, DLQ, redrive, `XAUTOCLAIM`, el lock y que un 400 no reintenta. `go test -race ./...`.
   - `make async-check` cubre el 202, el SUCCESS posterior, el sync, la DLQ con timestamps, el redrive y un `SIGKILL` del compute plane a mitad de un sleep.

El contrato queda en `docs/async.md`.

**Validación y Corrección Humana:** Riesgos que quedan en el diseño: el reclaim y el contenedor huérfano pueden solaparse unos segundos después de un kill, hasta que el arranque borra los warm. Los intentos de una misma invocación comparten `executionId` y el stream de logs, así que un `_end` de un intento puede aparecer antes que las líneas del siguiente. El 429 de la cola de concurrencia gasta un intento. `NIMBUS_ASYNC_CLAIM_IDLE_MS` más corto que una ejecución larga hace que otro worker vea el mensaje pendiente; el lock evita correrlo dos veces, pero el idle se reinicia. Nest aplica resultados en orden de `seq`; si ese consumidor está caído, la fila se actualiza cuando vuelve.

---

## Paso 5 — Observabilidad del compute plane

**Problema abordado:** No había forma de ver, fuera de los logs, cuántas invocaciones corrían, cuántas fallaban, cuánto duraban, cuántas fueron cold o warm, ni qué tan llena estaba la cola o la DLQ. Tampoco se podía seguir un invoke desde Nest hasta el container.

**Prompt / Herramienta utilizada:** Cursor Cloud Agent (Grok 4.7), pedido de Paso 5 del roadmap de Pedro, apilado sobre la rama de Paso 4.

**Código / Arquitectura generada:**

1. **Métricas en el compute plane**
   - `GET /metrics` con `prometheus/client_golang`. Contadores de invocaciones por `function_id`, `status` (`success`, `error`, `timeout`, `oom`) y `mode` (`sync`, `async`), más series aparte de errores, timeouts y OOM.
   - Histograma de duración, cold vs warm, e histograma de `initMs`.
   - Espera de cola, profundidad de la cola por función (scrape a Redis), ejecuciones activas, tamaño del pool warm, slots globales y el tope `NIMBUS_MAX_CONTAINERS`.
   - Rechazos 429 (`queue_full`, `queue_timeout`), reintentos async y el tamaño de la DLQ.
   - `function_id` es etiqueta. `execution_id` no: una serie por ejecución no se queda quieta.

2. **Trazas OpenTelemetry**
   - Go exporta OTLP HTTP con un batch processor. `NIMBUS_TRACING=0` (o `OTEL_SDK_DISABLED=true`) no instala el exporter. Si el collector no responde, el error se loguea como mucho cada 30s y la invocación no falla.
   - Nest abre `nimbus.invoke` y `nimbus.compute.call`, e inyecta `traceparent` en `POST /executions` y `POST /async/jobs`.
   - El job de Redis guarda `traceparent` y `tracestate`. El worker abre `async.attempt` en esa traza, también en los reintentos.
   - Los spans de Go son `queue.wait`, `artifact.download`, `container.acquire` y `container.run`.
   - El container recibe `NIMBUS_TRACE_ID` y `TRACEPARENT`. En el camino frío van en `docker -e`. En el warm el proceso se reutiliza, así que cada invoke los manda en el frame de stdin y el bootstrap los publica en `process.env` para esa llamada, y los escribe en el log.

3. **Perfil de Compose**
   - `make up` no levanta observabilidad. `make obs` prende Prometheus (`:9090`), Grafana (`:3001`, viewer anónimo, dashboard provisionado), Jaeger (`:16686`) y un collector OTLP (`:4318`) que reenvía a Jaeger.
   - `make obs-check` genera sync, async, error, timeout, OOM, DLQ y un 429, y consulta las APIs. CI no corre ese target.

4. **Control plane, cambio chico**
   - `GET /metrics` con dos series HTTP (`nimbus_http_requests_total`, `nimbus_http_request_duration_seconds`). Los UUID del path se reemplazan por `:id`.
   - `runner.service.ts` propaga el traceparent. No hay columnas nuevas. El body de `POST /executions` no cambia.

**Validación y Corrección Humana:** Riesgos que quedan en el diseño: el trace id dentro de un container warm se actualiza por invoke en `process.env`, pero `docker inspect` sigue mostrando el valor del `docker run` que creó el proceso. Un collector caído descarta spans; no hay cola durable de trazas. La profundidad de cola y la DLQ se leen en el scrape, así que un pico más corto que el intervalo de 5s puede no aparecer. `nimbus_containers_in_use` cuenta slots de Redis, no `docker ps`. Las etiquetas por `function_id` crecen con la cantidad de funciones del proceso.

---

## QA de los pasos 0 a 5

**Problema abordado:** El stack de los pasos 0 a 5 (concurrencia, config, warm, async y observabilidad) estaba entero en ramas sin mergear. Había que correrlo junto, buscar interacciones que los checks de cada paso no cubren, y corregir lo que se rompiera.

**Prompt / Herramienta utilizada:** Cursor Cloud Agent (Grok 4.7). Pedido de QA profundo del stack #9–#15, con Docker en la VM, la suite de `make` dos veces, un flujo de consola con Chrome headless, stress, restarts, zips maliciosos y compatibilidad (`NIMBUS_WARM=0`, `NIMBUS_TRACING=0`, body mínimo de `POST /executions`).

**Código / Arquitectura generada:**

1. **Unzip del artifact en el host**
   - Tope de 32 MiB descomprimidos, 4096 entradas, y rechazo de symlinks. El path `..` ya se rechazaba. El fallo sigue siendo `Error extrayendo zip`.

2. **Cancel de un async que todavía no fue admitido**
   - `XADD`, el retry y el redrive crean `nimbus:exec:{id}` en `pending` (TTL 24 h). El admit, si ve `cancelled`, no ejecuta. Un 409 es fatal: no reintenta y no entra a la DLQ.

3. **DLQ**
   - `dead()` copia `DurationMs` del intento. Antes el JSON lo perdía (`omitempty`).

4. **Tag de versión**
   - Dejó de ser `YYYYMMDD-HHMMSS`. Ahora lleva milisegundos y un sufijo aleatorio, y un choque del unique reintenta con otro tag. El tag viejo hacía que un segundo deploy del mismo segundo pisara el zip en MinIO y devolviera 500.

5. **Consola**
   - El healthcheck de nginx usa `127.0.0.1` (wget resolvía `localhost` a `::1`). El panel SSE no marca error cuando el stream cierra después de `end`.

6. **`make qa`**
   - `scripts/qa-deep.sh` y `scripts/qa-console.mjs`. El informe está en `docs/qa-pasos-0-5.md`.

**Validación y Corrección Humana:** Riesgos que quedan en el diseño: el camino frío (`NIMBUS_WARM=0`) sigue como root de la imagen. `docker inspect` muestra los env mientras el container vive. Multer no limita el tamaño del upload; el tope del zip aplica al descomprimir. Nest no reenvía `envVars` ni `cpus`, y no hay auth.

---

## Cancelar una ejecución que ya corre

**Problema abordado:** `POST /executions/:id/cancel` marcaba `cancelled` en Redis y, si la ejecución ya tenía container, no lo mataba. Un sleep de 2500 ms cancelado cerca de los 400 ms terminaba `SUCCESS` cerca de los 2500 ms, en frío y en warm. Un async cancelado en curso podía seguir, reintentar o caer en la DLQ. Un `SIGKILL` del compute plane dejaba `nimbus:slots:machine:{hostname}` (y los slots de la función) con miembros vencidos: la admisión solo barre la key del proceso vivo, y el hostname cambia al recrear el container.

**Prompt / Herramienta utilizada:** Cursor Cloud Agent (Grok 4.7), pedido de cerrar el hallazgo de QA de severidad media (cancel real) y el de severidad baja (keys de slots de un host muerto), en un PR aparte sobre `main`.

**Código / Arquitectura generada:**

1. **Corte del proceso**
   - El handler abre un contexto con causa propia para la ejecución que ya tiene slot. `POST /executions/:id/cancel` sigue poniendo el flag de Redis (cola y async todavía no admitido) y, si este proceso la está corriendo, cancela esa causa.
   - El camino frío hace `docker rm -f` del container `nimbus-{executionId}` en cuanto llega la causa. El exit 137 de ese `SIGKILL` no se clasifica como OOM.
   - El camino warm hace `docker rm -f` del container renombrado y lo marca tainted. El pool lo destruye y no lo devuelve. El invoke siguiente de esa clave es cold.
   - `POST /executions` responde 409 con `success: false`, `exitCode` 130 y `error: "Ejecución cancelada"`. La métrica `nimbus_invocations_total` usa el status `cancelled`.

2. **Async**
   - El 409 ya era fatal: no reintenta y no entra a la DLQ. El worker publica `status: ERROR` con el mismo texto. `attempts` no sube por el corte.

3. **Postgres**
   - El enum `InvocationStatus` no tiene `CANCELLED`. No hay migración. La fila queda en `ERROR` con el mensaje explícito. Nest no cambia: el sync ya guarda `ERROR` cuando `success` es false y el exit no es 124, y el consumidor async ya aplica un `ERROR`.

4. **Slots de un host muerto**
   - Al arrancar, y cada medio `NIMBUS_SLOT_LEASE_SEC`, se recorren `nimbus:slots:machine:*` y `nimbus:slots:fn:*`. Se borran los scores ya vencidos y la key si quedó vacía. Un peer con lease vigente no se toca. La cola FIFO no entra en el barrido.

5. **Prueba**
   - Tests de Go del override de un resultado success, del descarte del pool, del 409 sync, del async que no va a la DLQ, de la métrica `cancelled` y del barrido que borra al host muerto y deja al peer.
   - `make qa` exige duration menor que el sleep, que el container no siga, que el warm siguiente sea cold, el mismo corte con `NIMBUS_WARM=0`, el async en `ERROR` sin DLQ, y el `EXISTS` de la key muerta en 0.

**Validación y Corrección Humana:** El cupo de Redis del host muerto sigue contando hasta que vence el lease (como máximo `NIMBUS_SLOT_LEASE_SEC`); el barrido no borra leases todavía vigentes de otro proceso. Cancelar y que el handler alcance a responder en el mismo instante deja el resultado en cancelado y tira el container. El invoke sync de Nest sigue respondiendo HTTP 200 con `status: ERROR` y el texto de cancelación.

