# Nimbus Functions

Plataforma serverless (estilo Lambda) de Pedro Carassale y Federico Tessadro.

## Documentación

- **Stack técnico del MVP:** [docs/stack.md](docs/stack.md)

## Quickstart: Runtime Node.js

El runtime mínimo de Node.js ya está disponible. Para probarlo:

```bash
./scripts/run-hello.sh
```

Esto construye la imagen `nimbus-node` (si no existe) y ejecuta el ejemplo `examples/hello` dentro de un contenedor Docker.

### Estructura del runtime

```
runtime-node/
├── bootstrap.js   # Bootstrap de la plataforma
└── Dockerfile     # Imagen base del runtime

examples/hello/
└── index.js       # Handler de ejemplo
```

### Variables de entorno del runtime

| Variable | Default | Descripción |
|----------|---------|-------------|
| `NIMBUS_HANDLER_PATH` | `/var/task/index.js` | Ruta al módulo del handler |
| `NIMBUS_EVENT_PATH` | `/tmp/event.json` | Ruta al JSON del evento |

Roadmap hasta el MVP: [docs/roadmap.md](docs/roadmap.md).
