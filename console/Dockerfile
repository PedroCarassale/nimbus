# Nimbus Console — Dockerfile
# Build: crea archivos estáticos con Vite
# Runtime: nginx sirve los archivos

# ---- Build Stage ----
FROM node:22-alpine AS builder

WORKDIR /app

# Instalar dependencias
COPY package*.json ./
RUN npm ci

# Copiar código fuente
COPY . .

# Build con la URL de la API en producción (relativa, sin proxy)
ENV VITE_API_URL=
RUN npm run build

# ---- Runtime Stage ----
FROM nginx:alpine

# Copiar archivos estáticos
COPY --from=builder /app/dist /usr/share/nginx/html

# Configuración nginx para SPA + proxy a control-plane
COPY nginx.conf /etc/nginx/conf.d/default.conf

EXPOSE 80

CMD ["nginx", "-g", "daemon off;"]
