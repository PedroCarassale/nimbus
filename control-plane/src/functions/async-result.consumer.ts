import {
  Injectable,
  Logger,
  OnModuleDestroy,
  OnModuleInit,
} from '@nestjs/common';
import Redis from 'ioredis';
import { RedisService } from '../redis/redis.service';
import { FunctionsService } from './functions.service';

const RESULTS_STREAM = 'nimbus:async:results';
const RESULTS_GROUP = 'nimbus-control';

interface ResultEvent {
  invocationId?: string;
  executionId?: string;
  status?: string;
  output?: unknown;
  error?: string;
  exitCode?: number;
  durationMs?: number;
  attempts?: number;
  redrive?: boolean;
  seq?: number;
}

type InvocationStatus = 'PENDING' | 'RUNNING' | 'SUCCESS' | 'ERROR' | 'TIMEOUT';

/**
 * Applies compute-plane results onto the Invocation row.
 * A seq per invocation drops reclaimed copies that would move the row backwards.
 */
@Injectable()
export class AsyncResultConsumer implements OnModuleInit, OnModuleDestroy {
  private readonly logger = new Logger(AsyncResultConsumer.name);
  private client: Redis | null = null;
  private stopped = false;

  constructor(
    private readonly redis: RedisService,
    private readonly functions: FunctionsService,
  ) {}

  onModuleInit() {
    const consumer = `control-${process.pid}`;
    void this.loop(consumer);
  }

  async onModuleDestroy() {
    this.stopped = true;
    if (this.client) {
      this.client.disconnect();
    }
  }

  private async loop(consumer: string) {
    while (!this.stopped) {
      try {
        if (!this.client) {
          this.client = this.redis.getClient().duplicate({ lazyConnect: true });
          await this.client.connect();
          await this.ensureGroup();
        }
        await this.claimPending(consumer);
        const reply = await this.client.call(
          'XREADGROUP',
          'GROUP',
          RESULTS_GROUP,
          consumer,
          'COUNT',
          '20',
          'BLOCK',
          '2000',
          'STREAMS',
          RESULTS_STREAM,
          '>',
        );
        for (const message of readMessages(reply)) {
          await this.consume(message.id, message.data);
        }
      } catch (err) {
        if (this.stopped) {
          return;
        }
        this.logger.error(`Consumidor de resultados: ${err}`);
        this.client?.disconnect();
        this.client = null;
        await sleep(500);
      }
    }
  }

  private async ensureGroup() {
    try {
      await this.client?.call(
        'XGROUP',
        'CREATE',
        RESULTS_STREAM,
        RESULTS_GROUP,
        '0',
        'MKSTREAM',
      );
    } catch (err) {
      if (!String(err).includes('BUSYGROUP')) {
        throw err;
      }
    }
  }

  private async claimPending(consumer: string) {
    const reply = await this.client?.call(
      'XAUTOCLAIM',
      RESULTS_STREAM,
      RESULTS_GROUP,
      consumer,
      '5000',
      '0-0',
      'COUNT',
      '20',
    );
    if (!Array.isArray(reply) || reply.length < 2) {
      return;
    }
    for (const message of readMessages([[RESULTS_STREAM, reply[1]]])) {
      await this.consume(message.id, message.data);
    }
  }

  private async consume(id: string, data: string) {
    await this.apply(data);
    await this.client?.call('XACK', RESULTS_STREAM, RESULTS_GROUP, id);
  }

  private async apply(data: string) {
    let event: ResultEvent;
    try {
      event = JSON.parse(data) as ResultEvent;
    } catch {
      return;
    }
    if (!event.invocationId || !event.status || !event.seq) {
      return;
    }
    const appliedKey = `nimbus:async:applied:${event.invocationId}`;
    const previous = Number((await this.client?.get(appliedKey)) || '0');
    if (event.seq <= previous) {
      return;
    }

    const existing = await this.functions.findInvocation(event.invocationId);
    if (!existing) {
      await this.client?.set(appliedKey, String(event.seq));
      return;
    }

    const status = event.status as InvocationStatus;
    if (!isStatus(status)) {
      return;
    }

    if (event.redrive || status === 'PENDING') {
      await this.functions.updateInvocation(event.invocationId, {
        status: 'PENDING',
        executionId: event.executionId,
        clearOutput: true,
        error: null,
        durationMs: null,
        attempts: 0,
      });
    } else if (status === 'RUNNING') {
      await this.functions.updateInvocation(event.invocationId, {
        status: 'RUNNING',
        executionId: event.executionId,
        attempts: numberOrUndefined(event.attempts),
        ...(event.error ? { error: event.error } : {}),
      });
    } else {
      await this.functions.updateInvocation(event.invocationId, {
        status,
        executionId: event.executionId,
        output: event.output === undefined ? null : event.output,
        error: status === 'SUCCESS' ? null : event.error || null,
        durationMs: numberOrNull(event.durationMs),
        attempts: numberOrUndefined(event.attempts),
      });
    }
    await this.client?.set(appliedKey, String(event.seq));
  }
}

function isStatus(value: string): value is InvocationStatus {
  return (
    value === 'PENDING' ||
    value === 'RUNNING' ||
    value === 'SUCCESS' ||
    value === 'ERROR' ||
    value === 'TIMEOUT'
  );
}

function numberOrUndefined(value: unknown): number | undefined {
  return typeof value === 'number' ? value : undefined;
}

function numberOrNull(value: unknown): number | null {
  return typeof value === 'number' ? value : null;
}

function readMessages(reply: unknown): { id: string; data: string }[] {
  if (!Array.isArray(reply)) {
    return [];
  }
  const streams = Array.isArray(reply[0]) ? reply : [reply];
  const out: { id: string; data: string }[] = [];
  for (const stream of streams) {
    if (!Array.isArray(stream) || stream.length < 2) {
      continue;
    }
    const messages = stream[1];
    if (!Array.isArray(messages)) {
      continue;
    }
    for (const message of messages) {
      if (!Array.isArray(message) || message.length < 2) {
        continue;
      }
      const fields = message[1];
      if (!Array.isArray(fields)) {
        continue;
      }
      for (let i = 0; i < fields.length - 1; i += 2) {
        if (fields[i] === 'data') {
          out.push({ id: String(message[0]), data: String(fields[i + 1]) });
        }
      }
    }
  }
  return out;
}

function sleep(ms: number) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
