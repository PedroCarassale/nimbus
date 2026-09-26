# Nimbus Console

Consola web mínima para Nimbus Functions (MVP).

## Desarrollo local

```bash
# Instalar dependencias
npm install

# Iniciar servidor de desarrollo (con proxy a localhost:3000)
npm run dev
```

Abre http://localhost:5173 en tu navegador.

**Nota:** El servidor de desarrollo configura un proxy automático a `http://localhost:3000` para las llamadas API. Asegúrate de que el control plane esté corriendo (`make up`).

## Funcionalidades

- **Listar funciones**: Ver todas las funciones creadas
- **Crear función**: Nueva función con nombre
- **Desplegar código**: Subir archivo .zip con el código
- **Invocar función**: Editor JSON para el evento, selección de versión
- **Ver resultado**: Output JSON de la invocación
- **Logs en vivo**: Panel SSE con logs en tiempo real

## Build para producción

```bash
npm run build
```

Los archivos estáticos se generan en `dist/`.

## Variables de entorno

| Variable | Default | Descripción |
|----------|---------|-------------|
| `VITE_API_URL` | `/api` (proxy) | URL del control plane |

## Stack

- React 19 + TypeScript
- Vite 8
- Sin librerías de UI (CSS puro)
