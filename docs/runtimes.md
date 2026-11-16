# Runtimes (Paso 6)

El compute plane puede arrancar tres imágenes. El campo `runtime` de `POST /executions` elige cuál. Si no viene, sigue `nodejs20`: un cliente viejo no cambia de comportamiento.

| `runtime` | Imagen | Qué hay en el zip |
|-----------|--------|-------------------|
| `nodejs20` | `nimbus-node` | JavaScript. Handler `archivo.export` (default `handler` → `/var/task/index.js`, export `handler`) |
| `python3.12` | `nimbus-python` | Python. Handler `archivo.funcion` (`index.handler` → `/var/task/index.py`, función `handler`) |
| `go1.x` | `nimbus-go` | Un binario Linux estático. Sin handler, el archivo es `/var/task/bootstrap` |

Otro valor responde **400** con `"field": "runtime"` y no ocupa un slot.

`make up` construye las tres imágenes. `make hello` sigue en Node. `make hello-python`, `make hello-go` y `make runtime-check` cubren los otros dos.

## Contrato del bootstrap

Cada imagen trae un bootstrap propio. El código del usuario se monta en `/var/task` (solo lectura) y no implementa el protocolo: lo habla el bootstrap.

Sin `NIMBUS_WARM` (camino frío y `scripts/run-zip.sh` para Node): lee el evento de `NIMBUS_EVENT_PATH`, llama al handler una vez, imprime el JSON del resultado y sale. Exit 0 es éxito. Otro exit es error y el texto queda en stderr.

Con `NIMBUS_WARM=1` el proceso queda vivo. Avisa `{"type":"ready","initMs":N}` y después lee una línea JSON por stdin:

```json
{"type":"invoke","executionId":"…","event":{},"traceId":"…","traceparent":"…"}
```

Responde por stdout, una línea por frame:

```json
{"type":"log","stream":"stdout","executionId":"…","line":"hola"}
{"type":"done","executionId":"…","ok":true,"output":{},"error":"","exitCode":0}
```

`traceId` y `traceparent` de esa línea se publican en `NIMBUS_TRACE_ID` y `TRACEPARENT` antes de llamar al handler. Los logs del usuario no viajan como JSON suelto: el bootstrap los convierte en frames `log`.

El container, en los tres runtimes, lleva timeout, memoria, `--cpus` si hay cuota, `envVars`, `--network=none`, `--read-only`, `--pids-limit=256` y el tmpfs de `/tmp` cuando el proceso no es root. El warm siempre corre como el usuario `node` (uid 1000) de la imagen. Python y Go también en frío. Node en frío sigue como root: es el contrato anterior.

La clave del pool warm incluye el runtime. Un container de Node no atiende un invoke Python de la misma función.

## Python

La función es normal, no una corrutina:

```python
def handler(event, context):
    print("hola")
    return {"message": "Hola, %s" % event.get("name", "mundo")}
```

`context.memoryLimitInMB` (y `memory_limit_in_mb`) sale de `NIMBUS_MEMORY_MB`. `get_remaining_time_in_millis()` devuelve 0, igual que en Node. El ejemplo está en `examples/hello-python`.

La imagen es `python:3.12-alpine` con `PYTHONDONTWRITEBYTECODE=1`, así el import no intenta escribir `.pyc` en un filesystem de solo lectura.

## Go

El zip lleva un binario Linux estático, no el fuente. El container no tiene red ni toolchain, así que no se puede hacer `go build` adentro ni bajar módulos. Compilar afuera y empaquetar el binario es el camino corto y el que no depende de la red en el momento del invoke.

```bash
cd examples/hello-go
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags='-s -w' -o bootstrap .
zip hello-go.zip bootstrap
```

`CGO_ENABLED=0` deja el binario estático. La imagen es Alpine (musl): un binario dinámico contra glibc no arranca. `GOARCH` tiene que ser la del daemon (`amd64` en esta VM).

El bootstrap de la plataforma ejecuta ese binario en cada invoke:

- stdin: el evento, un solo JSON
- stdout: el resultado, un solo JSON
- stderr: logs
- exit 0: éxito. Cualquier otro exit es error y stderr es el mensaje

El binario de ejemplo también lee `NIMBUS_EVENT_PATH` si stdin viene vacío, para poder probarlo fuera de Docker. Dentro de la plataforma stdin siempre trae el evento.

En warm el container se reutiliza, pero cada invoke hace un `exec` del binario. No queda estado de proceso entre llamadas (a diferencia de Node y Python, que importan el módulo una vez). `initMs` de Go es el tiempo de comprobar que el binario existe, no el de cargarlo.

Si el binario muere por `SIGKILL` (el cgroup de memoria), el bootstrap termina con exit 137 y el container no vuelve al pool. Hace falta escribir las páginas: un `make` de ceros puede quedarse en la zero-page del kernel y no llegar al límite. El ejemplo escribe un byte por página.

El unzip del compute plane deja los archivos en `0644` para que un bit setuid del zip no llegue al host. Antes de `docker run`, solo para `go1.x`, se marca el binario como `0755` en el directorio de trabajo del host. El mount sigue siendo de solo lectura.

Si el request no trae `handler`, Go usa `bootstrap`. Si lo trae, es la ruta relativa del binario dentro del zip (`bin/hello` → `/var/task/bin/hello`).

## Métrica

`nimbus_invocations_total` suma la etiqueta `runtime` con esos tres valores. Un runtime vacío se anota como `nodejs20`. Las otras series no la llevan: la cardinalidad queda en tres series extra por función, status y mode, no en una serie por ejecución.

## Qué tiene que mandar Nest

No hay cambio de Nest ni de Prisma en este paso. `Function` no tiene columna `runtime` y `runner.service.ts` no arma ese campo. El invoke de la consola y `make hello` siguen en Node.

Para Python o Go hoy:

1. Crear la función y desplegar el zip por el control plane (`POST /functions`, `POST /functions/:id/deploy`). El zip no dice el runtime.
2. Llamar al compute plane directo:

```json
POST /executions
{
  "executionId": "…",
  "functionId": "…",
  "version": "…",
  "runtime": "python3.12",
  "handler": "index.handler",
  "event": { "name": "Ada" }
}
```

Go es igual con `"runtime": "go1.x"`. El `handler` se puede omitir.

Cuando el control plane lo sume, hace falta:

- Guardar `runtime` en la función (string, default `nodejs20`). Validar el DTO contra `nodejs20`, `python3.12` y `go1.x`. No hace falta un enum de Prisma.
- Aceptarlo en el alta o en una actualización de la función.
- Reenviarlo en el JSON de `POST /executions`, junto con `handler`. Para Go, si el usuario no eligió handler, omitirlo: el compute plane usa `bootstrap`.

`make runtime-check` despliega los dos ejemplos por Nest y los invoca así. También entra en `make qa` y en el job de compose del CI.
