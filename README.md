# Nimbus Functions

Plataforma serverless (estilo Lambda) de Pedro Carassale y Federico Tessadro.

## Documentación

- **Stack técnico del MVP:** [docs/stack.md](docs/stack.md)
- **Roadmap hasta el MVP:** [docs/roadmap.md](docs/roadmap.md)
- **Auditoría IA (cátedra):** [AI-DECISIONS.md](AI-DECISIONS.md)
- **Instrucciones para agentes:** [agents.md](agents.md)

## Quickstart

### Requisitos

- Docker + Docker Compose
- Bash
- zip/unzip
- AWS CLI o MinIO Client (`mc`) — para artifacts en MinIO

### Demo rápida (local)

Ejecuta el ejemplo "hello" con un solo comando:

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

### Control Plane API (Slice 3)

```bash
# Levantar todo el stack (postgres + minio + control-plane)
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

Ver `.env.example` para todas las variables.

Ejemplo con límites personalizados:

```bash
NIMBUS_MEMORY=256m NIMBUS_TIMEOUT_SEC=10 ./scripts/run-zip.sh mi-funcion.zip
```

## Estructura del Proyecto

```
nimbus-functions/
├── control-plane/         # NestJS Control Plane (Slice 3)
│   ├── src/               # Código fuente TypeScript
│   │   ├── functions/     # Módulo de funciones (CRUD + invoke)
│   │   ├── prisma/        # Servicio Prisma
│   │   └── minio/         # Servicio MinIO/S3
│   ├── prisma/            # Schema y migraciones Postgres
│   ├── Dockerfile         # Imagen del control plane
│   └── package.json       # Dependencias Node.js
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
│   └── smoke-test.sh      # Test E2E del control plane (Slice 3)
├── docs/                  # Documentación
│   ├── stack.md           # Stack técnico
│   └── roadmap.md         # Roadmap de desarrollo
├── docker-compose.yml     # Postgres + MinIO + Control Plane
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
