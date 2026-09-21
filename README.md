# Nimbus Functions

Plataforma serverless (estilo Lambda) de Pedro Carassale y Federico Tessadro.

## Documentación

- **Stack técnico del MVP:** [docs/stack.md](docs/stack.md)
- **Roadmap hasta el MVP:** [docs/roadmap.md](docs/roadmap.md)
- **Auditoría IA (cátedra):** [AI-DECISIONS.md](AI-DECISIONS.md)
- **Instrucciones para agentes:** [agents.md](agents.md)

## Quickstart

### Requisitos

- Docker
- Bash
- zip/unzip

### Demo rápida

Ejecuta el ejemplo "hello" con un solo comando:

```bash
./scripts/run-hello.sh
```

Esto empaqueta `examples/hello/` en un zip y lo ejecuta con el runtime Node.js.

### Ejecutar una función desde zip

```bash
# Empaquetar una función
./scripts/pack-hello.sh  # Crea dist/hello.zip

# Ejecutar desde zip
./scripts/run-zip.sh dist/hello.zip

# Con evento personalizado
./scripts/run-zip.sh dist/hello.zip mi-evento.json
```

### Variables de entorno

| Variable | Default | Descripción |
|----------|---------|-------------|
| `NIMBUS_MEMORY` | `128m` | Límite de memoria Docker |
| `NIMBUS_TIMEOUT_SEC` | `5` | Timeout de ejecución en segundos |
| `NIMBUS_HANDLER` | `handler` | Nombre de la función exportada |
| `NIMBUS_IMAGE` | `nimbus-node` | Nombre de la imagen Docker |

Ejemplo con límites personalizados:

```bash
NIMBUS_MEMORY=256m NIMBUS_TIMEOUT_SEC=10 ./scripts/run-zip.sh mi-funcion.zip
```

## Estructura del Proyecto

```
nimbus-functions/
├── runtime-node/      # Imagen Docker del runtime Node.js
│   ├── bootstrap.js   # Bootstrap de la plataforma
│   └── Dockerfile     # Imagen base del runtime
├── examples/          # Funciones de ejemplo
│   └── hello/         # Ejemplo básico
├── scripts/           # Scripts de desarrollo
│   ├── run-zip.sh     # Ejecuta función desde zip
│   ├── run-hello.sh   # Demo rápida (pack + run)
│   └── pack-hello.sh  # Empaqueta ejemplo en zip
├── docs/              # Documentación
│   ├── stack.md       # Stack técnico
│   └── roadmap.md     # Roadmap de desarrollo
└── dist/              # Artifacts generados (zips, ignorado en git)
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
