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
docker compose up -d

# Subir un artifact
./scripts/upload-artifact.sh hello dist/hello.zip

# Subir hello (empaqueta + sube)
./scripts/upload-hello.sh

# Ejecutar desde MinIO
./scripts/run-artifact.sh hello latest

# Ejecutar versión específica
./scripts/run-artifact.sh hello 20240115-120000 evento.json
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

Ver `.env.example` para todas las variables.

Ejemplo con límites personalizados:

```bash
NIMBUS_MEMORY=256m NIMBUS_TIMEOUT_SEC=10 ./scripts/run-zip.sh mi-funcion.zip
```

## Estructura del Proyecto

```
nimbus-functions/
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
│   └── minio-init.sh      # Inicializa bucket (alternativo)
├── docs/                  # Documentación
│   ├── stack.md           # Stack técnico
│   └── roadmap.md         # Roadmap de desarrollo
├── docker-compose.yml     # MinIO para desarrollo local
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
