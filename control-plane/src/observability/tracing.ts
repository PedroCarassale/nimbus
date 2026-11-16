import {
  context,
  diag,
  DiagLogger,
  propagation,
  SpanKind,
  SpanStatusCode,
  trace,
} from '@opentelemetry/api';
import { W3CTraceContextPropagator } from '@opentelemetry/core';
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-http';
import { Resource } from '@opentelemetry/resources';
import { BatchSpanProcessor } from '@opentelemetry/sdk-trace-base';
import { NodeTracerProvider } from '@opentelemetry/sdk-trace-node';
import type { NextFunction, Request, Response } from 'express';

/**
 * Tracing is on unless NIMBUS_TRACING is 0/false/off/no or OTEL_SDK_DISABLED=true.
 * Spans are batched. A collector that is down logs at most once every 30s and
 * does not fail the HTTP request.
 */
export function setupTracing(): void {
  installQuietLogger();
  if (!tracingEnabled()) {
    console.log('[trace] tracing apagado (NIMBUS_TRACING)');
    return;
  }

  const endpoint = exporterURL();
  const serviceName = process.env.OTEL_SERVICE_NAME || 'nimbus-control-plane';
  const exporter = new OTLPTraceExporter({
    url: endpoint,
    timeoutMillis: 2000,
  });
  const provider = new NodeTracerProvider({
    resource: new Resource({ 'service.name': serviceName }),
    spanProcessors: [
      new BatchSpanProcessor(exporter, {
        scheduledDelayMillis: 1000,
        exportTimeoutMillis: 2000,
        maxExportBatchSize: 128,
        maxQueueSize: 2048,
      }),
    ],
  });
  provider.register({
    propagator: new W3CTraceContextPropagator(),
  });
  console.log(`[trace] OTLP ${endpoint} service=${serviceName}`);
}

export function tracingEnabled(): boolean {
  const flag = (process.env.NIMBUS_TRACING || '').trim().toLowerCase();
  if (flag === '0' || flag === 'false' || flag === 'off' || flag === 'no') {
    return false;
  }
  if ((process.env.OTEL_SDK_DISABLED || '').trim().toLowerCase() === 'true') {
    return false;
  }
  return true;
}

/**
 * Server span for the incoming request. Invoke is named nimbus.invoke so a
 * trace shows the control-plane request before the client call to Go.
 */
export function tracingMiddleware(req: Request, res: Response, next: NextFunction): void {
  if (!tracingEnabled()) {
    next();
    return;
  }
  const path = req.path || '';
  if (path === '/metrics' || path === '/health') {
    next();
    return;
  }
  const parent = propagation.extract(context.active(), req.headers);
  const invoke = path.includes('/invoke');
  const tracer = trace.getTracer('nimbus-control-plane');
  const span = tracer.startSpan(
    invoke ? 'nimbus.invoke' : `${req.method} ${path}`,
    {
      kind: SpanKind.SERVER,
      attributes: {
        'http.request.method': req.method,
        'url.path': path,
      },
    },
    parent,
  );
  if (invoke) {
    span.setAttribute('nimbus.function_id', functionIdFrom(path));
    span.setAttribute('nimbus.mode', wantsAsync(req) ? 'async' : 'sync');
  }
  const ctx = trace.setSpan(parent, span);
  res.on('finish', () => {
    span.setAttribute('http.response.status_code', res.statusCode);
    if (res.statusCode >= 500) {
      span.setStatus({ code: SpanStatusCode.ERROR });
    }
    span.end();
  });
  context.with(ctx, () => next());
}

function functionIdFrom(path: string): string {
  const parts = path.split('/').filter(Boolean);
  const idx = parts.indexOf('invoke');
  if (idx > 0) {
    return parts[idx - 1];
  }
  return '';
}

function wantsAsync(req: Request): boolean {
  const header = req.header('x-invocation-type');
  if (header && header.trim().toLowerCase() === 'event') {
    return true;
  }
  const query = req.query?.async;
  const value = Array.isArray(query) ? query[0] : query;
  if (typeof value !== 'string') {
    return false;
  }
  const flag = value.trim().toLowerCase();
  return flag === 'true' || flag === '1' || flag === 'yes';
}

function exporterURL(): string {
  let raw = (process.env.OTEL_EXPORTER_OTLP_ENDPOINT || 'http://otel-collector:4318').trim();
  raw = raw.replace(/\/+$/, '');
  if (!raw.includes('://')) {
    raw = `http://${raw}`;
  }
  if (!raw.endsWith('/v1/traces')) {
    raw = `${raw}/v1/traces`;
  }
  return raw;
}

function installQuietLogger(): void {
  const logger: DiagLogger = {
    error(message: string) {
      logOnce(`[trace] ${message}`);
    },
    warn(message: string) {
      logOnce(`[trace] ${message}`);
    },
    info() {},
    debug() {},
    verbose() {},
  };
  diag.setLogger(logger);
}

let nextLogAt = 0;
function logOnce(message: string): void {
  const now = Date.now();
  if (now < nextLogAt) {
    return;
  }
  nextLogAt = now + 30_000;
  console.error(message);
}
