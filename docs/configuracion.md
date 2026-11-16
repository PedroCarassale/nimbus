# Configuración por función (Paso 2, lado Go)

El compute plane aplica, en cada `POST /executions`, timeout, memoria, CPU, handler y variables de entorno. Todos los campos son opcionales. Si no vienen, se usan los defaults del proceso. Un valor fuera de rango responde **400** con JSON y no llega a Docker ni ocupa un slot de la cola.

El control plane (Nest, Postgres, consola) no guarda esta config todavía. Lo arma Federico en su Paso 2. Acá está el contrato que Go ya cumple y el body que Nest tiene que mandar.

## `POST /executions`

```json
{
  "executionId": "uuid",
  "functionId": "uuid",
  "version": "20240115-120000",
  "event": {},
  "timeoutSec": 10,
  "memoryMb": 256,
  "cpus": 0.5,
  "handler": "src/app.main",
  "envVars": { "GREETING": "hola" },
  "maxConcurrency": 2
}
```

`executionId`, `functionId` y `version` siguen siendo obligatorios. El resto es opcional y compatible con los clientes que ya existen (Nest hoy manda `timeoutSec`, `memoryMb` y `handler`; no manda `cpus`, `envVars` ni `runtime`).

| Campo | Tipo | Si falta o es 0 | Si viene un valor | Cómo se aplica |
|-------|------|-----------------|-------------------|----------------|
| `timeoutSec` | entero | default del entorno | 1..300 | El contexto de Go mata el container y responde exit code **124** |
| `memoryMb` | entero | default del entorno | 64..2048 | `docker run --memory` y `--memory-swap` con el mismo valor (sin swap extra) |
| `cpus` | número | default del entorno | 0.1..4 | `docker run --cpus`. Si el default también es vacío, no se pasa el flag |
| `handler` | string | `handler` | ver abajo | `NIMBUS_HANDLER` dentro del container |
| `runtime` | string | `nodejs20` | `nodejs20`, `python3.12`, `go1.x` | Imagen y bootstrap. Ver [runtimes.md](runtimes.md) |
| `envVars` | objeto string→string | ninguna | límites de abajo | `docker run -e`. Los valores no se escriben en logs |
| `maxConcurrency` | entero | `NIMBUS_FN_CONCURRENCY` | ver [concurrencia.md](concurrencia.md) | Solo la admisión de esa invocación |

`0` en `timeoutSec`, `memoryMb` o `cpus` significa “no vino” y usa el default. Un negativo, un decimal en timeout o memoria, un string donde va un número, o un valor fuera de rango es **400**.

Respuesta de error:

```json
{
  "error": "timeoutSec debe ser un entero entre 1 y 300",
  "field": "timeoutSec"
}
```

`field` es `timeoutSec`, `memoryMb`, `cpus`, `handler`, `runtime` o `envVars`. El texto de `error` no incluye valores de variables de entorno.

Un timeout que se cumple sigue respondiendo **504** con `exitCode` 124, como antes. Un fallo del handler o un OOM responden **200** con `success: false` y el `exitCode` real (137 si lo mató el cgroup, 134 si Node abortó por el heap).

## Defaults del proceso (compute plane)

| Variable | Default | Si el valor es inválido |
|----------|---------|-------------------------|
| `NIMBUS_TIMEOUT_SEC` | `5` | se ignora y queda 5 |
| `NIMBUS_MEMORY_MB` | `128` | se ignora y queda 128 |
| `NIMBUS_MEMORY` | — | se lee solo si `NIMBUS_MEMORY_MB` no está. Acepta `128` o `128m` |
| `NIMBUS_HANDLER` | `handler` | se ignora y queda `handler` |
| `NIMBUS_CPUS` | vacío | no se pasa `--cpus`. Un valor fuera de 0.1..4 se ignora |

Estos topes (1..300, 64..2048, 0.1..4) son los del compute plane. El control plane puede imponer un máximo más chico al guardar la función; Go igual rechaza lo que se pase de acá.

El reloj del HTTP del compute plane cubre el timeout máximo (300s) más la espera de cola más un margen, para que un timeout válido no lo corte el servidor antes que Docker.

## Handler `archivo.export`

El bootstrap (`runtime-node/bootstrap.js`) resolvía siempre `/var/task/index.js` y usaba `NIMBUS_HANDLER_FUNCTION` solo como nombre del export. `index.handler` se buscaba como un export literal `"index.handler"` y no existía.

Ahora `NIMBUS_HANDLER` es la spec completa. El último punto separa el archivo del export. El `.js` no se escribe: Node prueba `archivo.js` y `archivo/index.js`, siempre debajo de `/var/task`.

| Spec | Archivo | Export |
|------|---------|--------|
| `handler` | `/var/task/index.js` | `handler` |
| `main` | `/var/task/index.js` | `main` |
| `index.handler` | `/var/task/index.js` | `handler` |
| `src/app.main` | `/var/task/src/app.js` (o `src/app/index.js`) | `main` |
| `src/app` | `/var/task/src/app.js` | `handler` |

Se rechaza un path absoluto, `..`, barras invertidas, espacios y más de un punto de export (`index.handler.extra`). El módulo tiene que exportar una función con ese nombre (`exports.main = async (event, context) => { ... }`).

`context.memoryLimitInMB` sale de `NIMBUS_MEMORY_MB`, que Go setea con el `memoryMb` efectivo. `context.getRemainingTimeInMillis` sigue devolviendo 0.

## Variables de entorno

`envVars` es un objeto. Cada valor es un string.

- Como máximo **32** variables.
- Nombre: `[A-Za-z_][A-Za-z0-9_]*`, hasta **128** caracteres.
- Valor: hasta **4096** bytes, sin saltos de línea ni NUL.
- La suma de nombres y valores no pasa de **16 KiB**.
- Un nombre que empieza con `NIMBUS_` (cualquier mayúsculas) está reservado y no se puede pisar. Go igual pone sus variables después de las del usuario, así que `NIMBUS_HANDLER`, `NIMBUS_HANDLER_PATH`, `NIMBUS_HANDLER_FUNCTION`, `NIMBUS_EVENT_PATH`, `NIMBUS_MEMORY_MB` y `NIMBUS_TIMEOUT_SEC` quedan en lo que definió la plataforma.

Los valores pueden ser secretos. El compute plane no los escribe en su log: en la línea del `docker run` se ve `CLAVE=***`. Tampoco entran en el JSON de error. Sí viajan en el `docker run -e` (el daemon y `docker inspect` los muestran mientras el container existe) y, si el handler los imprime, quedan en el stdout de la función y en el stream de logs del usuario.

## Memoria y CPU

`--memory` y `--memory-swap` usan el mismo número, así el límite es `memoryMb` y no el doble por swap. Si el proceso se pasa, el cgroup lo mata. Go mira `OOMKilled` con `docker inspect` (por eso el container no usa `--rm`: se borra después del inspect) y responde:

```json
{
  "success": false,
  "exitCode": 137,
  "error": "La función superó el límite de memoria (128 MB) y fue terminada"
}
```

El mismo texto se usa si Node aborta con “JavaScript heap out of memory” (exit 134). Un timeout también manda SIGKILL, pero si el plazo se venció primero el exit code es 124, no un OOM.

`--cpus=0.5` queda en `HostConfig.NanoCpus` (500000000). Sin `cpus` en el request y sin `NIMBUS_CPUS`, el container no tiene cuota de CPU.

El aislamiento anterior no cambia: `--network=none`, `--read-only`, `--pids-limit=256`. Con el pool warm el container además corre como el usuario `node` y acepta varios invokes; el contrato de memoria, CPU, handler y `envVars` es el mismo. Ver [`warm.md`](warm.md).

## Zip del artifact

Go descomprime el zip en el host, bajo `NIMBUS_WORK_DIR`, antes de crear el container. Un path con `..` que salga del directorio de la versión se rechaza. Un symlink también se rechaza. El total descomprimido no puede pasar de 32 MiB y el zip no puede traer más de 4096 entradas. Si se pasa, la invocación falla con `Error extrayendo zip` y no deja el archivo afuera.

## Qué manda Nest hoy

No hay campos nuevos en Prisma. `control-plane/src/functions/runner.service.ts` sigue armando el body así, con variables de entorno del control plane y no con la config de la función:

- `timeoutSec`: `NIMBUS_TIMEOUT_SEC` o `5`
- `memoryMb`: `NIMBUS_MEMORY` (default `128m`), sacando la `m`
- `handler`: `NIMBUS_HANDLER` o `handler`
- no envía `envVars`
- no envía `cpus`

`make hello` y `make smoke` siguen pasando por ese camino. Para probar un valor distinto hay que llamar al compute plane directo (`POST http://localhost:8080/executions`) con un artifact que ya esté en MinIO. `make config-check` hace eso.

Cuando el control plane tenga la config por función, el `invoke` tiene que reenviar este body (mismos nombres de campo):

```json
{
  "executionId": "<uuid generado por Nest>",
  "functionId": "<id de la función>",
  "version": "<tag de la versión>",
  "event": {},
  "timeoutSec": 10,
  "memoryMb": 256,
  "cpus": 0.5,
  "handler": "src/app.main",
  "envVars": { "GREETING": "hola" }
}
```

`maxConcurrency` sigue siendo opcional y aparte. Los límites que Federico valide al guardar (por ejemplo timeout ≤ 60s en la API) pueden ser más estrictos; Go aplica los de esta página.

## Prueba

Con el stack arriba:

```bash
make config-check
```

Eso comprueba los 400, un invoke sin campos opcionales, `envVars` en `process.env`, que el log del compute plane no tiene el valor, el handler `src/app.main`, un timeout de 2s sobre un sleep de 5s (exit 124), un OOM con `memoryMb` 128 y `--cpus=0.5` vía `docker inspect`.
