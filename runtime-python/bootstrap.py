#!/usr/bin/env python3
"""Nimbus Functions — Python bootstrap.

One-shot (default): read the event JSON, call the handler once, print the
result, and exit. This is the cold path.

Warm (NIMBUS_WARM=1): import the handler once, then loop. Each invocation
arrives as one JSON line on stdin and the result leaves as JSON lines on
stdout. Logs from print() and the logging stream go out as log frames so
they do not break the protocol.

NIMBUS_HANDLER is a file.function spec:
  "handler"       → /var/task/index.py, function handler
  "index.handler" → /var/task/index.py, function handler
  "src/app.main"  → /var/task/src/app.py, function main

The handler is a normal function (event, context) -> JSON value. A
coroutine is rejected: this runtime does not drive an event loop.
"""

import importlib.util
import json
import os
import sys
import traceback

TASK_ROOT = os.environ.get("NIMBUS_TASK_ROOT", "/var/task")
WARM = os.environ.get("NIMBUS_WARM") == "1"
CURRENT_EXEC = ""


def emit(obj):
    line = json.dumps(obj, ensure_ascii=False) + "\n"
    os.write(1, line.encode("utf-8"))


def fail(message):
    text = str(message)
    if WARM:
        try:
            emit({"type": "fatal", "error": text, "executionId": CURRENT_EXEC})
        except Exception:
            pass
    else:
        sys.stderr.write("[nimbus] %s\n" % text)
    raise SystemExit(1)


def inside_task(candidate):
    root = os.path.realpath(TASK_ROOT)
    resolved = os.path.realpath(candidate)
    return resolved == root or resolved.startswith(root + os.sep)


def parse_spec(spec):
    if ".." in spec or spec.startswith("/") or "\\" in spec or "\0" in spec:
        fail("Handler inválido: %s" % spec)
    slash = spec.rfind("/")
    dot = spec.rfind(".")
    if dot > slash and dot > 0:
        file_rel = spec[:dot]
        export_name = spec[dot + 1 :]
    elif slash >= 0:
        file_rel = spec
        export_name = "handler"
    else:
        file_rel = "index"
        export_name = spec
    if not file_rel or not export_name:
        fail("Handler inválido: %s" % spec)
    return file_rel, export_name


def resolve_module_file(file_rel):
    base = os.path.realpath(os.path.join(TASK_ROOT, file_rel))
    if not inside_task(base):
        fail("Handler fuera de /var/task")
    candidates = [base] if base.endswith(".py") else [base + ".py", os.path.join(base, "__init__.py")]
    for candidate in candidates:
        if inside_task(candidate) and os.path.isfile(candidate):
            return candidate
    return candidates[0]


def load_handler_target():
    spec = os.environ.get("NIMBUS_HANDLER", "").strip()
    if spec:
        file_rel, export_name = parse_spec(spec)
        return resolve_module_file(file_rel), export_name
    module_path = os.environ.get("NIMBUS_HANDLER_PATH") or os.path.join(TASK_ROOT, "index.py")
    if os.path.isabs(module_path) and not inside_task(module_path):
        fail("Handler fuera de /var/task")
    return module_path, os.environ.get("NIMBUS_HANDLER_FUNCTION") or "handler"


class Context:
    def __init__(self):
        self.function_name = os.environ.get("NIMBUS_FUNCTION_NAME", "local")
        self.functionName = self.function_name
        try:
            memory = int(os.environ.get("NIMBUS_MEMORY_MB", "128"))
        except ValueError:
            memory = 128
        self.memory_limit_in_mb = memory
        self.memoryLimitInMB = memory

    def get_remaining_time_in_millis(self):
        return 0

    def getRemainingTimeInMillis(self):
        return 0


class LineWriter:
    """Buffers text and turns each line into a warm log frame."""

    def __init__(self, stream_name):
        self.stream_name = stream_name
        self.buf = ""

    def write(self, text):
        if not isinstance(text, str):
            text = str(text)
        self.buf += text
        while "\n" in self.buf:
            line, self.buf = self.buf.split("\n", 1)
            emit_log(self.stream_name, line.rstrip("\r"))
        return len(text)

    def flush(self):
        if self.buf:
            emit_log(self.stream_name, self.buf)
            self.buf = ""

    def isatty(self):
        return False


def emit_log(stream_name, line):
    emit(
        {
            "type": "log",
            "stream": stream_name,
            "executionId": CURRENT_EXEC,
            "line": line,
        }
    )


def load_user_handler():
    module_path, export_name = load_handler_target()
    if not os.path.isfile(module_path):
        fail("Handler no encontrado: %s" % module_path)
    module_name = "nimbus_user_handler"
    spec = importlib.util.spec_from_file_location(module_name, module_path)
    if spec is None or spec.loader is None:
        fail("Error cargando handler: %s" % module_path)
    module = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(module)
    except Exception as err:
        fail("Error cargando handler: %s" % err)
    handler_fn = getattr(module, export_name, None)
    if not callable(handler_fn):
        fail('Handler "%s" no es una función' % export_name)
    return handler_fn


def json_safe(value):
    json.dumps(value)
    return value


def call_handler(handler_fn, event):
    result = handler_fn(event if isinstance(event, dict) else {}, Context())
    if hasattr(result, "__await__"):
        raise TypeError("el handler devolvió una corrutina; esta runtime espera una función normal")
    if result is None:
        return None
    return json_safe(result)


def run_once():
    event_path = os.environ.get("NIMBUS_EVENT_PATH") or os.path.join(TASK_ROOT, "event.json")
    event = {}
    if os.path.isfile(event_path):
        try:
            with open(event_path, "r", encoding="utf-8") as handle:
                event = json.load(handle)
        except Exception as err:
            fail("Error leyendo evento: %s" % err)
    handler_fn = load_user_handler()
    trace_id = os.environ.get("NIMBUS_TRACE_ID", "")
    traceparent = os.environ.get("TRACEPARENT", "")
    if trace_id or traceparent:
        sys.stderr.write("[nimbus] traceId=%s traceparent=%s\n" % (trace_id, traceparent))
    try:
        result = call_handler(handler_fn, event)
    except SystemExit:
        raise
    except Exception as err:
        sys.stderr.write("[nimbus] Error ejecutando handler: %s\n" % err)
        traceback.print_exc()
        raise SystemExit(1)
    sys.stdout.write(json.dumps(result, indent=2, ensure_ascii=False) + "\n")


def apply_trace(trace_id, traceparent):
    if trace_id:
        os.environ["NIMBUS_TRACE_ID"] = trace_id
    if traceparent:
        os.environ["TRACEPARENT"] = traceparent
    if trace_id or traceparent:
        emit_log("stderr", "[nimbus] traceId=%s traceparent=%s" % (trace_id, traceparent))


def handle_invoke(line, handler_fn, stdout_writer, stderr_writer):
    global CURRENT_EXEC
    try:
        msg = json.loads(line)
    except Exception as err:
        emit(
            {
                "type": "done",
                "executionId": "",
                "ok": False,
                "output": None,
                "error": "Pedido warm inválido: %s" % err,
                "exitCode": 1,
            }
        )
        return
    execution_id = msg.get("executionId") if isinstance(msg.get("executionId"), str) else ""
    CURRENT_EXEC = execution_id
    event = msg.get("event") if isinstance(msg.get("event"), dict) else {}
    trace_id = msg.get("traceId") if isinstance(msg.get("traceId"), str) else ""
    traceparent = msg.get("traceparent") if isinstance(msg.get("traceparent"), str) else ""
    apply_trace(trace_id, traceparent)
    try:
        result = call_handler(handler_fn, event)
        stdout_writer.flush()
        stderr_writer.flush()
        emit(
            {
                "type": "done",
                "executionId": execution_id,
                "ok": True,
                "output": result,
                "error": "",
                "exitCode": 0,
            }
        )
    except Exception as err:
        stdout_writer.flush()
        stderr_writer.flush()
        emit_log("stderr", "[nimbus] Error ejecutando handler: %s" % err)
        emit(
            {
                "type": "done",
                "executionId": execution_id,
                "ok": False,
                "output": None,
                "error": str(err),
                "exitCode": 1,
            }
        )


def serve(handler_fn):
    stdout_writer = LineWriter("stdout")
    stderr_writer = LineWriter("stderr")
    sys.stdout = stdout_writer
    sys.stderr = stderr_writer
    for line in sys.stdin:
        if line.strip() == "":
            continue
        handle_invoke(line, handler_fn, stdout_writer, stderr_writer)


def main_warm():
    import time

    t0 = time.perf_counter()
    handler_fn = load_user_handler()
    emit({"type": "ready", "initMs": int((time.perf_counter() - t0) * 1000)})
    serve(handler_fn)


if __name__ == "__main__":
    if WARM:
        try:
            main_warm()
        except SystemExit:
            raise
        except Exception as err:
            fail(err)
    else:
        run_once()
