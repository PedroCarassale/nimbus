import { NotFoundException } from '@nestjs/common';
import { InvocationsService } from './invocations.service';
import { PrismaService } from '../prisma/prisma.service';
import { RedisService } from '../redis/redis.service';

describe('InvocationsService', () => {
  let prisma: { invocation: { findUnique: jest.Mock } };
  let redis: { readLogStream: jest.Mock; streamExists: jest.Mock };
  let service: InvocationsService;

  beforeEach(() => {
    prisma = { invocation: { findUnique: jest.fn() } };
    redis = { readLogStream: jest.fn(), streamExists: jest.fn() };
    service = new InvocationsService(
      prisma as unknown as PrismaService,
      redis as unknown as RedisService,
    );
  });

  it('throws 404 for an unknown invocation', async () => {
    prisma.invocation.findUnique.mockResolvedValue(null);

    await expect(service.findOne('missing')).rejects.toBeInstanceOf(
      NotFoundException,
    );
  });

  it('reads logs from the stream of the execution id', async () => {
    prisma.invocation.findUnique.mockResolvedValue({
      executionId: 'exec-1',
      requestId: 'req-1',
    });
    redis.readLogStream.mockResolvedValue([{ stream: 'stdout', line: 'hola' }]);

    const logs = await service.getLogs('inv-1');

    expect(redis.readLogStream).toHaveBeenCalledWith('exec-1');
    expect(logs).toEqual([{ stream: 'stdout', line: 'hola' }]);
  });

  it('falls back to the request id for rows without an execution id', async () => {
    prisma.invocation.findUnique.mockResolvedValue({
      executionId: null,
      requestId: 'req-1',
    });
    redis.readLogStream.mockResolvedValue([]);

    await service.getLogs('inv-1');

    expect(redis.readLogStream).toHaveBeenCalledWith('req-1');
  });

  it('reports missing logs as false instead of throwing', async () => {
    prisma.invocation.findUnique.mockResolvedValue(null);

    await expect(service.logsExist('missing')).resolves.toBe(false);
  });
});
