# Nimbus Control Plane — Dockerfile
# Slice 3: NestJS API con bridge temporal hacia scripts de Slice 1-2

FROM node:20-alpine AS builder

WORKDIR /app

# Instalar dependencias de build
COPY package*.json ./
RUN npm ci

# Copiar schema de Prisma y generar cliente
COPY prisma ./prisma/
RUN npx prisma generate

# Copiar código fuente y compilar
COPY tsconfig*.json nest-cli.json ./
COPY src ./src/
RUN npm run build

# ---------- Runtime ----------
FROM node:20-alpine AS runtime

WORKDIR /app

# Instalar dependencias del sistema necesarias para el bridge temporal
# - bash: requerido por los scripts
# - curl: healthcheck + descargas
# - aws-cli: para comunicación con MinIO (S3)
# - docker-cli: para ejecutar containers (bridge temporal Slice 3)
# TODO(slice-4): Cuando el compute plane Go maneje Docker, estas deps se pueden remover
RUN apk add --no-cache \
    bash \
    curl \
    aws-cli \
    docker-cli \
    unzip

# Copiar dependencias de producción
COPY package*.json ./
RUN npm ci --omit=dev

# Copiar cliente Prisma generado
COPY --from=builder /app/node_modules/.prisma ./node_modules/.prisma

# Copiar aplicación compilada
COPY --from=builder /app/dist ./dist

# Copiar schema de Prisma para migraciones
COPY prisma ./prisma/

# El directorio /app/scripts se monta desde el host en docker-compose
# para reutilizar los scripts de Slice 1-2

ENV NODE_ENV=production
ENV PORT=3000

EXPOSE 3000

# Ejecutar migraciones y luego iniciar la app
CMD ["sh", "-c", "npx prisma migrate deploy && node dist/main.js"]
