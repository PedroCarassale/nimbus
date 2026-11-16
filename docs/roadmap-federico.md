# Nimbus Functions — Roadmap Federico (control plane + consola)

Resumen de lo que está hecho, cómo funciona y qué falta construir en el control plane y la consola.

**División del trabajo**
- **Pedro**: compute plane + infra (Go, Docker, Redis, colas, warm containers, métricas). Ver [`roadmap-pedro.md`](roadmap-pedro.md).
- **Federico**: control plane + consola (NestJS, Postgres, React). Es todo lo que ve y usa el usuario.

Repo: https://github.com/PedroCarassale/nimbus

---

## 1. Qué es Nimbus

Una plataforma serverless tipo AWS Lambda. El usuario sube una función chica (un zip con código Node.js), la invoca por HTTP y Nimbus la ejecuta en un container aislado con límites de CPU, memoria y tiempo. Después devuelve el resultado y muestra los logs en vivo.

## 2. Qué está hecho (MVP, todo en `main`)

| # | Parte | Qué hace |
|---|-------|----------|
| 0 | Runtime Node | Imagen Docker fija con Node 20 que carga el `handler(event, context)` del usuario (`runtime-node/`) |
| 1 | Runner | Corre el zip en un container con timeout, límite de memoria, sin red y con filesystem de solo lectura |
| 2 | Artifacts | Los zips se guardan en MinIO (compatible con S3) con versión: `functions/{fnId}/versions/{version}/code.zip` |
| 3 | Control plane | NestJS + Postgres (Prisma): crear, deployar e invocar funciones, y guardar la metadata |
| 4 | Compute plane | Servicio Go que baja el zip, corre Docker y usa Redis para permitir 1 ejecución a la vez por función |
| 5 | Logs en vivo | Go escribe stdout/stderr en Redis streams y Nest los manda por SSE |
| 6 | DX local | `make up` levanta todo y `make hello` hace el flujo completo |
| 7 | Consola | React + Vite en `http://localhost:8088`: crear función, subir zip, invocar y ver logs |

**Pendiente:** el flujo completo todavía no se probó de punta a punta con Docker real.

## 3. Cómo funciona un invoke

```
Consola / curl
   │  POST /functions/:id/invoke  { event }
   ▼
Control plane (NestJS :3000)  ── Postgres (functions, versions, invocations)
   │  POST /executions  (HTTP)
   ▼
Compute plane (Go :8080) ── Redis (lock por función + log stream)
   │  baja el zip de MinIO
   ▼
Container Docker (Node 20 + código del usuario)
   │  stdout/stderr → Redis stream nimbus:logs:{executionId}
   ▼
Nest devuelve el JSON  +  GET /invocations/:id/logs (SSE)
```

La regla clave es que **Nest nunca ejecuta código del usuario**. Nest valida, guarda y le pide a Go. Go ejecuta.

## 4. Control plane + consola: estado actual

### Estructura
```
control-plane/
  prisma/schema.prisma        # modelos Function, Version, Invocation
  src/functions/              # controller + service + runner.service (cliente HTTP a Go)
  src/invocations/            # detalle de invocación + SSE de logs
  src/minio/  src/redis/  src/prisma/
console/
  src/api.ts                  # cliente de la API
  src/pages/FunctionsList.tsx  FunctionDetail.tsx
  src/components/LogsPanel.tsx JsonEditor.tsx
```

### API actual
| Método | Ruta | Qué hace |
|--------|------|----------|
| POST | `/functions` | Crea una función `{ name }` |
| GET | `/functions` | Lista las funciones |
| GET | `/functions/:id` | Detalle de una función con sus versiones |
| POST | `/functions/:id/deploy` | Sube el zip (multipart, campo `file`) y crea una versión |
| POST | `/functions/:id/invoke` | Invoca `{ event, version? }` y devuelve output, status, `logsUrl` |
| GET | `/invocations/:id` | Detalle de una invocación |
| GET | `/invocations/:id/logs` | Logs por SSE (`follow=true/false`) |

### Cosas a saber del código actual
- `name` es único a nivel global. Con usuarios va a tener que ser único **por usuario**.
- No hay auth: cualquiera ve y ejecuta todo.
- El compute plane ya aplica timeout, memoria, CPU, handler y `envVars` por invocación ([`configuracion.md`](configuracion.md)). Nest todavía no los guarda: `runner.service.ts` manda timeout, memoria y handler desde variables de entorno, y no manda `envVars` ni `cpus`.
- En el invoke sync, `RUNNING` se guarda antes de llamar a Go. En async pasa a `RUNNING` cuando el worker arranca el intento.
- Hay `GET /invocations/:id`, pero no hay un listado de invocaciones por función.

## 5. Roadmap del control plane

En orden sugerido. Cada paso es un PR chico contra `main`.

### Paso 0 — Base estable
- [ ] Clonar, `make up`, `make hello` y abrir la consola. Arreglar lo que rompa.
- [ ] Tests básicos del control plane (Jest) + CI en GitHub Actions: build y tests en cada PR.

### Paso 1 — Auth y usuarios
- [ ] Tabla `users` + registro/login (JWT) para la consola.
- [ ] API keys por usuario para usar la API desde curl o scripts.
- [ ] Cada función pertenece a un usuario (`owner_id`); `name` único por usuario.
- [ ] Todos los endpoints filtran por usuario. Nadie ve ni invoca funciones ajenas.
- [ ] Consola: login, logout y manejo de sesión.

### Paso 2 — Configuración por función
- [ ] Campos en la función: `timeoutSec`, `memoryMb`, `envVars` (variables de entorno) y `handler`.
- [ ] `PATCH /functions/:id` para editar la config, con validación de límites (ej. timeout ≤ 60s, memoria ≤ 512MB).
- [ ] Mandar la config a Go en `POST /executions` (requiere cambio de contrato, ver sección 6).
- [ ] Consola: pantalla de configuración.

### Paso 3 — Versiones y alias
- [ ] Alias con nombre (`prod`, `dev`) que apuntan a una versión.
- [ ] Invocar por versión o alias; si no se indica nada, usar `latest`.
- [ ] Rollback: mover un alias a una versión anterior.
- [ ] Consola: listado de versiones y alias, con botón de rollback.

### Paso 4 — Historial y detalle de invocaciones
- [ ] `GET /functions/:id/invocations` con paginación y filtro por estado.
- [ ] Arreglar el estado `RUNNING` en el momento correcto.
- [ ] Consola: tabla de invocaciones (estado, duración, fecha) y detalle con input, output y logs.

### Paso 5 — URL pública por función
- [ ] Cada función puede tener una URL HTTP pública (ej. `/run/{usuario}/{función}`) que convierte el request HTTP en `event` y la respuesta del handler en respuesta HTTP.
- [ ] Opción de exigir API key o dejarla pública.

### Paso 6 — Triggers programados (después)
- [ ] Cron por función: "invocar cada 5 minutos".

### Paso 7 — Pulido de la consola
- [ ] Mensajes de error claros, estados de carga y pantalla vacía con un ejemplo de handler para descargar.
- [ ] Borrar funciones y versiones (también borra el artifact en MinIO).

## 6. Contrato Nest ↔ Go

El roadmap del compute plane está en [`roadmap-pedro.md`](roadmap-pedro.md).

Es el punto de integración entre control plane y compute plane. `POST /executions` recibe:

```json
{
  "executionId": "uuid",
  "functionId": "uuid",
  "version": "20240115-120000",
  "event": {},
  "timeoutSec": 5,
  "memoryMb": 128,
  "cpus": 0.5,
  "handler": "index.handler",
  "envVars": { "GREETING": "hola" },
  "maxConcurrency": 2
}
```

`timeoutSec`, `memoryMb`, `cpus`, `handler` y `envVars` son opcionales. Go los valida y los aplica en el container. Límites, defaults y el formato del handler (`index.handler`, `src/app.main`) están en [`configuracion.md`](configuracion.md). Un valor inválido responde **400** con `error` y `field`, antes de encolar.

Hoy Nest **no** manda la config de la función. `runner.service.ts` sigue enviando:

- `timeoutSec` desde `NIMBUS_TIMEOUT_SEC` (default 5)
- `memoryMb` desde `NIMBUS_MEMORY` (default `128m`)
- `handler` desde `NIMBUS_HANDLER` (default `handler`)

No envía `envVars` ni `cpus`. Cuando exista `PATCH /functions/:id`, el invoke tiene que reenviar esos campos con los mismos nombres. No hace falta cambiar el shape de más arriba. Los máximos del control plane pueden ser más chicos; Go rechaza igual lo que se pase de 300s, 2048 MB o 4 CPUs.

`maxConcurrency` es opcional. Si no viene, Go usa `NIMBUS_FN_CONCURRENCY`. La respuesta de una ejecución que entró es `{ executionId, success, output, error, exitCode, durationMs, queuedMs, coldStart, initMs }`. `queuedMs` son los milisegundos esperados en la cola (`0` si entró directo). `coldStart` es `true` cuando Go arrancó un container para ese invoke, e `initMs` es el arranque de ese container (`0` si reutilizó uno). Nest hoy no reenvía esos dos campos. El detalle está en [`warm.md`](warm.md).

Si la cola de la función está llena, o se vence la espera, Go responde **429** con `reason` `queue_full` o `queue_timeout`. El control plane lo traduce a **429 Too Many Requests** en `POST /functions/:id/invoke`. El detalle del semáforo, la cola y el tope global está en [`concurrencia.md`](concurrencia.md).

Invocación async (Paso 4 de Pedro, con un cambio chico en Nest): `POST /functions/:id/invoke?async=true` o el header `X-Invocation-Type: Event` responde **202** con `invocationId` y no espera al handler. Go encola en Redis, reintenta con backoff y deja en una DLQ lo que agota los intentos. Nest solo crea la fila, la marca `RUNNING` cuando el worker arranca y escribe el resultado final (`status`, `output`, `attempts`). La columna `attempts` es opcional. El invoke sin ese flag sigue respondiendo 200 como antes. Contrato, variables y `GET /dlq` / `POST /dlq/:id/redrive` están en [`async.md`](async.md).

Reglas:
- Todo campo nuevo que el control plane necesite de Go (variables de entorno, invocación async, etc.) se define primero en `docs/` y después se implementa en cada lado.
- Los cambios al contrato tienen que ser compatibles hacia atrás: agregar campos opcionales, no romper los que ya existen.


## 7. Reglas del repo

- `main` está protegida: se trabaja en branches y se entra por PR.
- Un PR por feature, chico.
- **Cátedra:** cada cambio hecho con IA suma una entrada en `AI-DECISIONS.md` con *Problema / Prompt-Herramienta / Código-Arquitectura* (ver `agents.md`). Se describe qué hace el cambio.
- Código en inglés; textos de producto (consola, mensajes de error) en español.

## 8. Para arrancar

```bash
git clone https://github.com/PedroCarassale/nimbus.git
cd nimbus
make up          # levanta postgres, redis, minio, control, compute y la consola
make hello       # crea, deploya e invoca la función de ejemplo
make console     # abre http://localhost:8088
```

Para leer primero: `README.md`, `docs/stack.md`, `docs/roadmap.md`, `control-plane/src/functions/functions.controller.ts`.
