# Nimbus Control Plane — Dockerfile
# Slice 3-4: NestJS API que delega ejecuciones al compute-plane Go

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

# Dependencias mínimas del sistema
# - curl: healthcheck
RUN apk add --no-cache curl

# Copiar dependencias de producción
COPY package*.json ./
RUN npm ci --omit=dev

# Copiar cliente Prisma generado
COPY --from=builder /app/node_modules/.prisma ./node_modules/.prisma

# Copiar aplicación compilada
COPY --from=builder /app/dist ./dist

# Copiar schema de Prisma para migraciones
COPY prisma ./prisma/

ENV NODE_ENV=production
ENV PORT=3000

EXPOSE 3000

# Ejecutar migraciones y luego iniciar la app
CMD ["sh", "-c", "npx prisma migrate deploy && node dist/main.js"]
