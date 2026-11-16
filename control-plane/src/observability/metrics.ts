import { Counter, Histogram, Registry } from 'prom-client';
import type { NextFunction, Request, Response } from 'express';

export const register = new Registry();

const httpRequests = new Counter({
  name: 'nimbus_http_requests_total',
  help: 'HTTP requests served by the control plane.',
  labelNames: ['method', 'route', 'status'] as const,
  registers: [register],
});

const httpDuration = new Histogram({
  name: 'nimbus_http_request_duration_seconds',
  help: 'HTTP request duration in the control plane.',
  labelNames: ['method', 'route'] as const,
  buckets: [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60],
  registers: [register],
});

/**
 * Records method, a templated route and status. UUIDs in the path are
 * replaced so invocation ids do not become labels.
 */
export function httpMetrics(req: Request, res: Response, next: NextFunction): void {
  if (req.path === '/metrics') {
    next();
    return;
  }
  const start = process.hrtime.bigint();
  res.on('finish', () => {
    const route = routeLabel(req);
    const status = String(res.statusCode);
    httpRequests.inc({ method: req.method, route, status });
    const seconds = Number(process.hrtime.bigint() - start) / 1e9;
    httpDuration.observe({ method: req.method, route }, seconds);
  });
  next();
}

function routeLabel(req: Request): string {
  const raw = (req.originalUrl || req.path || '').split('?')[0] || 'unknown';
  return raw
    .replace(/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/gi, ':id')
    .slice(0, 160);
}
