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
FROM alpine:3.19

RUN apk add --no-cache \
    docker-cli \
    ca-certificates \
    tzdata

WORKDIR /app

# Copiar binario compilado
COPY --from=builder /compute-plane /app/compute-plane

# Puerto por defecto
ENV PORT=8080

EXPOSE 8080

ENTRYPOINT ["/app/compute-plane"]
