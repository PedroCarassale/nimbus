# LocalStack: S3 y SQS en local, AWS sin cambiar código

El compose deja de usar Silo/MinIO. Los artifacts viven en S3 y la cola async en SQS. En la máquina de desarrollo los dos los atiende [LocalStack](https://localstack.cloud/) (`localstack/localstack:4.14.0`, digest `sha256:3ebc37595918b8accb852f8048fef2aff047d465167edd655528065b07bc364a`, solo los servicios `s3` y `sqs`). El mismo binario y el mismo cliente de Nest hablan con Amazon si las variables de endpoint quedan vacías.

Desde LocalStack 2026.03.0 la imagen de Docker Hub pide `LOCALSTACK_AUTH_TOKEN` para arrancar, también si solo se usan S3 y SQS. 4.14.0 es el último release community y arranca sin cuenta. La API que usa Nimbus es la de AWS, así que el pin no cambia el paso a un bucket y unas colas reales.

## Qué corre en el compose

```
Nest  ──PutObject──►  S3 nimbus-artifacts
                         │
                         ▼
Go    ──GetObject──►  functions/{id}/versions/{version}/code.zip

Nest  ──POST /async/jobs──►  Go
                              │  SendMessage + traceparent
                              ▼
                         SQS nimbus-async-jobs
                              │  long poll, ejecuta, borra o backoff
                              │  RedrivePolicy (maxReceiveCount)
                              ▼
                         SQS nimbus-async-dlq
                              │
                              ▼
                         Redis nimbus:async:results  ──►  Nest actualiza Invocation
```

`localstack-init` espera el healthcheck y crea, de forma idempotente:

- el bucket `nimbus-artifacts`
- la cola `nimbus-async-dlq`
- la cola `nimbus-async-jobs`, con `VisibilityTimeout` 30 y `RedrivePolicy` hacia la DLQ

`maxReceiveCount` sale de `NIMBUS_ASYNC_MAX_ATTEMPTS` (3 si no se setea). En 4.14.0 la URL de una cola es:

`http://localstack:4566/queue/us-east-1/000000000000/nimbus-async-jobs`

`http://localstack:4566/queue/us-east-1/000000000000/nimbus-async-dlq`

Si `SQS_QUEUE_URL` o `SQS_DLQ_URL` quedan vacías, el compute plane las pide con `GetQueueUrl` y reescribe el host al de `SQS_ENDPOINT`.

El puerto publicado en el host es `4566`. Credenciales de LocalStack: access key `test`, secret `test`, región `us-east-1`.

Redis sigue guardando locks de concurrencia, logs, el estado del job, la deduplicación, el lock del intento y el stream de resultados. SQS no reemplaza eso. El error y `durationMs` de una entrada de la DLQ también quedan en Redis (`nimbus:async:dlqmeta:{id}`), porque el cuerpo del mensaje no se edita.

## Variables

Las lee el compute plane (Go) y, para S3, el control plane (Nest, clase `MinioService`).

| Variable | LocalStack (compose) | AWS real |
|----------|----------------------|----------|
| `AWS_REGION` | `us-east-1` | la región de la cuenta |
| `AWS_ACCESS_KEY_ID` | `test` | la key, o vacío para la cadena de IAM |
| `AWS_SECRET_ACCESS_KEY` | `test` | el secret, o vacío para la cadena de IAM |
| `S3_BUCKET` | `nimbus-artifacts` | el bucket de artifacts |
| `S3_ENDPOINT` | `http://localstack:4566` | **vacío** |
| `S3_FORCE_PATH_STYLE` | `true` | vacío (el SDK usa virtual-hosted) |
| `SQS_ENDPOINT` | `http://localstack:4566` | **vacío** |
| `SQS_QUEUE_URL` | URL de `nimbus-async-jobs` | la URL que devuelve AWS |
| `SQS_DLQ_URL` | URL de `nimbus-async-dlq` | la URL de la DLQ |
| `NIMBUS_ASYNC_BACKEND` | `sqs` | `sqs` |

`S3_ENDPOINT` vacío: el SDK llama a `s3.<región>.amazonaws.com` y no fuerza path style. Si tampoco hay keys, Go no inyecta credenciales estáticas y usa la cadena por defecto (variables, archivo compartido, rol). Lo mismo hace Nest.

`SQS_ENDPOINT` es independiente de `S3_ENDPOINT`. Se puede dejar S3 en AWS y la cola en otro lado, o al revés.

Si `S3_*` no está, siguen valiendo `MINIO_ENDPOINT`, `MINIO_ACCESS_KEY` / `MINIO_ROOT_USER`, `MINIO_SECRET_KEY` / `MINIO_ROOT_PASSWORD` y `MINIO_BUCKET`. Un endpoint sin esquema recibe `http://`.

`NIMBUS_ASYNC_BACKEND=redis` (o vacío, que es el default del proceso) no abre SQS. Los tests unitarios y un compose viejo siguen por el stream `nimbus:async:jobs`. El compose de este paso setea `sqs`.

## Pasar a AWS sin tocar código

1. Crear el bucket y las dos colas. En la cola de jobs, `RedrivePolicy` con `maxReceiveCount` igual a `NIMBUS_ASYNC_MAX_ATTEMPTS`.
2. En el control plane y en el compute plane: `AWS_REGION`, el bucket en `S3_BUCKET`, y las credenciales o un rol. Dejar `S3_ENDPOINT` y `S3_FORCE_PATH_STYLE` vacíos.
3. En el compute plane: `NIMBUS_ASYNC_BACKEND=sqs`, `SQS_QUEUE_URL` y `SQS_DLQ_URL` con las URLs de AWS. Dejar `SQS_ENDPOINT` vacío.
4. No hace falta cambiar Nest más allá de esas variables. Sigue llamando a `POST /async/jobs`. El resultado sigue llegando por Redis.

Ejemplo de compute plane contra AWS, con rol de instancia y sin keys:

```bash
AWS_REGION=us-east-1 \
S3_BUCKET=nimbus-artifacts-prod \
S3_ENDPOINT= \
SQS_ENDPOINT= \
SQS_QUEUE_URL=https://sqs.us-east-1.amazonaws.com/123456789012/nimbus-async-jobs \
SQS_DLQ_URL=https://sqs.us-east-1.amazonaws.com/123456789012/nimbus-async-dlq \
NIMBUS_ASYNC_BACKEND=sqs
```

## Comprobación

Con el stack arriba:

```bash
make localstack-check
```

El script, vía `awslocal` dentro del contenedor, mira que el zip del deploy esté en el bucket, que un async lento aparezca en la cola de jobs y termine bien, que un handler que falla entre en `nimbus-async-dlq`, que `GET /dlq` traiga payload, error, intentos y duración, y que `POST /dlq/:id/redrive` lo devuelva a la cola de jobs.

`make async-check` sigue cubriendo el contrato de 202, backoff, reclaim y redrive. Con SQS el backoff de 400 ms del script se redondea a 1 s. Los huecos que pide el check (300 ms y 600 ms) se cumplen igual.
