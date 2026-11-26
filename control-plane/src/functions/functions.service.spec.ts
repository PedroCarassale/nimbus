import { ConflictException, NotFoundException } from '@nestjs/common';
import { Prisma } from '@prisma/client';
import { FunctionsService } from './functions.service';
import { PrismaService } from '../prisma/prisma.service';
import { MinioService } from '../minio/minio.service';

function makePrisma() {
  return {
    function: {
      findUnique: jest.fn(),
      findMany: jest.fn(),
      create: jest.fn(),
    },
    version: {
      findFirst: jest.fn(),
      create: jest.fn(),
    },
    invocation: {
      create: jest.fn(),
      findUnique: jest.fn(),
      update: jest.fn(),
    },
  };
}

function uniqueViolation() {
  return new Prisma.PrismaClientKnownRequestError('Unique constraint failed', {
    code: 'P2002',
    clientVersion: 'test',
  });
}

describe('FunctionsService', () => {
  let prisma: ReturnType<typeof makePrisma>;
  let minio: { uploadArtifact: jest.Mock };
  let service: FunctionsService;

  beforeEach(() => {
    prisma = makePrisma();
    minio = { uploadArtifact: jest.fn() };
    service = new FunctionsService(
      prisma as unknown as PrismaService,
      minio as unknown as MinioService,
    );
    jest.spyOn(console, 'log').mockImplementation(() => undefined);
  });

  afterEach(() => jest.restoreAllMocks());

  describe('create', () => {
    it('creates the function when the name is free', async () => {
      prisma.function.findUnique.mockResolvedValue(null);
      prisma.function.create.mockResolvedValue({ id: 'fn-1', name: 'hello' });

      const fn = await service.create({ name: 'hello' });

      expect(fn).toEqual({ id: 'fn-1', name: 'hello' });
      expect(prisma.function.create).toHaveBeenCalledWith({
        data: { name: 'hello' },
      });
    });

    it('rejects a duplicated name with 409', async () => {
      prisma.function.findUnique.mockResolvedValue({ id: 'fn-1', name: 'hello' });

      await expect(service.create({ name: 'hello' })).rejects.toBeInstanceOf(
        ConflictException,
      );
      expect(prisma.function.create).not.toHaveBeenCalled();
    });
  });

  describe('findOne', () => {
    it('throws 404 when the function does not exist', async () => {
      prisma.function.findUnique.mockResolvedValue(null);

      await expect(service.findOne('missing')).rejects.toBeInstanceOf(
        NotFoundException,
      );
    });
  });

  describe('deploy', () => {
    const zip = Buffer.from('zip');

    it('uploads the artifact and stores a new version', async () => {
      prisma.function.findUnique.mockResolvedValue({ id: 'fn-1', name: 'hello' });
      prisma.version.findFirst.mockResolvedValue(null);
      minio.uploadArtifact.mockResolvedValue('functions/fn-1/versions/v1/code.zip');
      prisma.version.create.mockImplementation(({ data }) =>
        Promise.resolve({ id: 'ver-1', ...data }),
      );

      const result = await service.deploy('fn-1', zip);

      const tag = minio.uploadArtifact.mock.calls[0][1] as string;
      expect(tag).toMatch(/^\d{8}-\d{9}-[a-z0-9]{1,4}$/);
      expect(minio.uploadArtifact).toHaveBeenCalledWith('fn-1', tag, zip);
      expect(result.version).toMatchObject({
        functionId: 'fn-1',
        version: tag,
        artifactKey: 'functions/fn-1/versions/v1/code.zip',
      });
    });

    it('retries with a new tag when the version insert collides', async () => {
      prisma.function.findUnique.mockResolvedValue({ id: 'fn-1', name: 'hello' });
      prisma.version.findFirst.mockResolvedValue(null);
      minio.uploadArtifact.mockResolvedValue('key');
      prisma.version.create
        .mockRejectedValueOnce(uniqueViolation())
        .mockResolvedValueOnce({ id: 'ver-2', version: 'v2', artifactKey: 'key' });

      const result = await service.deploy('fn-1', zip);

      expect(prisma.version.create).toHaveBeenCalledTimes(2);
      expect(result.version.id).toBe('ver-2');
    });

    it('gives up with 409 after five taken tags', async () => {
      prisma.function.findUnique.mockResolvedValue({ id: 'fn-1', name: 'hello' });
      prisma.version.findFirst.mockResolvedValue({ id: 'taken' });

      await expect(service.deploy('fn-1', zip)).rejects.toBeInstanceOf(
        ConflictException,
      );
      expect(prisma.version.findFirst).toHaveBeenCalledTimes(5);
      expect(minio.uploadArtifact).not.toHaveBeenCalled();
    });

    it('does not retry on errors other than a unique violation', async () => {
      prisma.function.findUnique.mockResolvedValue({ id: 'fn-1', name: 'hello' });
      prisma.version.findFirst.mockResolvedValue(null);
      minio.uploadArtifact.mockResolvedValue('key');
      prisma.version.create.mockRejectedValue(new Error('db down'));

      await expect(service.deploy('fn-1', zip)).rejects.toThrow('db down');
      expect(prisma.version.create).toHaveBeenCalledTimes(1);
    });

    it('throws 404 for an unknown function', async () => {
      prisma.function.findUnique.mockResolvedValue(null);

      await expect(service.deploy('missing', zip)).rejects.toBeInstanceOf(
        NotFoundException,
      );
    });
  });

  describe('createInvocation', () => {
    it('creates a PENDING row with a fresh requestId', async () => {
      prisma.invocation.create.mockImplementation(({ data }) =>
        Promise.resolve({ id: 'inv-1', ...data }),
      );

      const inv = await service.createInvocation('fn-1', 'ver-1', { a: 1 });

      expect(inv).toMatchObject({
        functionId: 'fn-1',
        versionId: 'ver-1',
        input: { a: 1 },
        status: 'PENDING',
      });
      expect(inv.requestId).toMatch(/^[0-9a-f-]{36}$/);
    });
  });

  describe('updateInvocation', () => {
    beforeEach(() => prisma.invocation.update.mockResolvedValue({}));

    it('only sends the fields that were provided', async () => {
      await service.updateInvocation('inv-1', { status: 'RUNNING' });

      expect(prisma.invocation.update).toHaveBeenCalledWith({
        where: { id: 'inv-1' },
        data: { status: 'RUNNING' },
      });
    });

    it('maps a null output and clearOutput to a database NULL', async () => {
      await service.updateInvocation('inv-1', { status: 'ERROR', output: null });
      await service.updateInvocation('inv-1', {
        status: 'PENDING',
        output: { stale: true },
        clearOutput: true,
      });

      expect(prisma.invocation.update.mock.calls[0][0].data.output).toBe(
        Prisma.DbNull,
      );
      expect(prisma.invocation.update.mock.calls[1][0].data.output).toBe(
        Prisma.DbNull,
      );
    });

    it('keeps the result fields of a finished invocation', async () => {
      await service.updateInvocation('inv-1', {
        status: 'SUCCESS',
        executionId: 'exec-1',
        output: { ok: true },
        error: null,
        durationMs: 120,
        attempts: 1,
      });

      expect(prisma.invocation.update).toHaveBeenCalledWith({
        where: { id: 'inv-1' },
        data: {
          status: 'SUCCESS',
          executionId: 'exec-1',
          output: { ok: true },
          error: null,
          durationMs: 120,
          attempts: 1,
        },
      });
    });
  });
});
