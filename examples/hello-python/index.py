"""Hello function for the python3.12 runtime.

handler: index.handler
event.mode selects a behavior used by make runtime-check:
  (none)  greeting
  sleep   event.ms milliseconds
  mem     retain event.mb megabytes
  error   raise
  env     return GREETING
  probe   uid, trace id, read-only fs, network
"""

import os
import socket
import time


def handler(event, context):
    event = event or {}
    mode = event.get("mode")
    if mode == "sleep":
        ms = int(event.get("ms") or 5000)
        time.sleep(ms / 1000.0)
        return {"slept": True, "ms": ms}
    if mode == "mem":
        chunks = []
        block = 8 * 1024 * 1024
        target = int(event.get("mb") or 512) * 1024 * 1024
        got = 0
        while got < target:
            chunks.append(bytearray(block))
            got += block
        return {"bytes": got}
    if mode == "error":
        raise RuntimeError(event.get("message") or "boom")
    if mode == "env":
        return {
            "greeting": os.environ.get("GREETING"),
            "memoryLimitInMB": context.memoryLimitInMB,
        }
    if mode == "probe":
        return _probe()
    name = event.get("name") or "mundo"
    print("hola desde python")
    return {
        "message": "Hola, %s" % name,
        "runtime": "python3.12",
        "uid": os.getuid(),
    }


def _probe():
    task_err = _write("/var/task/pwn.txt")
    tmp_err = _write("/tmp/pwn.txt")
    net_err = "ok"
    try:
        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.settimeout(1)
        sock.connect(("1.1.1.1", 443))
        sock.close()
    except OSError as err:
        net_err = err.strerror or str(err)
    return {
        "uid": os.getuid(),
        "traceId": os.environ.get("NIMBUS_TRACE_ID") or "",
        "taskWrite": task_err,
        "tmpWrite": tmp_err,
        "net": net_err,
    }


def _write(path):
    try:
        with open(path, "w", encoding="utf-8") as handle:
            handle.write("x")
    except OSError as err:
        return err.strerror or str(err)
    return "ok"
