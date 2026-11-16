# Containers warm (Paso 3)

Un invoke ya no tiene que pagar `docker run` cada vez. El compute plane deja vivo el container de una función y le manda el siguiente evento por el mismo proceso. Si el container se cuelga, se queda sin memoria o se rompe, se tira y el siguiente invoke arranca uno nuevo.

## Diseño

El runtime (`runtime-node/bootstrap.js`) tiene dos modos.

Sin `NIMBUS_WARM`, hace lo de antes: lee el evento de un archivo, llama al handler una vez, imprime el JSON y sale. Así siguen `scripts/run-zip.sh` y el camino frío.

Con `NIMBUS_WARM=1` el proceso carga el handler una sola vez y se queda en un loop. Cada invocación entra por stdin como una línea JSON y la respuesta sale por stdout, también en líneas JSON:

```json
{"type":"invoke","executionId":"…","event":{}}
{"type":"log","stream":"stdout","executionId":"…","line":"hola"}
{"type":"done","executionId":"…","ok":true,"output":{},"error":"","exitCode":0}
```

El bootstrap engancha `console.log`, `console.error` y `process.stdout.write` / `process.stderr.write`, y reescribe esas líneas como frames `log`. El resultado del `return` viaja dentro del frame `done`, no como JSON suelto. Así el log de una invocación no se mezcla con el de la siguiente ni con el protocolo.

### Por qué stdin/stdout y no un socket o un HTTP adentro

El container sigue con `--network=none`. Un server HTTP en el loopback del container no se puede alcanzar desde afuera. La alternativa sería `docker exec` por cada invoke, que abre otro proceso y suma latencia. Un socket Unix exige un bind mount compartido entre el compute plane y el container del usuario, con permisos y un archivo en el host. `docker run -i` deja un solo proceso atado a stdin/stdout: sin puerto, sin socket y sin un mount extra además del código.

El límite de ese canal: si el handler escribe directo al fd 1 con `fs.writeSync(1, …)`, se salta el hook y puede romper el frame. En ese caso el compute plane tira el container. `console.log` y `console.error` quedan bien.

## Aislamiento

El container warm usa los mismos límites que el frío, y corre como el usuario `node` de la imagen (el camino frío sigue con el usuario de la imagen, root):

- `--network=none`
- `--read-only`
- `--memory` y `--memory-swap` con el mismo tope
- `--cpus` si hay cuota
- `--pids-limit=256`
- `--user node`
- `--tmpfs /tmp` chico, para que el usuario sin privilegios tenga un directorio escribible (`HOME=/tmp`)

El código queda montado en `/var/task` de solo lectura. El evento no se escribe en el disco: viaja en el frame. El zip de una versión se extrae una vez por proceso, bajo `NIMBUS_WORK_DIR/warm-code`, y se reutiliza el directorio. Al apagar el proceso se borra. Si el proceso murió antes, el arranque limpia ese directorio y los containers con la etiqueta `nimbus.warm=1`.

## Qué container se reutiliza

La clave es función + versión + handler + `memoryMb` + `cpus` + un hash de `envVars`. El timeout no entra: lo aplica Go desde afuera, y el mismo proceso puede atender plazos distintos.

Dos invokes con el mismo código pero distinto `GREETING`, otra memoria o otro handler no comparten container. Las variables quedan en el entorno del proceso desde el `docker run`; no se pueden cambiar sin arrancar otro.

`process.env.NIMBUS_TIMEOUT_SEC` dentro del container es el timeout del invoke que lo creó. Los invokes siguientes se cortan igual en su propio plazo, pero esa variable no se actualiza. `context.getRemainingTimeInMillis` sigue devolviendo 0.

## Pool

| Variable | Default | Efecto |
|----------|---------|--------|
| `NIMBUS_WARM` | activo | `0`, `false`, `off` o `no` vuelven al camino frío: un `docker run` por invoke. |
| `NIMBUS_WARM_IDLE_SEC` | `60` | Un container ocioso se borra después de este tiempo. El barrido corre cada 1s. |
| `NIMBUS_FN_CONCURRENCY` | `2` | Tope de containers warm (ocupados + ociosos) de una función. |
| `NIMBUS_MAX_CONTAINERS` | `8` | Tope de containers warm de este proceso. Los ociosos cuentan. |

Si hace falta un container nuevo y ya se llegó al tope, se echa el ocioso menos usado (LRU). Primero uno de la misma función cuando el tope es el de la función; si el tope es el global, el más viejo de cualquiera. Un container ocupado no se echa.

Si todos los cupos están ocupados y no hay ocioso para sacar, el invoke igual corre: arranca un container, lo usa y lo borra al terminar. No se suma al pool. La admisión de Redis sigue siendo la que decide si el invoke entra.

Al apagar el proceso se borran todos los containers del pool. Al arrancar se borran los que quedaron etiquetados `nimbus.warm=1` de un proceso anterior.

Mientras un invoke corre, el container se renombra a `nimbus-{executionId}` y al terminar vuelve a su nombre del pool. `make config-check` inspecciona ese nombre para ver `--cpus`.

## Qué se descarta

Un container se reutiliza si el handler devuelve un valor o lanza una excepción que el bootstrap atrapa. El proceso sigue vivo.

Se mata y no se reutiliza si:

- se vence el timeout (exit 124, HTTP 504)
- el proceso se cae, llama a `process.exit`, o hay un `uncaughtException` / `unhandledRejection`
- el cgroup lo mata por memoria, o Node aborta por el heap (137 / 134)
- el protocolo se desalinea (una línea que no es JSON, o un frame de otra ejecución)

El siguiente invoke de esa clave es cold start.

## Estado que se filtra

Como en Lambda, el módulo queda cargado. Un `let n = 0` en el archivo, el cache de `require` y lo que el handler deje en `global` sobreviven al siguiente invoke del mismo container. `make warm-check` lo comprueba: `n` sube y el `pid` no cambia entre warms, y vuelve a 1 cuando el container se tira.

Eso es a propósito. No es un aislamiento entre invokes. El aislamiento entre funciones y versiones sigue siendo otro container, con red cortada, disco de solo lectura y límites de memoria y CPU.

## Logs y cancel

Cada línea del frame `log` se escribe en `nimbus:logs:{executionId}` de esa invocación. El bootstrap manda el `executionId` en el frame; si no coincide, el container se descarta y no se escribe en el stream equivocado.

`POST /executions/:id/cancel`, si el invoke todavía está en la cola, lo saca de la cola. Si ya corre, el proceso corta el contexto, hace `docker rm -f` del container (en ese momento se llama `nimbus-{executionId}`) y lo marca tainted. No vuelve al pool: el siguiente invoke de esa clave arranca uno nuevo. El `POST /executions` responde 409 con `success: false` y `"Ejecución cancelada"`. El detalle del exit 130 y de la fila en Postgres está en [`concurrencia.md`](concurrencia.md).

## Contrato

La respuesta de `POST /executions` suma dos campos. Los clientes que ignoran campos desconocidos siguen igual. Un 429 de la cola no los trae, porque ese invoke no llegó a correr.

```json
{
  "executionId": "uuid",
  "success": true,
  "output": {},
  "error": "",
  "exitCode": 0,
  "durationMs": 40,
  "queuedMs": 0,
  "coldStart": false,
  "initMs": 0
}
```

| Campo | Significado |
|-------|-------------|
| `coldStart` | `true` si este invoke arrancó un container nuevo. `false` si reutilizó uno. Con `NIMBUS_WARM` apagado es siempre `true`. |
| `initMs` | Milisegundos desde `docker run` hasta el frame `ready` (arranque del container + carga del handler). `0` en un warm hit. En el camino frío de un solo uso también es `0`: ahí no se separa el arranque del handler. `durationMs` sigue midiendo todo el trabajo después del slot, incluida la descarga del zip la primera vez. |

Nest no reenvía estos campos. `runner.service.ts` no se tocó: el invoke de la consola y de `make hello` no muestran `coldStart`. Para verlo hay que llamar al compute plane (`POST /executions`) o correr `make warm-check`.

## Prueba

Con el stack arriba:

```bash
make warm-check
```

Eso recrea el compute plane con idle corto, despliega un handler con un contador en el módulo y comprueba:

- el primer invoke de cada serie es `coldStart=true` con `initMs > 0`, y los siguientes son warm, con el mismo `pid` y `n` creciente
- la mediana de `durationMs` warm es menor que la cold (el código ya está en disco; la primera invocación, que además descarga el zip, se imprime aparte)
- los logs de dos invokes no se cruzan en Redis
- después del idle, `docker ps` no muestra containers `nimbus.warm=1` y el siguiente invoke es cold
- un timeout sobre un container warm lo borra y el siguiente es cold (`n` vuelve a 1)
- un OOM no se reutiliza: el siguiente también es cold

Al terminar restaura el compute plane del compose.

## Números medidos

Medidos en esta máquina con `make warm-check` (idle 6s, tres series de un cold y cuatro warm, handler que solo incrementa un contador). El cold de la tabla ya no incluye la descarga del zip. `initMs` es solo el arranque del container.

| | ms |
|--|----|
| Primera invocación, con descarga | 163 (`initMs` 113) |
| Mediana cold (`durationMs`) | 166 |
| Mediana warm (`durationMs`) | 41 |
| Mediana `initMs` del cold | 118 |

Muestras de `durationMs` cold: 166, 145, 179. Warm: 41, 51, 41, 41, 41, 44, 52, 42, 41, 41, 40, 43. `initMs` cold: 118, 94, 126.

En esa corrida el warm reutilizó el mismo `pid` y `n` siguió de 1 a 5 en cada serie. Antes del idle había cinco containers con label `nimbus.warm=1`; nueve segundos después `docker ps` no mostraba ninguno y el invoke siguiente volvió a `coldStart=true` (`durationMs` 188, `initMs` 132). El timeout sobre el container reutilizado respondió 504 / exit 124 en 2071 ms y el siguiente fue cold (`n` volvió a 1). El OOM con 128 MB y un pedido de 512 MB salió 137 y el invoke siguiente, con la misma clave, también fue cold.
