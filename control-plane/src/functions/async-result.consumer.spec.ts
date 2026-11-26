import { AsyncResultConsumer } from './async-result.consumer';
import { FunctionsService } from './functions.service';
import { RedisService } from '../redis/redis.service';

/**
 * Drives the private apply() with a fake Redis client: the loop itself needs a
 * real server, but the rules that decide what reaches Postgres do not.
 */
describe('AsyncResultConsumer.apply', () => {
  let store: Map<string, string>;
  let functions: { findInvocation: jest.Mock; updateInvocation: jest.Mock };
  let consumer: AsyncResultConsumer;

  const apply = (event: unknown) =>
    (consumer as unknown as { apply(data: string): Promise<void> }).apply(
      typeof event === 'string' ? event : JSON.stringify(event),
    );

  beforeEach(() => {
    store = new Map();
    functions = {
      findInvocation: jest.fn().mockResolvedValue({ id: 'inv-1' }),
      updateInvocation: jest.fn().mockResolvedValue({}),
    };
    consumer = new AsyncResultConsumer(
      {} as RedisService,
      functions as unknown as FunctionsService,
    );
    (consumer as unknown as { client: unknown }).client = {
      get: (key: string) => Promise.resolve(store.get(key) ?? null),
      set: (key: string, value: string) => {
        store.set(key, value);
        return Promise.resolve('OK');
      },
    };
  });

  it('writes a final result and remembers its seq', async () => {
    await apply({
      invocationId: 'inv-1',
      executionId: 'exec-1',
      status: 'SUCCESS',
      output: { ok: true },
      durationMs: 30,
      attempts: 2,
      seq: 3,
    });

    expect(functions.updateInvocation).toHaveBeenCalledWith('inv-1', {
      status: 'SUCCESS',
      executionId: 'exec-1',
      output: { ok: true },
      error: null,
      durationMs: 30,
      attempts: 2,
    });
    expect(store.get('nimbus:async:applied:inv-1')).toBe('3');
  });

  it('ignores an older or repeated seq so the row never moves backwards', async () => {
    await apply({ invocationId: 'inv-1', status: 'SUCCESS', seq: 5 });
    await apply({ invocationId: 'inv-1', status: 'RUNNING', seq: 4 });
    await apply({ invocationId: 'inv-1', status: 'SUCCESS', seq: 5 });

    expect(functions.updateInvocation).toHaveBeenCalledTimes(1);
  });

  it('keeps the error of a failed attempt while it retries', async () => {
    await apply({
      invocationId: 'inv-1',
      status: 'RUNNING',
      attempts: 2,
      error: 'intento 1 falló',
      seq: 2,
    });

    expect(functions.updateInvocation).toHaveBeenCalledWith(
      'inv-1',
      expect.objectContaining({
        status: 'RUNNING',
        attempts: 2,
        error: 'intento 1 falló',
      }),
    );
  });

  it('resets the row on a DLQ redrive', async () => {
    await apply({ invocationId: 'inv-1', status: 'ERROR', redrive: true, seq: 9 });

    expect(functions.updateInvocation).toHaveBeenCalledWith('inv-1', {
      status: 'PENDING',
      executionId: undefined,
      clearOutput: true,
      error: null,
      durationMs: null,
      attempts: 0,
    });
  });

  it.each([
    ['invalid JSON', 'not json'],
    ['a missing seq', { invocationId: 'inv-1', status: 'SUCCESS' }],
    ['an unknown status', { invocationId: 'inv-1', status: 'DONE', seq: 1 }],
  ])('drops %s', async (_label, event) => {
    await apply(event);

    expect(functions.updateInvocation).not.toHaveBeenCalled();
  });

  it('acknowledges results for invocations that no longer exist', async () => {
    functions.findInvocation.mockResolvedValue(null);

    await apply({ invocationId: 'gone', status: 'SUCCESS', seq: 1 });

    expect(functions.updateInvocation).not.toHaveBeenCalled();
    expect(store.get('nimbus:async:applied:gone')).toBe('1');
  });
});
