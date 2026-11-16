# Concurrencia y cola (Paso 1)

El compute plane admite varias ejecuciones de la misma función al mismo tiempo y, cuando llega al tope, las siguientes esperan turno en Redis. Si la cola se llena, la invocación se rechaza enseguida.

## Variables de entorno (compute plane)

| Variable | Default | Descripción |
|----------|---------|-------------|
| `NIMBUS_FN_CONCURRENCY` | `2` | Ejecuciones simultáneas por función. Un valor menor a 1 se ignora y queda el default. |
| `NIMBUS_FN_QUEUE_MAX` | `32` | Cuántas invocaciones pueden esperar por función. `0` desactiva la espera: al llegar al tope se responde 429. |
| `NIMBUS_FN_QUEUE_WAIT_SEC` | `30` | Tiempo máximo en la cola. Si se vence, la invocación sale de la cola y el compute plane responde 429. |
| `NIMBUS_MAX_CONTAINERS` | `8` | Tope de ejecuciones vivas en este proceso de compute (una máquina en el compose actual). |
| `NIMBUS_SLOT_LEASE_SEC` | `30` | Vigencia de un slot sin renovación. El proceso lo renueva mientras la ejecución sigue viva. |

El control plane no manda estos límites. Solo reenvía el body de `POST /executions` y traduce el HTTP 429.

## Contrato `POST /executions`

Compatible hacia atrás: los campos nuevos son opcionales.

```json
{
  "executionId": "uuid",
  "functionId": "uuid",
  "version": "20240115-120000",
  "event": {},
  "timeoutSec": 5,
  "memoryMb": 128,
  "cpus": 0.5,
  "handler": "handler",
  "envVars": { "GREETING": "hola" },
  "maxConcurrency": 2
}
```

`timeoutSec`, `memoryMb`, `cpus`, `handler` y `envVars` son opcionales. Los límites y el formato del handler están en [`configuracion.md`](configuracion.md). Un valor inválido responde 400 y no entra a la cola.

`maxConcurrency` (opcional, entero positivo) reemplaza `NIMBUS_FN_CONCURRENCY` **solo para esa admisión**. Si no viene, o vale 0 o menos, se usa el default del entorno. El tope global `NIMBUS_MAX_CONTAINERS` sigue aplicando. Dos invocaciones en vuelo con valores distintos admiten hasta el mayor de los dos, nunca por encima del tope global.

Respuesta de una ejecución que entró (el campo `queuedMs` es nuevo; `0` si no esperó):

```json
{
  "executionId": "uuid",
  "success": true,
  "output": {},
  "error": "",
  "exitCode": 0,
  "durationMs": 2100,
  "queuedMs": 0
}
```

`durationMs` mide el trabajo después de conseguir el slot (descarga, arranque del container, handler). `queuedMs` mide solo la espera en la cola.

### Cola llena o espera agotada → HTTP 429

El compute plane responde **429** con JSON. No encola la invocación que no entra.

Cola llena (no esperó):

```json
{
  "error": "Cola llena para esta función",
  "executionId": "uuid",
  "retryAfter": 5,
  "reason": "queue_full",
  "queuedMs": 0
}
```

Se agotó `NIMBUS_FN_QUEUE_WAIT_SEC` (sí esperó, después se fue de la cola):

```json
{
  "error": "Tiempo máximo de espera en cola excedido",
  "executionId": "uuid",
  "retryAfter": 5,
  "reason": "queue_timeout",
  "queuedMs": 30000
}
```

`retryAfter` es una pista fija de 5 segundos, el mismo campo de antes. También va el header `Retry-After: 5`.

El control plane (`POST /functions/:id/invoke`) traduce ese 429 a **429 Too Many Requests** para el cliente. El body queda así:

```json
{
  "statusCode": 429,
  "error": "Too Many Requests",
  "message": "Cola llena para esta función",
  "invocationId": "uuid",
  "requestId": "uuid",
  "executionId": "uuid",
  "queuedMs": 0
}
```

La invocación queda guardada en Postgres con estado `ERROR` y ese mensaje. El cambio en el control plane es solo el cliente HTTP (`runner.service.ts`) y el `invoke` del controller.

Si la ejecución se cancela mientras espera, el compute plane responde **409** con `"error": "Ejecución cancelada mientras esperaba en la cola"`. El invoke del control plane sigue siendo síncrono y no expone el `executionId` hasta que termina, así que esa cancelación se usa contra el compute plane directo.

## Cancelar

`POST /executions/:id/cancel` sirve para una ejecución en cola y para una que ya corre:

- **En cola:** sale de la cola en el momento y no arranca. El `POST /executions` que estaba bloqueado responde 409.
- **En curso:** se marca `cancelled` en Redis. El slot sigue ocupado hasta que el proceso suelta la ejecución, para no entregar ese cupo dos veces. Hoy el container no se mata por ese flag.

Cancelar un id que no está en cola ni en curso responde 404.

## Cómo se guarda en Redis

Por función:

- `nimbus:slots:fn:{functionId}` — sorted set. El member es el `executionId` y el score es el vencimiento del lease (unix ms).
- `nimbus:queue:fn:{functionId}` — sorted set FIFO. El score es un ticket (`INCR`), así el orden no depende del reloj.
- `nimbus:queue:seq:fn:{functionId}` — secuencia de tickets.

Por proceso de compute (el hostname del container; en el compose es estable por `container_name`):

- `nimbus:slots:machine:{hostname}` — mismas leases, cuenta contra `NIMBUS_MAX_CONTAINERS`.

Por ejecución:

- `nimbus:exec:{executionId}` — hash con `functionId`, `status` (`pending` / `queued` / `running` / `cancelled`), `heartbeat` y `cancelled`.

La admisión es un script Lua: limpia leases vencidos, respeta el FIFO (solo la cabeza de la cola puede tomar un slot si hay gente esperando) y encola o rechaza en el mismo paso. No hay un lock de “una sola ejecución” como el `SET NX` anterior (`nimbus:lock:fn:{functionId}` ya no se usa).

## Si el proceso se cae

Un slot lo renueva el proceso cada un tercio del lease. Si el proceso muere, deja de renovar. La próxima admisión (o la renovación de otra ejecución viva) borra los scores ya vencidos y ese cupo, también el del tope global, vuelve a estar libre. La ventana es `NIMBUS_SLOT_LEASE_SEC` (default 30s).

Quien espera en la cola refresca `heartbeat` en cada intento. Si deja de hacerlo durante `NIMBUS_FN_QUEUE_WAIT_SEC`, la próxima admisión lo saca de la cola. Una cabeza muerta no bloquea a los demás para siempre.

El tope global cuenta ejecuciones que ya tienen slot, no hace `docker ps`. El slot se toma antes de `docker run` y se suelta al terminar. Un container huérfano puede seguir existiendo en el daemon después de que el lease venció; el cupo de Redis igual se recupera.

## Prueba de carga

Con el stack arriba:

```bash
make load
```

Eso recrea el compute plane con concurrencia 2 y cola de 3, sube el timeout del control plane a 20s, despliega `examples/slow` y dispara 6 invokes de un sleep de 2s. Después restaura los defaults del compose. El script imprime, por cada invoke, el timestamp de envío, el de respuesta, `queuedMs` y el `startedAt` del handler.
