#!/usr/bin/env node
/**
 * Nimbus Functions — Node.js Bootstrap
 * 
 * Lee el handler del usuario desde NIMBUS_HANDLER_PATH,
 * lee el evento desde NIMBUS_EVENT_PATH (si existe),
 * ejecuta el handler y devuelve el resultado como JSON.
 */

const fs = require('fs');
const path = require('path');

const HANDLER_PATH = process.env.NIMBUS_HANDLER_PATH || '/var/task/index.js';
const EVENT_PATH = process.env.NIMBUS_EVENT_PATH || '/var/task/event.json';
const HANDLER_FUNCTION = process.env.NIMBUS_HANDLER_FUNCTION || 'handler';

async function main() {
  let event = {};
  
  if (fs.existsSync(EVENT_PATH)) {
    try {
      const eventData = fs.readFileSync(EVENT_PATH, 'utf8');
      event = JSON.parse(eventData);
    } catch (err) {
      console.error(`[nimbus] Error leyendo evento: ${err.message}`);
      process.exit(1);
    }
  }

  if (!fs.existsSync(HANDLER_PATH)) {
    console.error(`[nimbus] Handler no encontrado: ${HANDLER_PATH}`);
    process.exit(1);
  }

  let userModule;
  try {
    userModule = require(HANDLER_PATH);
  } catch (err) {
    console.error(`[nimbus] Error cargando handler: ${err.message}`);
    process.exit(1);
  }

  const handlerFn = userModule[HANDLER_FUNCTION];
  if (typeof handlerFn !== 'function') {
    console.error(`[nimbus] Handler "${HANDLER_FUNCTION}" no es una función`);
    process.exit(1);
  }

  try {
    const context = {
      functionName: process.env.NIMBUS_FUNCTION_NAME || 'local',
      memoryLimitInMB: parseInt(process.env.NIMBUS_MEMORY_MB || '128', 10),
      getRemainingTimeInMillis: () => 0,
    };

    const result = await handlerFn(event, context);
    console.log(JSON.stringify(result, null, 2));
  } catch (err) {
    console.error(`[nimbus] Error ejecutando handler: ${err.message}`);
    if (err.stack) {
      console.error(err.stack);
    }
    process.exit(1);
  }
}

main().catch((err) => {
  console.error(`[nimbus] Error fatal: ${err.message}`);
  process.exit(1);
});
