# AI Decision Log — Nimbus Functions

Log de auditoría de decisiones técnicas asistidas por IA.
Formato pedido por la cátedra (AI-DECISIONS.md).

---

## Stack MVP y cortes de arquitectura

**Problema abordado:** Definir las piezas de una plataforma tipo Lambda (control plane, compute, artifacts, runtime, logs) y el stack concreto del MVP sin over-engineering.

**Prompt / Herramienta utilizada:** Conversación de diseño con Grok Bot (nimbus) en Cursor; sin generación de código de plataforma en ese momento. Herramienta: asistente de chat.

**Código / Arquitectura generada:** Documento `docs/stack.md` — NestJS + Postgres + Redis + MinIO + Go/Docker + runtime Node fijo + SSE; front React diferible; Firecracker/auth/billing fuera del MVP.

**Validación y Corrección Humana:** Validado por Pedro: se acordó el stack y el orden (runtime antes que API/DB). Product copy en español. No se aceptó scaffold completo automático al inicio; primero guía arquitectónica.

---

## Runtime Node mínimo (hello en Docker)

**Problema abordado:** Tener el primer contrato ejecutable runtime ↔ aislamiento sin control plane: handler de usuario + bootstrap + imagen Docker + script local.

**Prompt / Herramienta utilizada:** Cloud agent de Cursor sobre `PedroCarassale/nimbus` — pedido de `examples/hello`, `runtime-node/bootstrap.js`, Dockerfile y `scripts/run-hello.sh` con timeout.

**Código / Arquitectura generada:** PR #1 — bootstrap carga handler y event JSON, imprime `{ ok, result|error }`; imagen `node:22-alpine`; script `run-hello.sh` con timeout 5s. Prueba reportada: JSON `hola` con event de muestra.

**Validación y Corrección Humana:** Merge a `main` por pedido de Pedro tras revisar el PR. Alcance acotado (sin Nest/Go/MinIO). Pendiente en slices siguientes: zip real, memoria cgroup, y no asumir que este bootstrap es el protocolo final tipo Lambda Runtime API.

---

## Roadmap hasta el MVP

**Problema abordado:** Ordenar el trabajo restante hasta un MVP demostrable (create → deploy → invoke sync).

**Prompt / Herramienta utilizada:** Grok Bot (nimbus) — armado de roadmap en chat y persistencia en `docs/roadmap.md`.

**Código / Arquitectura generada:** `docs/roadmap.md` con slices 1–7 (runner, artifacts, control plane, compute, logs, DX, consola opcional) y exclusiones explícitas.

**Validación y Corrección Humana:** Pedro pidió guardar el roadmap en el repo. Criterio de done acordado: compose + zip + invoke HTTP + logs sin Docker manual. Ajustable si el alcance del MVP cambia con Federico.

---

## Slice 1 — Runner Local con Zip, Timeout y Memoria

**Problema abordado:** Implementar un runner local real que ejecute funciones Node.js desde archivos zip, con soporte para timeout configurable, límites de memoria, y captura de stdout/stderr con códigos de salida significativos.

**Prompt / Herramienta utilizada:** Cursor Cloud Agent (Claude)

**Código / Arquitectura generada:**

1. **Runtime Node.js mejorado (`runtime-node/`)**
   - `Dockerfile`: Imagen Alpine con Node.js 20, configura paths para handler y evento, usa ENTRYPOINT
   - `bootstrap.js`: Entrypoint mejorado que carga el módulo del usuario, ejecuta el handler con event/context (estilo Lambda), y devuelve JSON. Soporta evento opcional.

2. **Ejemplo Hello mejorado (`examples/hello/`)**
   - `index.js`: Handler con saludo personalizable, timestamp y context
   - `event.json`: Evento de ejemplo

3. **Scripts nuevos (`scripts/`)**
   - `run-zip.sh`: Script principal que:
     - Construye la imagen Docker si no existe
     - Extrae el zip a directorio temporal (con cleanup automático via trap)
     - Ejecuta con `docker run --rm --memory --pids-limit --network=none --read-only`
     - Aplica timeout via comando `timeout`
     - Códigos de salida: 0 (éxito), 124 (timeout), 2 (uso), 3 (build), 4 (unzip)
   - `pack-hello.sh`: Empaqueta `examples/hello` en `dist/hello.zip`
   - `run-hello.sh`: Wrapper que empaqueta y ejecuta en un comando (reemplaza versión anterior)

4. **Documentación**
   - `docs/roadmap.md`: Slice 1 marcado como completado
   - `README.md`: Actualizado con instrucciones de uso zip

**Validación y Corrección Humana:** Validado por Pedro (2026-09-21): revisó el checklist del slice 1 (run-hello/run-zip, timeout, memoria, aislamiento network=none + read-only, contrato handler event/context, Node 20, docs/roadmap). Sin correcciones adicionales pedidas en esa validación.

---

## Slice 2 — Artifacts con MinIO (upload versionado + run-artifact)

**Problema abordado:** Dejar de depender solo de paths locales de zip. Almacenar zips de funciones versionados en MinIO (compatible S3) y permitir que el runner descargue el artifact antes de ejecutar, reutilizando el aislamiento Docker del Slice 1.

**Prompt / Herramienta utilizada:** Cursor Cloud Agent (Claude) — pedido explícito de implementar Slice 2 según especificación detallada en el issue/prompt.

**Código / Arquitectura generada:**

1. **Docker Compose (`docker-compose.yml`)**
   - Servicio MinIO con API `:9000` y consola `:9001`
   - Healthcheck integrado (`mc ready local`)
   - Sidecar `minio-init` que crea bucket `nimbus-artifacts` al iniciar
   - Volume persistente `nimbus-minio-data`
   - Credenciales configurables via env (default: `minioadmin`)

2. **Scripts de artifacts (`scripts/`)**
   - `upload-artifact.sh`: Sube zip a key `functions/{fnId}/versions/{version}/code.zip`. Version = timestamp si no se especifica. Soporta AWS CLI y `mc`.
   - `upload-hello.sh`: Wrapper que empaqueta hello y lo sube como función "hello"
   - `run-artifact.sh`: Descarga artifact de MinIO a temp dir, ejecuta con `run-zip.sh`, cleanup automático. Soporta `latest` para buscar última versión.
   - `run-hello-from-minio.sh`: Demo completa que levanta MinIO, sube hello, y lo ejecuta desde MinIO
   - `minio-init.sh`: Script alternativo para inicializar bucket manualmente

3. **Configuración**
   - `.env.example`: Variables de MinIO y runner documentadas
   - Variables: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_ENDPOINT_URL`, `MINIO_BUCKET`

4. **Documentación**
   - `README.md`: Actualizado con instrucciones de MinIO y nuevos scripts
   - `docs/roadmap.md`: Slice 2 marcado como completado
