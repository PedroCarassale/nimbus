import { BadRequestException, HttpException, HttpStatus } from '@nestjs/common';
import { Response } from 'express';
import { FunctionsController } from './functions.controller';
import { FunctionsService } from './functions.service';
import { RunnerService } from './runner.service';

function makeRes() {
  const res = {
    status: jest.fn(),
    json: jest.fn(),
  };
  res.status.mockReturnValue(res);
  return res;
}

describe('FunctionsController.invoke', () => {
  let functions: {
    findOne: jest.Mock;
    getLatestVersion: jest.Mock;
    getVersionByTag: jest.Mock;
    createInvocation: jest.Mock;
    updateInvocation: jest.Mock;
  };
  let runner: { invoke: jest.Mock; enqueue: jest.Mock };
  let controller: FunctionsController;
  let res: ReturnType<typeof makeRes>;

  beforeEach(() => {
    functions = {
      findOne: jest.fn().mockResolvedValue({ id: 'fn-1', name: 'hello' }),
      getLatestVersion: jest
        .fn()
        .mockResolvedValue({ id: 'ver-1', version: 'v1' }),
      getVersionByTag: jest.fn(),
      createInvocation: jest
        .fn()
        .mockResolvedValue({ id: 'inv-1', requestId: 'req-1' }),
      updateInvocation: jest.fn().mockResolvedValue({}),
    };
    runner = { invoke: jest.fn(), enqueue: jest.fn() };
    controller = new FunctionsController(
      functions as unknown as FunctionsService,
      runner as unknown as RunnerService,
    );
    res = makeRes();
    jest.spyOn(console, 'log').mockImplementation(() => undefined);
  });

  afterEach(() => jest.restoreAllMocks());

  const invoke = (
    body: { event?: Record<string, unknown>; version?: string },
    asyncQuery?: string,
    header?: string,
  ) =>
    controller.invoke(
      'fn-1',
      body,
      asyncQuery,
      header,
      res as unknown as Response,
    );

  it('marks the invocation RUNNING before calling the compute plane', async () => {
    runner.invoke.mockImplementation(() => {
      expect(functions.updateInvocation).toHaveBeenCalledWith('inv-1', {
        status: 'RUNNING',
      });
      return Promise.resolve({
        executionId: 'exec-1',
        success: true,
        output: { ok: true },
        exitCode: 0,
        durationMs: 10,
      });
    });

    await invoke({ event: { a: 1 } });

    expect(runner.invoke).toHaveBeenCalledWith('fn-1', 'v1', { a: 1 });
    expect(functions.updateInvocation).toHaveBeenLastCalledWith(
      'inv-1',
      expect.objectContaining({ status: 'SUCCESS', attempts: 1 }),
    );
    expect(res.status).toHaveBeenCalledWith(HttpStatus.OK);
    expect(res.json).toHaveBeenCalledWith(
      expect.objectContaining({
        invocationId: 'inv-1',
        status: 'SUCCESS',
        output: { ok: true },
        logsUrl: '/invocations/inv-1/logs',
      }),
    );
  });

  it.each([
    [124, 'TIMEOUT'],
    [1, 'ERROR'],
    [137, 'ERROR'],
  ])('maps exit code %i to %s', async (exitCode, status) => {
    runner.invoke.mockResolvedValue({
      executionId: 'exec-1',
      success: false,
      output: null,
      error: 'boom',
      exitCode,
      durationMs: 10,
    });

    await invoke({});

    expect(res.json).toHaveBeenCalledWith(expect.objectContaining({ status }));
  });

  it('defaults the event to an empty object', async () => {
    runner.invoke.mockResolvedValue({ success: true, exitCode: 0 });

    await invoke({});

    expect(runner.invoke).toHaveBeenCalledWith('fn-1', 'v1', {});
  });

  it('uses the requested version tag', async () => {
    functions.getVersionByTag.mockResolvedValue({ id: 'ver-0', version: 'v0' });
    runner.invoke.mockResolvedValue({ success: true, exitCode: 0 });

    await invoke({ version: 'v0' });

    expect(functions.getVersionByTag).toHaveBeenCalledWith('fn-1', 'v0');
    expect(runner.invoke).toHaveBeenCalledWith('fn-1', 'v0', {});
  });

  it('rejects with 400 when the function has no versions', async () => {
    functions.getLatestVersion.mockResolvedValue(null);

    await expect(invoke({})).rejects.toBeInstanceOf(BadRequestException);
    expect(functions.createInvocation).not.toHaveBeenCalled();
  });

  it('rejects with 400 when the requested version does not exist', async () => {
    functions.getVersionByTag.mockResolvedValue(null);

    await expect(invoke({ version: 'nope' })).rejects.toThrow(/nope/);
  });

  it('translates a full queue into 429 and stores the error', async () => {
    runner.invoke.mockResolvedValue({
      executionId: 'exec-1',
      success: false,
      error: 'Cola llena',
      exitCode: 429,
      durationMs: 5,
      queuedMs: 30,
      httpStatus: 429,
    });

    const err = await invoke({}).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(HttpException);
    expect((err as HttpException).getStatus()).toBe(HttpStatus.TOO_MANY_REQUESTS);
    expect(functions.updateInvocation).toHaveBeenLastCalledWith(
      'inv-1',
      expect.objectContaining({ status: 'ERROR', error: 'Cola llena' }),
    );
  });

  describe('async', () => {
    it.each([
      ['true', undefined],
      ['1', undefined],
      [undefined, 'Event'],
    ])('answers 202 for ?async=%s / header %s', async (query, header) => {
      runner.enqueue.mockResolvedValue({ executionId: 'exec-1', httpStatus: 202 });

      await invoke({}, query, header);

      expect(runner.enqueue).toHaveBeenCalledWith('inv-1', 'fn-1', 'v1', {});
      expect(runner.invoke).not.toHaveBeenCalled();
      expect(res.status).toHaveBeenCalledWith(HttpStatus.ACCEPTED);
      expect(res.json).toHaveBeenCalledWith(
        expect.objectContaining({ invocationId: 'inv-1', status: 'PENDING' }),
      );
    });

    it('stays sync for ?async=false', async () => {
      runner.invoke.mockResolvedValue({ success: true, exitCode: 0 });

      await invoke({}, 'false');

      expect(runner.enqueue).not.toHaveBeenCalled();
      expect(res.status).toHaveBeenCalledWith(HttpStatus.OK);
    });

    it('returns 400 with the field when the compute plane rejects the config', async () => {
      runner.enqueue.mockResolvedValue({
        executionId: 'exec-1',
        httpStatus: 400,
        error: 'memoryMb fuera de rango',
        field: 'memoryMb',
      });

      await expect(invoke({}, 'true')).rejects.toBeInstanceOf(BadRequestException);
      expect(functions.updateInvocation).toHaveBeenLastCalledWith(
        'inv-1',
        expect.objectContaining({ status: 'ERROR', attempts: 0 }),
      );
    });

    it('returns 502 when the job cannot be enqueued', async () => {
      runner.enqueue.mockResolvedValue({
        executionId: 'exec-1',
        httpStatus: 502,
        error: 'ECONNREFUSED',
      });

      const err = await invoke({}, 'true').catch((e: unknown) => e);

      expect((err as HttpException).getStatus()).toBe(HttpStatus.BAD_GATEWAY);
    });
  });
});
