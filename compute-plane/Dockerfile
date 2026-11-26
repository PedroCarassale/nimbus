# Nimbus Compute Plane — Go Service
# Slice 4: Ejecuta funciones con aislamiento Docker

FROM golang:1.22-alpine AS builder

WORKDIR /build

# Dependencias de build
RUN apk add --no-cache git

# Copiar go.mod y go.sum para cache de dependencias
COPY go.mod go.sum* ./
RUN go mod download

# Copiar código fuente
COPY . .

# Compilar binario estático
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /compute-plane ./cmd/server

# --- Runtime ---
# alpine 3.21 ships docker-cli 27.x, which can talk to Docker Engine 27–29.
# The previous 3.19 CLI spoke an API older than Engine 29's minimum.
FROM alpine:3.21

RUN apk add --no-cache \
    docker-cli \
    ca-certificates \
    tzdata \
    wget \
    e2fsprogs

WORKDIR /app

# Copiar binario compilado
COPY --from=builder /compute-plane /app/compute-plane

# Puerto por defecto.
# NIMBUS_WORK_DIR se monta en el host con el mismo path (ver docker-compose).
ENV PORT=8080
ENV NIMBUS_WORK_DIR=/tmp/nimbus-work
ENV NIMBUS_IMAGE=nimbus-node

RUN mkdir -p /tmp/nimbus-work

EXPOSE 8080

ENTRYPOINT ["/app/compute-plane"]
