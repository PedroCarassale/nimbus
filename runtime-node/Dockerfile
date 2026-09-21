# Nimbus Functions — Node.js Runtime
# Imagen base para ejecutar funciones Node.js del usuario

FROM node:20-alpine

WORKDIR /var/runtime

# El código del usuario se monta en /var/task
ENV NIMBUS_HANDLER_PATH=/var/task/index.js
ENV NIMBUS_EVENT_PATH=/var/task/event.json

COPY bootstrap.js /var/runtime/bootstrap.js

# El bootstrap lee el handler y el evento, ejecuta la función
ENTRYPOINT ["node", "/var/runtime/bootstrap.js"]
