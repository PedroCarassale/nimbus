# Agents — Nimbus Functions

Instrucciones obligatorias para cualquier asistente de IA (Cursor, Copilot, cloud agents, etc.) que trabaje en este repo.

## Proyecto universitario — AI Decision Log

Este es un trabajo universitario. El ingeniero humano es responsable del código en producción; la IA asiste bajo auditoría.

### Archivo obligatorio

Mantener en la **raíz del repo** el archivo:

`AI-DECISIONS.md`

Es el log de auditoría de decisiones técnicas cuando se genera o cambia código/diseño con IA.

### Regla en cada cambio con IA

Antes de cerrar un commit que incluya código o diseño generado/modificado con IA, **agregar una entrada nueva** al final de `AI-DECISIONS.md` con esta estructura exacta:

```markdown
## <título corto de la decisión>

**Problema abordado:** …
**Prompt / Herramienta utilizada:** …
**Código / Arquitectura generada:** …
**Validación y Corrección Humana:** …
```

### Criterios de la sección humana

En **Validación y Corrección Humana** el humano (o el agente en su nombre, dejando claro qué falta revisar) debe señalar, si aplica:

- alucinaciones o supuestos incorrectos
- riesgos de seguridad
- ineficiencias / hotspots
- cambios hechos sobre la propuesta de la IA

Si el humano aún no validó, escribir explícitamente: `PENDIENTE de validación humana por Pedro/Federico` y no inventar una auditoría falsa.

### Qué no hacer

- No commitear solo código de IA sin actualizar `AI-DECISIONS.md`
- No borrar entradas anteriores del log
- No usar el log para chat irrelevante: solo decisiones técnicas de código/arquitectura

### Commits

Mensajes de commit en español o inglés claros. Preferir que el mismo commit (o uno inmediatamente anterior en el PR) incluya el update a `AI-DECISIONS.md`.

---

## Contexto del Proyecto

Nimbus Functions es una plataforma serverless estilo AWS Lambda, desarrollada por Pedro Carassale y Federico Tessadro.

**Stack técnico:**
- Control plane: NestJS + TypeScript
- Compute plane: Go
- Runtime: Node.js (imagen Docker fija)
- Metadata: PostgreSQL
- Cache/Cola: Redis
- Artifacts: MinIO (S3-compatible)
- Consola: React + Vite (opcional para MVP)

Ver `docs/stack.md` para detalles completos y `docs/roadmap.md` para el orden de slices.

## Estructura del Repositorio

```
nimbus-functions/
├── runtime-node/      # Imagen Docker del runtime Node.js
│   ├── Dockerfile
│   └── bootstrap.js   # Entrypoint que ejecuta el handler del usuario
├── examples/          # Funciones de ejemplo
│   └── hello/
├── scripts/           # Scripts de desarrollo
│   ├── run-zip.sh     # Ejecuta función desde zip
│   ├── run-hello.sh   # Demo rápida
│   └── pack-hello.sh  # Empaqueta ejemplo
├── docs/              # Documentación
│   ├── stack.md       # Stack técnico
│   └── roadmap.md     # Roadmap de desarrollo
└── AI-DECISIONS.md    # Log de auditoría IA (cátedra)
```

## Convenciones de código

- **Scripts bash**: `set -euo pipefail` al inicio
- **JavaScript**: CommonJS para runtime, ES modules donde aplique
- **Docker**: Imágenes Alpine cuando sea posible
- **Commits**: Mensajes descriptivos en español o inglés

## Fuera de alcance (no implementar sin pedir)

Ver `docs/roadmap.md` para el orden de slices. No adelantar:
- Nest, Go, MinIO, Postgres, Redis, SSE, React (slices futuros)
- Multi-runtime (Python, Go)
- Firecracker/microVM
- Auth, billing, VPC

## Variables de Entorno del Runner

| Variable | Default | Descripción |
|----------|---------|-------------|
| `NIMBUS_MEMORY` | `128m` | Límite de memoria Docker |
| `NIMBUS_TIMEOUT_SEC` | `5` | Timeout de ejecución |
| `NIMBUS_HANDLER` | `handler` | Nombre de la función exportada |
| `NIMBUS_IMAGE` | `nimbus-node` | Nombre de la imagen Docker |

## Testing Local

```bash
# Demo rápida (empaqueta y ejecuta hello)
./scripts/run-hello.sh

# Con evento personalizado
./scripts/run-hello.sh mi-evento.json

# Desde un zip arbitrario
./scripts/run-zip.sh path/to/fn.zip event.json
```
