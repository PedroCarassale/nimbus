# Nimbus Functions

Plataforma serverless (estilo Lambda) de Pedro Carassale y Federico Tessadro.

## Documentación

- **Stack técnico del MVP:** [docs/stack.md](docs/stack.md)
- **Roadmap hasta el MVP:** [docs/roadmap.md](docs/roadmap.md)
- **Auditoría IA (cátedra):** [AI-DECISIONS.md](AI-DECISIONS.md)
- **Instrucciones para agentes:** [agents.md](agents.md)

---

## 🚀 MVP Demo — 3 Comandos

**Requisitos:** Docker, Docker Compose, curl, jq, zip

```bash
# 1. Clonar e ir al directorio
git clone https://github.com/PedroCarassale/nimbus.git && cd nimbus

# 2. Levantar el stack completo
make up

# 3. Ejecutar demo end-to-end: crear → desplegar → invocar función
make hello

# 4. Abrir la consola web (opcional)
make console
```

**Consola Web:** http://localhost:8088

Esto demuestra el flujo completo del MVP:
- **Control Plane** (NestJS): crea función, almacena metadata en Postgres, sube código a MinIO
- **Compute Plane** (Go): ejecuta la función en Docker aislado con timeout y límites de memoria
- **Live Logs** (SSE): logs en tiempo real via Redis streams

### Arquitectura

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                              docker compose up                                   │
├──────────┬──────────┬──────────┬────────────┬────────────┬──────────────────────┤
│ Postgres │  Redis   │  MinIO   │  Control   │  Compute   │      Console         │
│ (metada) │ (locks)  │(artifacts│ Plane:3000 │ Plane:8080 │   (React):8088       │
└──────────┴──────────┴──────────┴─────┬──────┴──────┬─────┴──────────┬───────────┘
                                       │ HTTP        │ Docker         │ Browser
                                       ▼             ▼                ▼
                                 ┌─────────────────────────────┐  ┌────────────┐
                                 │  nimbus-node container      │  │   Usuario  │
                                 │  (isolated: --network=none) │  │            │
                                 └─────────────────────────────┘  └────────────┘
```

### Comandos Útiles

```bash
make help         # Ver todos los comandos disponibles
make console      # Abrir consola web en http://localhost:8088
make smoke        # Test completo con verificación de logs SSE
make logs         # Ver logs de todos los servicios
make down         # Detener el stack
make down-clean   # Detener y eliminar volúmenes
```

### Consola Web

La consola en http://localhost:8088 permite:
- Crear y listar funciones
- Subir archivo .zip para desplegar
- Invocar funciones con editor JSON
- Ver resultado de la invocación
- Logs en tiempo real via SSE

### Ver Logs en Tiempo Real (SSE)

Después de invocar una función, puedes ver los logs en streaming:

```bash
curl -N http://localhost:3000/invocations/{invocationId}/logs
```

---

## Quickstart Detallado

### Requisitos

- Docker + Docker Compose
- Bash
- zip/unzip
- curl y jq (para scripts de demo)

### Demo local sin stack (solo runtime)

Ejecuta el ejemplo "hello" directamente con Docker:

```bash
./scripts/run-hello.sh
```

Esto empaqueta `examples/hello/` en un zip y lo ejecuta con el runtime Node.js.

### Demo con MinIO (Slice 2)

Ejecuta el flujo completo con almacenamiento de artifacts en MinIO:

```bash
./scripts/run-hello-from-minio.sh
```

Esto:
1. Levanta MinIO si no está corriendo
2. Empaqueta y sube `hello` a MinIO
3. Descarga y ejecuta la función desde MinIO

Consola MinIO: http://localhost:9001 (usuario: `minioadmin`, password: `minioadmin`)

### Ejecutar una función desde zip

```bash
# Empaquetar una función
./scripts/pack-hello.sh  # Crea dist/hello.zip

# Ejecutar desde zip
./scripts/run-zip.sh dist/hello.zip

# Con evento personalizado
./scripts/run-zip.sh dist/hello.zip mi-evento.json
```

### MinIO y Artifacts (Slice 2)

```bash
# Levantar MinIO (crea el bucket automáticamente)
docker compose up -d minio minio-init

# Subir un artifact
./scripts/upload-artifact.sh hello dist/hello.zip

# Subir hello (empaqueta + sube)
./scripts/upload-hello.sh

# Ejecutar desde MinIO
./scripts/run-artifact.sh hello latest

# Ejecutar versión específica
./scripts/run-artifact.sh hello 20240115-120000 evento.json
```

### Control Plane API (Slice 3-4)

```bash
# Levantar todo el stack (postgres + redis + minio + compute-plane + control-plane)
docker compose up -d

# Esperar a que esté listo
curl http://localhost:3000/health

# Crear una función
curl -X POST http://localhost:3000/functions \
  -H "Content-Type: application/json" \
  -d '{"name": "mi-funcion"}'

# Desplegar un zip (obtener FN_ID del paso anterior)
./scripts/pack-hello.sh
curl -X POST http://localhost:3000/functions/{FN_ID}/deploy \
  -F "file=@dist/hello.zip"

# Invocar la función
curl -X POST http://localhost:3000/functions/{FN_ID}/invoke \
  -H "Content-Type: application/json" \
  -d '{"event": {"nombre": "Nimbus"}}'

# Smoke test completo (create → deploy → invoke)
./scripts/smoke-test.sh
```

#### Endpoints del Control Plane

| Método | Endpoint | Descripción |
|--------|----------|-------------|
| GET | `/health` | Health check |
| POST | `/functions` | Crear función |
| GET | `/functions` | Listar funciones |
| GET | `/functions/:id` | Obtener detalles |
| POST | `/functions/:id/deploy` | Desplegar zip |
| POST | `/functions/:id/invoke` | Invocar función |
| GET | `/invocations/:id` | Obtener detalles de invocación |
| GET | `/invocations/:id/logs` | Stream SSE de logs en vivo |

#### Endpoints del Compute Plane (interno)

| Método | Endpoint | Descripción |
|--------|----------|-------------|
| GET | `/health` | Health check |
| POST | `/executions` | Ejecutar función (Nest → Go) |
| POST | `/executions/:id/cancel` | Cancelar ejecución |

### Live Logs con SSE (Slice 5)

El endpoint `/invocations/:id/logs` expone logs en tiempo real usando Server-Sent Events.

```bash
# Invocar función y obtener invocationId
RESPONSE=$(curl -s -X POST http://localhost:3000/functions/{FN_ID}/invoke \
  -H "Content-Type: application/json" \
  -d '{"event": {"nombre": "Test"}}')

INVOCATION_ID=$(echo "$RESPONSE" | jq -r '.invocationId')

# Ver logs en vivo (streaming)
curl -N http://localhost:3000/invocations/$INVOCATION_ID/logs

# Ver logs existentes sin streaming
curl -N "http://localhost:3000/invocations/$INVOCATION_ID/logs?follow=false"
```

**Formato de eventos SSE:**

```
event: connected
data: {"invocationId":"...","requestId":"...","executionId":"...","status":"RUNNING"}

event: log
data: {"timestamp":1695744000000,"stream":"stdout","line":"Hello World","sequence":1}

event: log
data: {"timestamp":1695744000100,"stream":"stderr","line":"[debug] processing...","sequence":2}

event: end
data: {"message":"Execution complete"}
```

**Esquema de keys Redis:**
- Stream de logs: `nimbus:logs:{executionId}`
- TTL: 1 hora después de completar la ejecución
- Max entries: 1000 líneas por stream

### Arquitectura Slice 4

```
Cliente HTTP
    │
    ▼
┌─────────────────────────────────────┐
│   Control Plane (NestJS :3000)      │
│   - Valida request                  │
│   - Consulta metadata en Postgres   │
│   - Registra invocation             │
└──────────────┬──────────────────────┘
               │ HTTP POST /executions
               ▼
┌─────────────────────────────────────┐
│   Compute Plane (Go :8080)          │
│   - Adquiere lock Redis por función │
│   - Descarga artifact de MinIO      │
│   - Ejecuta Docker (isolated)       │
│   - Libera lock y retorna resultado │
└─────────────────────────────────────┘
               │
               ▼
┌─────────────────────────────────────┐
│   Docker Container (nimbus-node)    │
│   --network=none --read-only        │
│   --memory=128m --pids-limit=256    │
└─────────────────────────────────────┘
```

### Contrato Nest ↔ Go

**Request POST /executions:**
```json
{
  "executionId": "uuid",
  "functionId": "uuid",
  "version": "20240115-120000",
  "event": { "key": "value" },
  "timeoutSec": 5,
  "memoryMb": 128,
  "handler": "handler"
}
```

**Response:**
```json
{
  "executionId": "uuid",
  "success": true,
  "output": { "resultado": "..." },
  "error": "",
  "exitCode": 0,
  "durationMs": 150
}
```

**Error 429 (función ocupada):**
```json
{
  "error": "Función ocupada, reintente más tarde",
  "executionId": "uuid",
  "retryAfter": 5
}
```

### Variables de entorno

**Runner (Slice 1):**

| Variable | Default | Descripción |
|----------|---------|-------------|
| `NIMBUS_MEMORY` | `128m` | Límite de memoria Docker |
| `NIMBUS_TIMEOUT_SEC` | `5` | Timeout de ejecución en segundos |
| `NIMBUS_HANDLER` | `handler` | Nombre de la función exportada |
| `NIMBUS_IMAGE` | `nimbus-node` | Nombre de la imagen Docker |

**MinIO (Slice 2):**

| Variable | Default | Descripción |
|----------|---------|-------------|
| `AWS_ACCESS_KEY_ID` | `minioadmin` | Access key para MinIO/S3 |
| `AWS_SECRET_ACCESS_KEY` | `minioadmin` | Secret key para MinIO/S3 |
| `AWS_ENDPOINT_URL` | `http://localhost:9000` | Endpoint de MinIO |
| `MINIO_BUCKET` | `nimbus-artifacts` | Nombre del bucket |

**PostgreSQL (Slice 3):**

| Variable | Default | Descripción |
|----------|---------|-------------|
| `POSTGRES_USER` | `nimbus` | Usuario de Postgres |
| `POSTGRES_PASSWORD` | `nimbus` | Password de Postgres |
| `POSTGRES_DB` | `nimbus` | Nombre de la base de datos |
| `DATABASE_URL` | `postgresql://nimbus:nimbus@localhost:5432/nimbus` | Connection string |

**Redis (Slice 4):**

| Variable | Default | Descripción |
|----------|---------|-------------|
| `REDIS_URL` | `redis:6379` | Dirección de Redis |
| `REDIS_PASSWORD` | `` | Password de Redis (vacío por defecto) |

**Compute Plane (Slice 4):**

| Variable | Default | Descripción |
|----------|---------|-------------|
| `COMPUTE_PLANE_URL` | `http://compute-plane:8080` | URL del compute plane (para Nest) |
| `COMPUTE_PLANE_PORT` | `8080` | Puerto del compute plane |

**Control Plane — Redis (Slice 5):**

| Variable | Default | Descripción |
|----------|---------|-------------|
| `REDIS_URL` | `redis:6379` | Dirección de Redis para logs SSE |
| `REDIS_PASSWORD` | `` | Password de Redis (vacío por defecto) |

Ver `.env.example` para todas las variables.

Ejemplo con límites personalizados:

```bash
NIMBUS_MEMORY=256m NIMBUS_TIMEOUT_SEC=10 ./scripts/run-zip.sh mi-funcion.zip
```

## Estructura del Proyecto

```
nimbus-functions/
├── console/               # React Console (Slice 7)
│   ├── src/               # Código fuente TypeScript + React
│   │   ├── components/    # Componentes (LogsPanel, JsonEditor)
│   │   ├── pages/         # Páginas (FunctionsList, FunctionDetail)
│   │   ├── api.ts         # Cliente HTTP para control-plane
│   │   └── types.ts       # Tipos TypeScript
│   ├── Dockerfile         # Build + nginx
│   └── package.json       # Dependencias
├── control-plane/         # NestJS Control Plane (Slice 3)
│   ├── src/               # Código fuente TypeScript
│   │   ├── functions/     # Módulo de funciones (CRUD + invoke)
│   │   ├── prisma/        # Servicio Prisma
│   │   └── minio/         # Servicio MinIO/S3
│   ├── prisma/            # Schema y migraciones Postgres
│   ├── Dockerfile         # Imagen del control plane
│   └── package.json       # Dependencias Node.js
├── compute-plane/         # Go Compute Plane (Slice 4)
│   ├── cmd/server/        # Entrypoint del servidor
│   ├── internal/
│   │   ├── handlers/      # HTTP handlers (StartExecution, Cancel)
│   │   ├── executor/      # Descarga MinIO + Docker run
│   │   ├── concurrency/   # Redis locks distribuidos
│   │   └── minio/         # Cliente MinIO
│   ├── Dockerfile         # Imagen del compute plane
│   └── go.mod             # Dependencias Go
├── runtime-node/          # Imagen Docker del runtime Node.js
│   ├── bootstrap.js       # Bootstrap de la plataforma
│   └── Dockerfile         # Imagen base del runtime
├── examples/              # Funciones de ejemplo
│   └── hello/             # Ejemplo básico
├── scripts/               # Scripts de desarrollo
│   ├── run-zip.sh         # Ejecuta función desde zip (Slice 1)
│   ├── run-hello.sh       # Demo rápida local (Slice 1)
│   ├── pack-hello.sh      # Empaqueta ejemplo en zip
│   ├── upload-artifact.sh # Sube zip a MinIO (Slice 2)
│   ├── upload-hello.sh    # Empaqueta + sube hello (Slice 2)
│   ├── run-artifact.sh    # Descarga + ejecuta desde MinIO (Slice 2)
│   ├── run-hello-from-minio.sh  # Demo completa MinIO (Slice 2)
│   ├── minio-init.sh      # Inicializa bucket (alternativo)
│   └── smoke-test.sh      # Test E2E del control plane (Slice 3-4)
├── docs/                  # Documentación
│   ├── stack.md           # Stack técnico
│   └── roadmap.md         # Roadmap de desarrollo
├── docker-compose.yml     # Postgres + Redis + MinIO + Compute + Control
├── .env.example           # Variables de entorno de ejemplo
└── dist/                  # Artifacts generados (ignorado en git)
```

## Crear tu propia función

1. Crea un directorio con tu código:

```javascript
// mi-funcion/index.js
exports.handler = async (event, context) => {
  return {
    statusCode: 200,
    body: { mensaje: "¡Hola desde Nimbus!" }
  };
};
```

2. Empaqueta en un zip:

```bash
cd mi-funcion && zip -r ../mi-funcion.zip .
```

3. Ejecuta:

```bash
./scripts/run-zip.sh mi-funcion.zip
```

## Códigos de salida

| Código | Significado |
|--------|-------------|
| 0 | Éxito |
| 1 | Error en el handler del usuario |
| 2 | Error de uso (argumentos inválidos) |
| 3 | Error construyendo imagen Docker |
| 4 | Error extrayendo zip |
| 124 | Timeout excedido |
