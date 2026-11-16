#!/usr/bin/env node
/**
 * Nimbus Functions — Node.js bootstrap
 *
 * NIMBUS_HANDLER is a file.export spec:
 *   "handler"       → /var/task/index.js, export handler
 *   "index.handler" → /var/task/index.js, export handler
 *   "src/app.main"  → /var/task/src/app.js (or src/app/index.js), export main
 *
 * When NIMBUS_HANDLER is unset, NIMBUS_HANDLER_PATH and
 * NIMBUS_HANDLER_FUNCTION keep the previous behavior.
 */

const fs = require('fs');
const path = require('path');

const TASK_ROOT = '/var/task';

function fail(message) {
  console.error(`[nimbus] ${message}`);
  process.exit(1);
}

function insideTask(candidate) {
  const root = path.resolve(TASK_ROOT);
  const resolved = path.resolve(candidate);
  return resolved === root || resolved.startsWith(root + path.sep);
}

function parseSpec(spec) {
  if (
    spec.includes('..') ||
    spec.startsWith('/') ||
    spec.includes('\\') ||
    spec.includes('\0')
  ) {
    fail(`Handler inválido: ${spec}`);
  }

  const slash = spec.lastIndexOf('/');
  const dot = spec.lastIndexOf('.');
  let fileRel;
  let exportName;
  if (dot > slash && dot > 0) {
    fileRel = spec.slice(0, dot);
    exportName = spec.slice(dot + 1);
  } else if (slash >= 0) {
    fileRel = spec;
    exportName = 'handler';
  } else {
    fileRel = 'index';
    exportName = spec;
  }

  if (!fileRel || !exportName) {
    fail(`Handler inválido: ${spec}`);
  }
  return { fileRel, exportName };
}

function resolveModuleFile(fileRel) {
  const base = path.resolve(TASK_ROOT, fileRel);
  if (!insideTask(base)) {
    fail('Handler fuera de /var/task');
  }

  const candidates = base.endsWith('.js')
    ? [base]
    : [base + '.js', path.join(base, 'index.js'), base];

  for (const candidate of candidates) {
    if (!insideTask(candidate)) {
      continue;
    }
    try {
      if (fs.statSync(candidate).isFile()) {
        return candidate;
      }
    } catch {
      // Try the next candidate.
    }
  }
  return candidates[0];
}

function loadHandlerTarget() {
  const spec = (process.env.NIMBUS_HANDLER || '').trim();
  if (spec) {
    const parsed = parseSpec(spec);
    return {
      modulePath: resolveModuleFile(parsed.fileRel),
      exportName: parsed.exportName,
    };
  }

  const modulePath = process.env.NIMBUS_HANDLER_PATH || path.join(TASK_ROOT, 'index.js');
  if (!insideTask(modulePath) && path.isAbsolute(modulePath)) {
    const resolved = path.resolve(modulePath);
    if (!insideTask(resolved)) {
      fail('Handler fuera de /var/task');
    }
  }
  return {
    modulePath,
    exportName: process.env.NIMBUS_HANDLER_FUNCTION || 'handler',
  };
}

async function main() {
  const eventPath = process.env.NIMBUS_EVENT_PATH || path.join(TASK_ROOT, 'event.json');
  let event = {};

  if (fs.existsSync(eventPath)) {
    try {
      event = JSON.parse(fs.readFileSync(eventPath, 'utf8'));
    } catch (err) {
      fail(`Error leyendo evento: ${err.message}`);
    }
  }

  const target = loadHandlerTarget();
  if (!fs.existsSync(target.modulePath)) {
    fail(`Handler no encontrado: ${target.modulePath}`);
  }

  let userModule;
  try {
    userModule = require(target.modulePath);
  } catch (err) {
    fail(`Error cargando handler: ${err.message}`);
  }

  const handlerFn = userModule[target.exportName];
  if (typeof handlerFn !== 'function') {
    fail(`Handler "${target.exportName}" no es una función`);
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
