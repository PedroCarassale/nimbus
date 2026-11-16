#!/usr/bin/env node
/**
 * Nimbus Functions — Node.js bootstrap
 *
 * One-shot (default): read the event JSON, call the handler once, print
 * the result, and exit. This is the cold path and the local runner.
 *
 * Warm (NIMBUS_WARM=1): load the handler once, then loop. Each invocation
 * arrives as one JSON line on stdin and the result leaves as JSON lines
 * on stdout. The process stays up, so module state survives between
 * calls (the same leak Lambda keeps). A timeout, crash, or OOM is handled
 * outside: the compute plane destroys the container and does not reuse it.
 *
 * NIMBUS_HANDLER is a file.export spec:
 *   "handler"       → /var/task/index.js, export handler
 *   "index.handler" → /var/task/index.js, export handler
 *   "src/app.main"  → /var/task/src/app.js (or src/app/index.js), export main
 *
 * When NIMBUS_HANDLER is unset, NIMBUS_HANDLER_PATH and
 * NIMBUS_HANDLER_FUNCTION keep the previous behavior.
 *
 * NIMBUS_TASK_ROOT overrides the task directory (default /var/task).
 * The platform does not set it. It exists so the protocol can be tested
 * outside Docker.
 */

const fs = require('fs');
const path = require('path');
const readline = require('readline');

const TASK_ROOT = process.env.NIMBUS_TASK_ROOT || '/var/task';
const WARM = process.env.NIMBUS_WARM === '1';

let currentExec = '';

function emit(obj) {
  fs.writeSync(1, JSON.stringify(obj) + '\n');
}

function fail(message) {
  if (WARM) {
    try {
      emit({ type: 'fatal', error: String(message), executionId: currentExec });
    } catch {
      // The pipe is already gone. Exiting is the useful signal.
    }
  } else {
    console.error(`[nimbus] ${message}`);
  }
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
  if (path.isAbsolute(modulePath) && !insideTask(modulePath)) {
    fail('Handler fuera de /var/task');
  }
  return {
    modulePath,
    exportName: process.env.NIMBUS_HANDLER_FUNCTION || 'handler',
  };
}

function logTrace(traceId, traceparent) {
  if (!traceId && !traceparent) {
    return;
  }
  console.error(`[nimbus] traceId=${traceId || ''} traceparent=${traceparent || ''}`);
}

function contextFor() {
  return {
    functionName: process.env.NIMBUS_FUNCTION_NAME || 'local',
    memoryLimitInMB: parseInt(process.env.NIMBUS_MEMORY_MB || '128', 10),
    getRemainingTimeInMillis: () => 0,
  };
}

function installLogHooks() {
  const pending = { stdout: '', stderr: '' };

  function wrap(streamName) {
    const stream = process[streamName];
    stream.write = function write(chunk, encoding, cb) {
      if (typeof encoding === 'function') {
        cb = encoding;
        encoding = undefined;
      }
      const text = Buffer.isBuffer(chunk)
        ? chunk.toString(typeof encoding === 'string' ? encoding : 'utf8')
        : String(chunk);
      pending[streamName] += text;
      let nl;
      while ((nl = pending[streamName].indexOf('\n')) >= 0) {
        const line = pending[streamName].slice(0, nl).replace(/\r$/, '');
        pending[streamName] = pending[streamName].slice(nl + 1);
        emit({
          type: 'log',
          stream: streamName,
          executionId: currentExec,
          line,
        });
      }
      if (typeof cb === 'function') {
        cb();
      }
      return true;
    };
  }

  wrap('stdout');
  wrap('stderr');

  return function flush() {
    for (const streamName of ['stdout', 'stderr']) {
      if (pending[streamName].length === 0) {
        continue;
      }
      emit({
        type: 'log',
        stream: streamName,
        executionId: currentExec,
        line: pending[streamName],
      });
      pending[streamName] = '';
    }
  };
}

function loadUserHandler() {
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
  return handlerFn;
}

async function runOnce() {
  const eventPath = process.env.NIMBUS_EVENT_PATH || path.join(TASK_ROOT, 'event.json');
  let event = {};

  if (fs.existsSync(eventPath)) {
    try {
      event = JSON.parse(fs.readFileSync(eventPath, 'utf8'));
    } catch (err) {
      fail(`Error leyendo evento: ${err.message}`);
    }
  }

  const handlerFn = loadUserHandler();
  logTrace(process.env.NIMBUS_TRACE_ID, process.env.TRACEPARENT);

  try {
    const result = await handlerFn(event, contextFor());
    console.log(JSON.stringify(result, null, 2));
  } catch (err) {
    console.error(`[nimbus] Error ejecutando handler: ${err.message}`);
    if (err.stack) {
      console.error(err.stack);
    }
    process.exit(1);
  }
}

async function handleInvoke(line, handlerFn, flush) {
  let msg;
  try {
    msg = JSON.parse(line);
  } catch (err) {
    emit({
      type: 'done',
      executionId: '',
      ok: false,
      output: null,
      error: `Pedido warm inválido: ${err.message}`,
      exitCode: 1,
    });
    return;
  }

  const executionId = typeof msg.executionId === 'string' ? msg.executionId : '';
  currentExec = executionId;
  const event = msg.event && typeof msg.event === 'object' ? msg.event : {};
  // docker -e is fixed when the warm process starts. Each invoke carries its
  // own trace and we publish it on process.env for the handler of this call.
  const traceId = typeof msg.traceId === 'string' ? msg.traceId : '';
  const traceparent = typeof msg.traceparent === 'string' ? msg.traceparent : '';
  if (traceId) {
    process.env.NIMBUS_TRACE_ID = traceId;
  }
  if (traceparent) {
    process.env.TRACEPARENT = traceparent;
  }
  if (traceId || traceparent) {
    emit({
      type: 'log',
      stream: 'stderr',
      executionId,
      line: `[nimbus] traceId=${traceId} traceparent=${traceparent}`,
    });
  }

  try {
    const result = await handlerFn(event, contextFor());
    flush();
    let output = result === undefined ? null : result;
    try {
      JSON.stringify(output);
    } catch (err) {
      flush();
      emit({
        type: 'done',
        executionId,
        ok: false,
        output: null,
        error: `El handler devolvió un valor que no es JSON: ${err.message}`,
        exitCode: 1,
      });
      return;
    }
    emit({
      type: 'done',
      executionId,
      ok: true,
      output,
      error: '',
      exitCode: 0,
    });
  } catch (err) {
    const message = err && err.message ? err.message : String(err);
    emit({
      type: 'log',
      stream: 'stderr',
      executionId,
      line: `[nimbus] Error ejecutando handler: ${message}`,
    });
    if (err && err.stack) {
      emit({
        type: 'log',
        stream: 'stderr',
        executionId,
        line: err.stack,
      });
    }
    flush();
    emit({
      type: 'done',
      executionId,
      ok: false,
      output: null,
      error: message,
      exitCode: 1,
    });
  }
}

function serve(handlerFn, flush) {
  const lines = [];
  let working = false;
  const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });

  async function drain() {
    if (working) {
      return;
    }
    working = true;
    try {
      while (lines.length > 0) {
        const line = lines.shift();
        if (line.trim() === '') {
          continue;
        }
        await handleInvoke(line, handlerFn, flush);
      }
    } catch (err) {
      fail(err && err.message ? err.message : String(err));
    } finally {
      working = false;
      if (lines.length > 0) {
        drain();
      }
    }
  }

  rl.on('line', (line) => {
    lines.push(line);
    drain();
  });
  rl.on('close', () => {
    process.exit(0);
  });
}

function mainWarm() {
  const started = Date.now();
  const flush = installLogHooks();
  const handlerFn = loadUserHandler();
  emit({ type: 'ready', initMs: Date.now() - started });

  process.on('uncaughtException', (err) => {
    fail(err && err.message ? err.message : String(err));
  });
  process.on('unhandledRejection', (err) => {
    const message = err && err.message ? err.message : String(err);
    fail(message);
  });

  serve(handlerFn, flush);
}

if (WARM) {
  mainWarm();
} else {
  runOnce().catch((err) => {
    console.error(`[nimbus] Error fatal: ${err.message}`);
    process.exit(1);
  });
}
