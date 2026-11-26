import { Logger } from '@nestjs/common';
import { RunnerService } from './runner.service';

function jsonResponse(status: number, body: unknown) {
  return {
    status,
    ok: status >= 200 && status < 300,
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

describe('RunnerService', () => {
  const originalEnv = { ...process.env };
  let fetchMock: jest.Mock;
  let service: RunnerService;

  beforeEach(() => {
    process.env = { ...originalEnv, COMPUTE_PLANE_URL: 'http://compute:8080' };
    delete process.env.NIMBUS_TIMEOUT_SEC;
    delete process.env.NIMBUS_MEMORY;
    delete process.env.NIMBUS_HANDLER;
    fetchMock = jest.fn();
    global.fetch = fetchMock as unknown as typeof fetch;
    jest.spyOn(Logger.prototype, 'log').mockImplementation(() => undefined);
    jest.spyOn(Logger.prototype, 'error').mockImplementation(() => undefined);
    jest.spyOn(Logger.prototype, 'warn').mockImplementation(() => undefined);
    service = new RunnerService();
  });

  afterEach(() => {
    process.env = originalEnv;
    jest.restoreAllMocks();
  });

  function sentBody(): Record<string, unknown> {
    return JSON.parse(fetchMock.mock.calls[0][1].body as string);
  }

  describe('invoke', () => {
    it('posts the execution with the default limits', async () => {
      fetchMock.mockResolvedValue(
        jsonResponse(200, {
          success: true,
          output: { msg: 'hola' },
          exitCode: 0,
          durationMs: 42,
          queuedMs: 0,
        }),
      );

      const result = await service.invoke('fn-1', 'v1', { name: 'Ada' });

      expect(fetchMock.mock.calls[0][0]).toBe('http://compute:8080/executions');
      expect(sentBody()).toMatchObject({
        functionId: 'fn-1',
        version: 'v1',
        event: { name: 'Ada' },
        timeoutSec: 5,
        memoryMb: 128,
        handler: 'handler',
      });
      expect(result).toMatchObject({
        success: true,
        output: { msg: 'hola' },
        exitCode: 0,
        durationMs: 42,
      });
      expect(result.executionId).toBe(sentBody().executionId);
    });

    it('reads the limits from the environment', async () => {
      process.env.NIMBUS_TIMEOUT_SEC = '10';
      process.env.NIMBUS_MEMORY = '256m';
      process.env.NIMBUS_HANDLER = 'index.main';
      fetchMock.mockResolvedValue(jsonResponse(200, { success: true }));

      await service.invoke('fn-1', 'v1', {});

      expect(sentBody()).toMatchObject({
        timeoutSec: 10,
        memoryMb: 256,
        handler: 'index.main',
      });
    });

    it('sends a JSON POST', async () => {
      fetchMock.mockResolvedValue(jsonResponse(200, { success: true }));

      await service.invoke('fn-1', 'v1', {});

      const init = fetchMock.mock.calls[0][1];
      expect(init.method).toBe('POST');
      expect(init.headers['Content-Type']).toBe('application/json');
    });

    it('marks a full queue as httpStatus 429', async () => {
      fetchMock.mockResolvedValue(
        jsonResponse(429, { error: 'Cola llena para esta función', queuedMs: 30 }),
      );

      const result = await service.invoke('fn-1', 'v1', {});

      expect(result).toMatchObject({
        success: false,
        httpStatus: 429,
        exitCode: 429,
        error: 'Cola llena para esta función',
        queuedMs: 30,
      });
    });

    it('turns a network failure into an error result instead of throwing', async () => {
      fetchMock.mockRejectedValue(new Error('ECONNREFUSED'));

      const result = await service.invoke('fn-1', 'v1', {});

      expect(result.success).toBe(false);
      expect(result.exitCode).toBe(1);
      expect(result.error).toContain('ECONNREFUSED');
    });
  });

  describe('enqueue', () => {
    it('returns the execution id when the compute plane accepts the job', async () => {
      fetchMock.mockResolvedValue(jsonResponse(202, { executionId: 'exec-9' }));

      const result = await service.enqueue('inv-1', 'fn-1', 'v1', {});

      expect(fetchMock.mock.calls[0][0]).toBe('http://compute:8080/async/jobs');
      expect(sentBody()).toMatchObject({ invocationId: 'inv-1', functionId: 'fn-1' });
      expect(result).toEqual({ executionId: 'exec-9', httpStatus: 202 });
    });

    it('passes through a 400 with the invalid field', async () => {
      fetchMock.mockResolvedValue(
        jsonResponse(400, { error: 'timeoutSec fuera de rango', field: 'timeoutSec' }),
      );

      const result = await service.enqueue('inv-1', 'fn-1', 'v1', {});

      expect(result).toMatchObject({
        httpStatus: 400,
        error: 'timeoutSec fuera de rango',
        field: 'timeoutSec',
      });
    });

    it('reports 502 when the compute plane is unreachable', async () => {
      fetchMock.mockRejectedValue(new Error('ECONNREFUSED'));

      const result = await service.enqueue('inv-1', 'fn-1', 'v1', {});

      expect(result.httpStatus).toBe(502);
      expect(result.error).toContain('ECONNREFUSED');
    });
  });

  describe('cancel', () => {
    it('returns true only when the compute plane confirms', async () => {
      fetchMock.mockResolvedValueOnce(jsonResponse(200, { cancelled: true }));
      fetchMock.mockResolvedValueOnce(jsonResponse(404, {}));

      await expect(service.cancel('exec-1')).resolves.toBe(true);
      await expect(service.cancel('exec-2')).resolves.toBe(false);
      expect(fetchMock.mock.calls[0][0]).toBe(
        'http://compute:8080/executions/exec-1/cancel',
      );
    });
  });
});
