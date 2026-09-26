import { Injectable, NotFoundException } from '@nestjs/common';
import { PrismaService } from '../prisma/prisma.service';
import { RedisService, LogEntry } from '../redis/redis.service';

@Injectable()
export class InvocationsService {
  constructor(
    private readonly prisma: PrismaService,
    private readonly redis: RedisService,
  ) {}

  async findOne(id: string) {
    const invocation = await this.prisma.invocation.findUnique({
      where: { id },
      include: {
        function: true,
        version: true,
      },
    });

    if (!invocation) {
      throw new NotFoundException(`Invocación ${id} no encontrada`);
    }

    return invocation;
  }

  async findByRequestId(requestId: string) {
    const invocation = await this.prisma.invocation.findUnique({
      where: { requestId },
      include: {
        function: true,
        version: true,
      },
    });

    if (!invocation) {
      throw new NotFoundException(
        `Invocación con requestId ${requestId} no encontrada`,
      );
    }

    return invocation;
  }

  async getExecutionIdForInvocation(invocationId: string): Promise<string> {
    const invocation = await this.prisma.invocation.findUnique({
      where: { id: invocationId },
      select: { executionId: true, requestId: true },
    });

    if (!invocation) {
      throw new NotFoundException(`Invocación ${invocationId} no encontrada`);
    }

    return invocation.executionId || invocation.requestId;
  }

  async getLogs(invocationId: string): Promise<LogEntry[]> {
    const executionId = await this.getExecutionIdForInvocation(invocationId);
    return this.redis.readLogStream(executionId);
  }

  async *subscribeLogs(
    invocationId: string,
    lastId?: string,
  ): AsyncGenerator<LogEntry, void, unknown> {
    const executionId = await this.getExecutionIdForInvocation(invocationId);
    yield* this.redis.subscribeToLogStream(executionId, lastId);
  }

  async logsExist(invocationId: string): Promise<boolean> {
    try {
      const executionId = await this.getExecutionIdForInvocation(invocationId);
      return this.redis.streamExists(executionId);
    } catch {
      return false;
    }
  }
}
