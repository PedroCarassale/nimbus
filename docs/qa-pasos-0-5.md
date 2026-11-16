# QA de los pasos 0 a 5

Pasada sobre el stack integrado de Pedro (PRs #9, #10, #12, #13, #14 y #15, sin mergear), con la rama `cursor/paso-5-observabilidad-fe05` como base. Esta rama agrega las correcciones que salieron de la pasada y el target `make qa`.

El stack se levantó limpio (`make down-clean`, `make up`, `make obs`) con Docker arrancado a mano en la VM. El bridge de Compose necesitaba `iptables-legacy -P FORWARD ACCEPT` y `iptables -P FORWARD ACCEPT`. Sin eso, un container no sale ni a la red de usuario que sí usa el runtime: el runtime igual va con `--network=none`.

## Suite oficial, dos veces seguidas

Corrida el 2026-11-16 sobre las imágenes de Paso 5, antes de los arreglos de esta rama. Las dos vueltas terminaron sin ningún `*_FAIL`.

| Check | Vuelta 1 (18:39:23Z–18:41:26Z) | Vuelta 2 (18:41:26Z–18:43:34Z) |
| --- | --- | --- |
| `go test -race ./...` | ok en todos los paquetes | ok en todos los paquetes |
| `npm run build` (Nest y consola) | ok | ok |
| `make hello` | SUCCESS, 204 ms | SUCCESS, 157 ms |
| `make smoke` | PASSED. Invoke 176 ms (`8284c42f-cc18-4e4e-afd1-83d46dcf658c`). Error de handler → ERROR. Timeout → TIMEOUT | PASSED. Invoke 160 ms (`b688d5a2-bd84-4875-844b-6adf9db027b7`) |
| `make load` | 6 invokes, sleep 2 s, concurrencia 2, cola 3. HTTP 200 en 5 (wall 4372, 2270, 4213, 6304 y 2125 ms; cola 2249, 0, 2093, 4169 y 0 ms) y un 429 a los 77 ms | 5 veces 200 (wall 4297, 2114, 4198, 2232 y 6279 ms) y un 429 a los 68 ms |
| `make config-check` | 400 en timeout 301, memoria 8, cpus 9, cpus 0.05 y handler `..`. Timeout 2 s → 504 / exit 124. OOM → exit 137 | misma forma, sin fallo del script |
| `make warm-check` | mediana cold 178 ms, warm 48 ms, init 125 ms. Tras idle, el siguiente es cold. Timeout en warm: 504, exit 124, 2065 ms, `coldStart false`. OOM 128 MB pidiendo 512: exit 137. El siguiente es cold, n=1 | mediana cold 146 ms, warm 37.5 ms, init 105 ms. Primera con descarga 156 ms |
| `make async-check` | 202 y luego SUCCESS. El header `Event` también. El handler que falla entra a la DLQ al tercer intento. Backoff 781 ms y 1005 ms. `durationMs` del resultado en la DLQ salió `null`. SIGKILL del compute: volvió en el reclaim y terminó SUCCESS | backoff 779 ms y 1005 ms. Otra vez `durationMs: null` en la DLQ. El script igual dio ok |
| `make obs-check` | `obs-check listo` | `obs-check listo` |

`obs-check` no está en CI. Nest no tiene script de tests: CI y esta pasada solo compilan.

## Después de los arreglos

`go test -count=1 -race ./...` (19:11Z, sin cache): ok. async 1.067 s, logs 1.102 s, metrics 1.045 s, minio 1.012 s, trace 1.013 s, warm 1.016 s. executor ~3 s por el zip de 32 MiB del test nuevo.

`make smoke` sobre las imágenes nuevas: PASSED. Invoke 201 ms, requestId `1e42f5a6-88a7-4c14-bde9-7aecf4801d10`. Error de handler → ERROR. Timeout → TIMEOUT.

`make async-check` sobre las imágenes nuevas:

- SUCCESS attempts=1, `durationMs` 190, output `async-Nimbus`.
- Header `Event`: SUCCESS, `durationMs` 42.
- Handler que siempre falla: la fila quedó con `durationMs` 44 (antes el campo de la DLQ salía `null`).
- Redrive: salió de la DLQ y volvió (`RUNNING,ERROR`) después de 3 intentos.
- SIGKILL: el compute volvió en 1313 ms. SUCCESS attempts=2, `durationMs` 3164.

## `make qa` (2026-11-16T19:08:47Z)

Log `/tmp/nimbus-qa/qa-20261116-190847.log`. **26 PASS, 0 FAIL.** Una corrida anterior del mismo script se trabó en un `wait` sin pid (esperaba al `tee` del log) y otra marcó dos asserts que no medían lo que parecían: Node es PID 1 en cada container, y los async que se reintentan después de un restart arman un pool nuevo. Esos asserts se corrigieron. El fallo de deploy de v2 de esa corrida sí era un bug del producto y está abajo.

| Prueba | Resultado |
| --- | --- |
| Health de la consola | `healthy` (wget a `127.0.0.1:80`) |
| curl: create, deploy, invoke `{"name":"Curl"}` | SUCCESS 205 ms, mensaje `¡Hola, Curl!`. SSE `follow=false` trae `event: end` |
| Consola en Chrome headless | Crear `qa-ui-20261116-190847`, zip, deploy `v20261116-190902315-3f6z`, invoke. Estado SUCCESS 197 ms, output `¡Hola, Consola!`, panel de logs en `Finalizado` |
| Mismo env, dos invokes | cold n=1 y después warm n=2, `coldStart false` |
| Deploy v2 con el warm de v1 vivo | v2 cold, `mark=v2`, n=1. v1 otra vez warm, n=3, `coldStart false` |
| Env distinto (`GREETING` beta) | cold, n=1. No sigue el contador de alpha |
| Async con cola de concurrencia en 0 | 202, luego ERROR attempts=3, `Cola llena para esta función`, una entrada en la DLQ |
| Async con cola de 4 y un holder de 3 s | SUCCESS attempts=1, `slept: 10` |
| Cancel de un sync que espera en la FIFO | `cancelled: true`. El POST que esperaba trae `cancelada mientras esperaba` |
| Cancel de un sync que ya corre (sleep 2500 ms) | `cancelled: true` y el handler igual termina SUCCESS, `durationMs` 2547 |
| Cancel de un async antes de que el handler arranque | HTTP 200, `cancelled: true`. La fila termina ERROR con el texto de cancelación (attempts=2: el primer intento gastó un 429 y el siguiente vio el flag) |
| Timeout y OOM en warm | HTTP 504 exit 124, `coldStart false`. OOM HTTP 200 exit 137. El siguiente es cold, n=1 |
| `NIMBUS_MAX_CONTAINERS=3` | pico 3 containers de usuario, 3 respuestas 200 y 5 respuestas 429 |
| 80 invokes en 4 funciones (16 async) | HTTP 200×64 y 202×16. Los 80 terminaron SUCCESS. `nimbus_invocations_total` subió 80. Slots de la máquina actual: 0. Warm vivos: 8. Ningún container de usuario en exited |
| Restart de redis, compute y control con 8 async de 2.5 s en vuelo | Los 8 ids warm de antes del restart desaparecieron. Los 8 jobs terminaron SUCCESS. Quedó 1 warm nuevo |
| Aislamiento | uid 1000. `/var/task` y `/etc` EROFS. `/tmp` escribible (tmpfs). Red `ENETUNREACH`. El valor de `GREETING` no aparece en el log del compute |
| Zip malicioso | `../` → `illegal file path`, sin archivo afuera. Symlink → `el zip contiene un symlink: escape`. 40 MiB descomprimidos (zip de 41016 bytes) → `supera 33554432 bytes` |
| Body | JSON inválido → 400 `JSON inválido`. Body ~1.2 MB → 400 `El cuerpo de la solicitud supera 1 MB` |
| Compatibilidad | `POST /executions` mínimo → 200. `NIMBUS_WARM=0` → SUCCESS, `coldStart true`, warm containers 0→0. `NIMBUS_TRACING=0` → 200 |

## Bugs que salieron y cómo quedaron

1. **Healthcheck de la consola.** `wget --spider http://localhost:80` resuelve `localhost` a `::1` y nginx escucha solo IPv4. El container quedaba `unhealthy` para siempre aunque el puerto 8088 respondiera 200 desde el host. El chequeo ahora pega a `http://127.0.0.1:80`. No se agregó `listen [::]:80`.

2. **Zip sin tope, descomprimido en el host.** Un zip de ~41 KB con 40 MiB de ceros se extrajo entero bajo `/tmp/nimbus-work` y el invoke siguió. Un symlink dentro del zip se guardaba como archivo regular. El path `../` ya se rechazaba. Ahora el unzip corta en 32 MiB descomprimidos, 4096 entradas y cualquier symlink. Tests en `executor_test.go`.

3. **Cancel de un async aceptado (202) antes de que un worker lo admita.** `POST /executions/:id/cancel` respondía 404 y el job corría igual, porque el hash `nimbus:exec:{id}` recién nacía en el admit. El `XADD`, el retry y el redrive ahora crean ese hash en `pending` (TTL 24 h). Si el flag ya está, el admit devuelve cancelado y no llama a `Execute`. El 409 es fatal: no reintenta y no va a la DLQ. Test `TestCancelPendingAsyncDoesNotRun`.

4. **`durationMs` desaparecía en la DLQ.** El campo tiene `omitempty` y `dead()` no copiaba `RunResult.DurationMs`, así que un 0 se iba y un valor real también. Las dos vueltas de `async-check` lo mostraron como `null`. Ahora se copia. En la corrida posterior el boom quedó con `durationMs` 44.

5. **Dos deploys de la misma función en el mismo segundo.** El tag era `YYYYMMDD-HHMMSS` y hay un unique `(function_id, version)`. El segundo deploy hacía `PutObject` sobre la misma key de MinIO (pisaba el zip de la versión que ya estaba en Postgres) y después Prisma devolvía 500. El tag ahora lleva milisegundos y cuatro caracteres aleatorios (`20261116-190902315-3f6z`). Si igual choca, se reintenta con otro tag antes de dejar el 500.

6. **El panel de logs marcaba error al cerrar el SSE.** `onerror` leía un `status` viejo (`connecting`) y, después del evento `end`, mostraba "Conexión perdida". Un ref corta ese camino cuando el stream ya cerró bien. La corrida de Chrome llegó a `Finalizado` con el saludo y sin ese texto.

## Lo que queda abierto

- Cancelar una ejecución que ya está corriendo solo pone el flag. El container sigue hasta que el handler termina o vence el timeout. En esta pasada, un sleep de 2500 ms cancelado a los ~400 ms igual devolvió SUCCESS a los 2547 ms. Severidad media. Está escrito en `docs/concurrencia.md` y `docs/async.md`.
- Con `NIMBUS_WARM=0` el proceso es el usuario de la imagen (root). El camino warm corre como uid 1000 (`node`). Esta pasada midió el uid del camino warm. Severidad baja. Es el corte del paso 3.
- Mientras el container vive, `docker inspect` muestra los env. El log del compute los redacta: en `make qa` el valor de `GREETING` no estaba en `docker logs nimbus-compute-plane`. Si el handler los imprime, salen en el log del usuario.
- Un `SIGKILL` del compute deja miembros en `nimbus:slots:machine:{hostname-muerto}` y en el sorted set de la función. El score queda en el pasado (no otorgan cupo) y el host actual quedó en `ZCARD 0`. Nadie barre las keys de hostnames que ya no existen. Severidad baja.
- Multer no limita el tamaño del upload. El tope de 32 MiB corta la bomba al descomprimir, no en el body de Nest. Un zip enorme en memoria del control plane sigue siendo posible. Severidad baja.
- Nest no reenvía `envVars`, `cpus` ni `coldStart` al compute. `getRemainingTimeInMillis` sigue en 0. Los intentos async comparten `executionId` y el stream de logs. Un 429 gasta un intento async. No hay auth. Nada de eso se cambió en esta pasada.

## `/dev/kvm`

```
crw-rw---- 1 root docker 10, 232 /dev/kvm
```

`open` O_RDWR como `ubuntu` devuelve `EACCES` (13). Como root, el open funciona. El CPU tiene `vmx`, `ept` y `vpid`. `kvm-ok` no está instalado. Firecracker es plausible en esta VM para root. No se implementó.

## Cómo repetirlo

```bash
make up
make obs
make qa
```

`make qa` corre `scripts/qa-deep.sh`. Usa el Chrome del sistema (`/usr/local/bin/google-chrome`) y baja `puppeteer-core` a `/tmp/nimbus-qa` si no está. Al salir recrea compute y control plane con los defaults del compose.
