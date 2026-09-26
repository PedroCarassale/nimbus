import { Injectable, OnModuleDestroy, Logger } from '@nestjs/common';
import Redis from 'ioredis';

export interface LogEntry {
  timestamp: number;
  executionId: string;
  stream: string;
  line: string;
  sequence: number;
}

@Injectable()
export class RedisService implements OnModuleDestroy {
  private readonly logger = new Logger(RedisService.name);
  private readonly client: Redis;

  constructor() {
    const redisUrl = process.env.REDIS_URL || 'redis:6379';
    const [host, port] = redisUrl.split(':');

    this.client = new Redis({
      host,
      port: parseInt(port || '6379', 10),
      password: process.env.REDIS_PASSWORD || undefined,
      lazyConnect: true,
      retryStrategy: (times) => {
        if (times > 10) return null;
        return Math.min(times * 100, 3000);
      },
    });

    this.client.on('error', (err) => {
      this.logger.error(`Redis error: ${err.message}`);
    });

    this.client.on('connect', () => {
      this.logger.log('Connected to Redis');
    });

    this.client.connect().catch((err) => {
      this.logger.warn(`Initial Redis connection failed: ${err.message}`);
    });
  }

  async onModuleDestroy() {
    await this.client.quit();
  }

  getClient(): Redis {
    return this.client;
  }

  getStreamKey(executionId: string): string {
    return `nimbus:logs:${executionId}`;
  }

  async readLogStream(executionId: string): Promise<LogEntry[]> {
    const streamKey = this.getStreamKey(executionId);
    const entries: LogEntry[] = [];

    try {
      const messages = await this.client.xrange(streamKey, '-', '+');

      for (const msg of messages) {
        const [, fields] = msg;
        if (Array.isArray(fields)) {
          const dataIndex = fields.indexOf('data');
          if (dataIndex !== -1 && dataIndex + 1 < fields.length) {
            try {
              const entry = JSON.parse(fields[dataIndex + 1]) as LogEntry;
              entries.push(entry);
            } catch {
              continue;
            }
          }
        }
      }
    } catch (err) {
      this.logger.error(`Error reading stream ${streamKey}: ${err}`);
    }

    return entries;
  }

  async *subscribeToLogStream(
    executionId: string,
    lastId = '0',
  ): AsyncGenerator<LogEntry, void, unknown> {
    const streamKey = this.getStreamKey(executionId);
    let currentId = lastId;
    let endReached = false;

    while (!endReached) {
      try {
        const result = await this.client.call(
          'XREAD',
          'BLOCK',
          '1000',
          'COUNT',
          '100',
          'STREAMS',
          streamKey,
          currentId,
        );

        if (!result) continue;

        const streams = result as [string, [string, string[]][]][];
        for (const [, messages] of streams) {
          for (const [msgId, fields] of messages) {
            const dataIndex = fields.indexOf('data');
            if (dataIndex !== -1 && dataIndex + 1 < fields.length) {
              try {
                const entry = JSON.parse(fields[dataIndex + 1]) as LogEntry;
                currentId = msgId;
                yield entry;

                if (entry.stream === '_end') {
                  endReached = true;
                  break;
                }
              } catch {
                continue;
              }
            }
          }
        }
      } catch (err) {
        if ((err as Error).message?.includes('Connection is closed')) {
          break;
        }
        this.logger.error(`Error in stream subscription: ${err}`);
        await new Promise((resolve) => setTimeout(resolve, 100));
      }
    }
  }

  async streamExists(executionId: string): Promise<boolean> {
    const streamKey = this.getStreamKey(executionId);
    const exists = await this.client.exists(streamKey);
    return exists > 0;
  }
}
