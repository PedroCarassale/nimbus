# Nimbus Functions — Stack MVP

**Owners:** Pedro Carassale, Federico Tessadro

---

## Stack Técnico

| Sujeto | Tecnología | Notas |
|--------|------------|-------|
| Control plane (APIs create/deploy/config) | NestJS + TypeScript | APIs y orquestación; no ejecuta el código del usuario |
| Metadata | PostgreSQL | Funciones, versiones, invocaciones, configuración inmutable |
| Cola / concurrency / cache | Redis | Locks, rate limits, warm hints, pub/sub de logs |
| Artifact store | MinIO (API S3) | Local-first; luego S3 real sin cambiar contrato |
| Compute plane (scheduler + runner) | Go | Binario chico, manejo de timeouts y procesos |
| Aislamiento (MVP) | Docker | Firecracker/microVM después |
| Runtime del usuario | Node.js (imagen fija de la plataforma) | El zip solo trae el código del usuario |
| Invoke sync | Nest (edge) → Go (exec) | Nest valida; Go ejecuta |
| Contrato Nest ↔ Go | HTTP o gRPC corto | Métodos: `StartExecution`, `Cancel` |
| Logs live | SSE desde control plane | Runner empuja líneas a Redis/stream |
| Métricas (post-MVP) | OpenTelemetry → Prometheus + Grafana | No bloquea el primer slice |
| Consola | React + TypeScript + Vite | Create/deploy/invoke + logs |
| Entorno local | Docker Compose | pg + redis + minio + control + compute |

---

## Arquitectura de Deploy

- **Artifact de deploy** = zip del código del usuario (no la imagen del runner).
- El runner usa una imagen Node.js fija provista por la plataforma.
- El código del usuario se monta/extrae en el container en tiempo de ejecución.

---

## Alcance MVP

- Invoke sincrónico
- Zip con código Node.js
- Aislamiento con Docker
- Timeout configurable
- Logs en tiempo real vía SSE

---

## Diferido (post-MVP)

| Feature | Descripción |
|---------|-------------|
| Multi-runtime | Python, Go, otros runtimes además de Node.js |
| Auth productizada | Sistema de autenticación y autorización completo |
| Billing | Medición de uso y facturación |
| VPC | Networking privado para funciones |
| Async / DLQ | Invocaciones asíncronas con dead-letter queues |
| Provisioned concurrency | Instancias pre-calentadas |
| Firecracker | Aislamiento con microVMs en lugar de Docker |

---

## Flujo de Invocación (MVP)

```
Cliente → Control Plane (Nest) → Compute Plane (Go) → Container Docker (Node.js + código usuario)
                ↑                        |
                └── Logs SSE ←── Redis ──┘
```

1. El cliente hace request al control plane (NestJS)
2. Nest valida auth, busca metadata en PostgreSQL
3. Nest envía `StartExecution` al compute plane (Go)
4. Go levanta/reutiliza container Docker con la imagen Node.js
5. Go monta el zip del usuario y ejecuta la función
6. Logs se empujan a Redis y se streamean vía SSE al cliente
7. Resultado se devuelve sincronicamente

---

*Documento de stack técnico para el MVP de Nimbus Functions.*
