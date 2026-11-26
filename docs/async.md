# Invocación async, reintentos y DLQ (Paso 4)

Un invoke puede volver enseguida con un id y ejecutarse después. Si el handler falla, el compute plane reintenta con backoff. Cuando se acaban los intentos, la invocación queda en una DLQ para mirarla y volver a mandarla. El invoke síncrono no cambia.

## Diseño

`NIMBUS_ASYNC_BACKEND` elige el transporte. `sqs` (el valor del compose, con LocalStack) manda el job a Amazon SQS o a LocalStack. `redis`, o la variable vacía, usa el stream de abajo. Los dos publican el resultado en `nimbus:async:results`, así que Nest actualiza la fila igual. El detalle de S3 y de SQS está en [`localstack-aws.md`](localstack-aws.md).

```
POST /functions/:id/invoke?async=true
        │  202 { invocationId, executionId, status: PENDING }
        ▼
Control plane (Nest) crea la fila en Postgres
        │  POST /async/jobs
        ▼
Compute plane (Go) valida y encola (SQS o XADD a nimbus:async:jobs)
        │
        ▼
Worker del mismo proceso (consumer group)
        │  semáforo, cola, config, pool warm y logs de siempre
        ▼
Redis nimbus:async:results  ──►  un consumidor de Nest actualiza la fila
```

Con el backend `redis`, la cola es un stream con el consumer group `workers`. Cada proceso usa un nombre de consumer distinto. Si el proceso muere, el mensaje queda pendiente: el siguiente hace `XAUTOCLAIM` cuando el idle supera `NIMBUS_ASYNC_CLAIM_IDLE_MS` y lo vuelve a correr.

Mientras corre, un lock `nimbus:async:lock:{invocationId}` impide que dos workers ejecuten la misma invocación a la vez. El worker renueva el lock. Si deja de renovarlo, el lock vence y el reclaim puede empezar otro intento. Ese intento cuenta: `attempts` sube.

Un fallo reintentable no deja el mensaje pendiente. Se confirma (`XACK`) y se guarda en el sorted set `nimbus:async:retry`, con score = momento en que vence el backoff. Un loop lo promueve otra vez al stream. Así el retry sobrevive un reinicio y no depende de un `sleep` en el proceso.

Los resultados van a `nimbus:async:results`. Nest tiene su propio consumer group (`nimbus-control`) y escribe `status`, `output`, `error`, `durationMs` y `attempts` en la fila `Invocation`. Cada evento lleva un `seq` por invocación. Uno más viejo, por ejemplo una copia reclamada, no pisa un estado más nuevo.

Go no abre Postgres. La fila la sigue escribiendo Nest.

## Backend SQS

Con `NIMBUS_ASYNC_BACKEND=sqs` el worker hace long polling de `nimbus-async-jobs`. El cuerpo es el mismo job. `traceparent` y `tracestate` van en `MessageAttributes`.

La visibilidad inicial es `NIMBUS_ASYNC_CLAIM_IDLE_MS` redondeado hacia arriba a segundos, con piso de 30 s si ese valor es 0. Un backoff positivo menor a 1 s queda en 1 s: SQS no acepta fracciones. Mientras el intento corre, el heartbeat extiende la visibilidad. Si el proceso muere, el mensaje vuelve a ser visible y otro receive lo toma. El resultado se publica en Redis antes de borrar el mensaje: si Redis no acepta el resultado, el mensaje sigue en la cola y el siguiente receive lo publica de nuevo. Éxito, cancelación y un 400 de validación borran el mensaje. No van a la DLQ.

El long poll usa `WaitTimeSeconds=1`. Es long polling, y un mensaje que ya está en la cola vuelve en ese receive. Una espera de 20 s llega tarde a los checks locales cuando el emulador no corta la espera al aparecer el mensaje.

`ApproximateReceiveCount` en LocalStack sube también en polls que el cliente no llega a ver. Mientras la fase en Redis sigue en `queued`, el intento del cuerpo (1) manda. Al arrancar, el compute plane suelta los slots de su propio hostname: son de un proceso que ya no existe y, si se quedan, el reintento espera hasta que vence el lease.

Un fallo reintentable no borra el mensaje: `ChangeMessageVisibility` lo esconde durante el backoff. La `RedrivePolicy` de la cola (`maxReceiveCount` = `NIMBUS_ASYNC_MAX_ATTEMPTS`, los dos en 3 en el compose) lo mueve a `nimbus-async-dlq` cuando el receive count la supera. El proceso no copia el mensaje a mano. El error, los intentos y `durationMs` quedan en Redis (`nimbus:async:dlqmeta:{id}`) porque el cuerpo de SQS no se puede actualizar. `GET /dlq` lee la DLQ y le pega esos campos. `POST /dlq/:id/redrive` borra ese mensaje y manda uno nuevo a `nimbus-async-jobs` con `attempt` 1.

`maxReceiveCount` se fija al crear la cola. Cambiar `NIMBUS_ASYNC_MAX_ATTEMPTS` después no lo actualiza solo: hay que volver a correr el init (`scripts/localstack-init.sh` lo deja alineado).

El estado, el lock, la deduplicación y el stream de resultados siguen en Redis.

## Contrato

Todo lo nuevo es opcional. `POST /functions/:id/invoke` sin flags sigue respondiendo **200** con el mismo JSON de antes, cuando la ejecución termina.

Es async si pasa alguna de estas:

- query `async=true`, `async=1` o `async=yes`
- header `X-Invocation-Type: Event`

`X-Invocation-Type: RequestResponse`, o no mandar nada, es sync.

Respuesta **202**:

```json
{
  "invocationId": "uuid",
  "requestId": "uuid",
  "executionId": "uuid",
  "status": "PENDING",
  "logsUrl": "/invocations/{invocationId}/logs"
}
```

El 202 sale cuando Redis aceptó el job, no cuando el handler terminó. `GET /invocations/:id` muestra el avance:

| status | cuándo |
|--------|--------|
| `PENDING` | aceptada, todavía no arrancó este intento |
| `RUNNING` | el worker tomó el intento. `attempts` es el número de intento (1 = el primero) |
| `SUCCESS` | el handler devolvió valor |
| `ERROR` | falló y no quedan reintentos, o la config era inválida |
| `TIMEOUT` | el último intento venció el timeout y no quedan reintentos |

`attempts` en esa respuesta es la columna nueva, nullable. Las filas viejas quedan en `null`. Un sync que termina queda en `1`.

Una config inválida (timeout, memoria, CPU, handler, env) responde **400** en el momento del enqueue, igual que `POST /executions`. No entra a la cola y no va a la DLQ. La fila queda en `ERROR`.

Go expone la cola en `POST /async/jobs` con el mismo body que `POST /executions` más `invocationId`. Responde **202** `{ invocationId, executionId, status: "queued" }`. Repetir el mismo `invocationId` no encola dos veces.

Si el request trae `traceparent`, el job lo guarda y el worker sigue en esa traza. Sin el header el intento abre una traza propia. El detalle está en [`observabilidad.md`](observabilidad.md).

## Qué se reintenta

| Resultado | Reintento |
|-----------|-----------|
| Handler que lanza, exit distinto de 0, OOM | sí |
| Timeout (exit 124) | sí |
| 429 de la cola de concurrencia, o el worker se cayó a mitad | sí |
| 400 de validación, o cancelación explícita (409), esté en cola o ya corriendo | no, y no entra a la DLQ |

`NIMBUS_ASYNC_MAX_ATTEMPTS` cuenta el primer intento. El default es **3**: un intento y dos reintentos, como Lambda. El tiempo de espera después del intento `n` es `min(max, base * 2^(n-1))` más un jitter aleatorio entre 0 y `NIMBUS_ASYNC_BACKOFF_JITTER_MS`.

## DLQ

Cuando se acaban los intentos, Go guarda la entrada en el hash `nimbus:async:dlq:items` y la agrega al stream `nimbus:async:dlq`. Trae el payload, el último error, el exit code, los intentos y los timestamps de cada arranque (`attemptTimes`), más `enqueuedAt`, `failedAt` y `deadAt`.

| Método | Ruta | Qué hace |
|--------|------|----------|
| GET | `/dlq` | Lista las que siguen en la DLQ (`entries`, máximo 200, las más nuevas primero) |
| POST | `/dlq/:id/redrive` | Saca una y la vuelve a encolar como intento 1. `:id` es el `invocationId` |

El redrive responde 200 con `{ id, invocationId, executionId, status: "queued" }`. Si no está, 404. Nest pone la fila otra vez en `PENDING` y el worker sigue. Un id que no existe no se inventa.

Estos endpoints están en el compute plane (`:8080`), no en Nest.

## Cancelar un async

`POST /executions/:id/cancel` usa el `executionId` que devolvió el 202. Desde el enqueue existe el hash `nimbus:exec:{executionId}` con estado `pending`, así que cancelar antes de que un worker lo tome no responde 404.

- Si todavía no arrancó (está en el stream, en el backoff o en la cola FIFO), el intento termina en `ERROR` con el texto de cancelación y no entra a la DLQ.
- Si ya está corriendo, el compute plane mata el container y no lo devuelve al pool warm. El intento termina en `ERROR` con `"Ejecución cancelada"`, `exitCode` 130, y no se reintenta ni entra a la DLQ. `durationMs` es el tiempo hasta el corte.

`InvocationStatus` en Prisma no tiene `CANCELLED`. Se usa `ERROR` con ese mensaje. El consumidor de Nest ya aplica un resultado `ERROR`; no hay cambio de código en el control plane.

## Si se cae el proceso

- El slot de concurrencia vence solo (`NIMBUS_SLOT_LEASE_SEC`), igual que antes. El reintento usa el mismo `executionId`, así que si el slot sigue tomado lo recupera en lugar de pedir otro.
- El mensaje sin `XACK` lo reclama el consumer group. El intento que quedó a medias cuenta como gastado.
- Un container warm huérfano se borra al arrancar (`nimbus.warm=1`), antes de que los workers acepten trabajo.
- Dos workers no ejecutan la misma invocación juntos mientras el lock vive. Un container que el proceso muerto dejó corriendo puede solaparse unos segundos con el intento nuevo: ya no hay nadie leyendo su resultado. Es la misma ventana del lease de los slots.

Los logs de todos los intentos van al mismo stream `nimbus:logs:{executionId}`. Un intento que llegó a escribir `_end` puede aparecer antes que las líneas del siguiente. El estado de verdad está en la fila, no en ese marcador.

## Variables (compute plane)

| Variable | Default | Efecto |
|----------|---------|--------|
| `NIMBUS_ASYNC_BACKEND` | `redis` en el proceso, `sqs` en el compose | `sqs` usa SQS. `redis` o vacío usa el stream. Otro valor corta el arranque. |
| `SQS_ENDPOINT` | vacío | Base URL. Vacío: Amazon SQS de la región. `http://localstack:4566` en el compose. No usa `S3_ENDPOINT`. |
| `SQS_QUEUE_URL` | se resuelve por `SQS_QUEUE_NAME` | URL de `nimbus-async-jobs`. |
| `SQS_DLQ_URL` | se resuelve por `SQS_DLQ_NAME` | URL de `nimbus-async-dlq`. |
| `NIMBUS_ASYNC_ENABLED` | activo | `0`, `false`, `off` o `no` no arranca workers. Encolar sigue respondiendo 202 y el job espera. |
| `NIMBUS_ASYNC_MAX_ATTEMPTS` | `3` | Intentos totales. Mínimo 1. |
| `NIMBUS_ASYNC_BACKOFF_BASE_MS` | `500` | Espera después del primer fallo. |
| `NIMBUS_ASYNC_BACKOFF_MAX_MS` | `30000` | Tope del backoff. |
| `NIMBUS_ASYNC_BACKOFF_JITTER_MS` | `250` | Se suma un valor aleatorio entre 0 y este número. |
| `NIMBUS_ASYNC_CLAIM_IDLE_MS` | `20000` | En Redis, idle mínimo para `XAUTOCLAIM`. En SQS, visibilidad del mensaje mientras un worker lo tiene (en segundos, hacia arriba). Tiene que cubrir el tiempo en el que un worker vivo todavía no renovó, si no otro receive lo toma. El lock evita ejecutarlo dos veces. |
| `NIMBUS_ASYNC_LOCK_MS` | `15000` | Vida del lock. Se renueva mientras el intento corre. |
| `NIMBUS_ASYNC_WORKERS` | `4` | Goroutines consumidoras en este proceso. |

El control plane no tiene variables nuevas: usa el mismo `REDIS_URL` y `COMPUTE_PLANE_URL`.

## Cambios en el control plane

Chicos, para que la fila termine con el resultado. Federico sigue siendo el dueño de Nest y Prisma.

- `POST /functions/:id/invoke` entiende el query y el header. El 202 no espera a Go más que el enqueue.
- El sync marca `RUNNING` antes de llamar a Go, no después de que vuelve.
- Columna opcional `invocations.attempts` (migración `0003_invocation_attempts`). Sin default: las filas viejas quedan en NULL.
- `GET /invocations/:id` incluye `attempts`.
- `AsyncResultConsumer` lee `nimbus:async:results` y actualiza la fila. No hay endpoint interno nuevo.

## Prueba

Con el stack arriba (`make up` de esta versión):

```bash
make async-check
```

Eso recrea el compute plane con backoff de 400 ms y claim de 1,5 s, y comprueba:

- el 202 vuelve en menos de un segundo y después `GET /invocations/:id` está en `SUCCESS` con el output
- el header `X-Invocation-Type: Event` también
- el invoke sin flags sigue en 200
- un 400 de config no entra a la DLQ
- un handler que siempre lanza termina en `ERROR` con 3 intentos y en `GET /dlq`, con los timestamps de cada arranque separados por el backoff
- `POST /dlq/:id/redrive` la saca y, como el handler sigue fallando, vuelve a la DLQ
- matar el compute plane a mitad de un sleep de 3 s (entra en el timeout de 5 s): al reiniciar, el mensaje se reclama y la fila llega a `SUCCESS` con `attempts >= 2`

Al salir restaura el compute plane del compose.
