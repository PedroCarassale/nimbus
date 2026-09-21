#!/usr/bin/env node
"use strict";

const fs = require("fs");
const path = require("path");

const handlerPath = process.env.NIMBUS_HANDLER_PATH || "/var/task/index.js";
const eventPath = process.env.NIMBUS_EVENT_PATH || "/tmp/event.json";

async function main() {
  let handler;
  try {
    const mod = require(path.resolve(handlerPath));
    handler = mod.handler;
    if (typeof handler !== "function") {
      throw new Error(`handler is not a function in ${handlerPath}`);
    }
  } catch (err) {
    console.log(JSON.stringify({ ok: false, error: `load handler: ${err.message}` }));
    process.exit(1);
  }

  let event;
  try {
    const raw = fs.readFileSync(eventPath, "utf8");
    event = JSON.parse(raw);
  } catch (err) {
    console.log(JSON.stringify({ ok: false, error: `read event: ${err.message}` }));
    process.exit(1);
  }

  try {
    const result = await handler(event);
    console.log(JSON.stringify({ ok: true, result }));
  } catch (err) {
    console.log(JSON.stringify({ ok: false, error: `handler error: ${err.message}` }));
    process.exit(1);
  }
}

main();
