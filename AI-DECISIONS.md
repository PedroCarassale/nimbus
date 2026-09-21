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

**Problema abordado:** Ordenar el trabajo restante hasta un MVP demostrable (create → deploy → invoke + logs).

**Prompt / Herramienta utilizada:** Grok Bot (nimbus) — armado de roadmap en chat y persistencia en `docs/roadmap.md`.

**Código / Arquitectura generada:** `docs/roadmap.md` con slices 1–7 (runner, artifacts, control plane, compute, logs, DX, consola opcional) y exclusiones explícitas.

**Validación y Corrección Humana:** Pedro pidió guardar el roadmap en el repo. Criterio de done acordado: compose + zip + invoke HTTP + logs sin Docker manual. Ajustable si el alcance del MVP cambia con Federico.
