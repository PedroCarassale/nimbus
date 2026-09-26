import {
  Controller,
  Get,
  Param,
  Res,
  Query,
  NotFoundException,
  Logger,
} from '@nestjs/common';
import { Response } from 'express';
import { InvocationsService } from './invocations.service';

@Controller('invocations')
export class InvocationsController {
  private readonly logger = new Logger(InvocationsController.name);

  constructor(private readonly invocationsService: InvocationsService) {}

  /**
   * GET /invocations/:id — Get invocation details
   */
  @Get(':id')
  async findOne(@Param('id') id: string) {
    const invocation = await this.invocationsService.findOne(id);
    return {
      id: invocation.id,
      requestId: invocation.requestId,
      executionId: invocation.executionId,
      functionId: invocation.functionId,
      functionName: invocation.function.name,
      versionId: invocation.versionId,
      version: invocation.version?.version,
      status: invocation.status,
      input: invocation.input,
      output: invocation.output,
      error: invocation.error,
      durationMs: invocation.durationMs,
      createdAt: invocation.createdAt,
      updatedAt: invocation.updatedAt,
    };
  }

  /**
   * GET /invocations/:id/logs — Server-Sent Events stream of logs
   *
   * Returns live logs as SSE events. Each event has:
   * - event: "log" for log lines, "end" when execution completes
   * - data: JSON with timestamp, stream (stdout/stderr), and line
   *
   * Query params:
   * - follow=true (default): keep connection open for live logs
   * - follow=false: return existing logs and close
   * - lastId: resume from specific message ID
   *
   * Usage: curl -N http://localhost:3000/invocations/{id}/logs
   */
  @Get(':id/logs')
  async streamLogs(
    @Param('id') id: string,
    @Query('follow') follow: string = 'true',
    @Query('lastId') lastId?: string,
    @Res() res?: Response,
  ) {
    if (!res) return;

    const invocation = await this.invocationsService.findOne(id);
    const shouldFollow = follow !== 'false';

    this.logger.log(
      `SSE logs requested for invocation ${id} (follow: ${shouldFollow})`,
    );

    res.setHeader('Content-Type', 'text/event-stream');
    res.setHeader('Cache-Control', 'no-cache');
    res.setHeader('Connection', 'keep-alive');
    res.setHeader('X-Accel-Buffering', 'no');
    res.flushHeaders();

    const sendEvent = (event: string, data: unknown) => {
      res.write(`event: ${event}\n`);
      res.write(`data: ${JSON.stringify(data)}\n\n`);
    };

    sendEvent('connected', {
      invocationId: id,
      requestId: invocation.requestId,
      executionId: invocation.executionId,
      status: invocation.status,
    });

    if (!shouldFollow) {
      const logs = await this.invocationsService.getLogs(id);
      for (const entry of logs) {
        if (entry.stream === '_end') {
          sendEvent('end', { message: 'Execution complete' });
        } else {
          sendEvent('log', {
            timestamp: entry.timestamp,
            stream: entry.stream,
            line: entry.line,
            sequence: entry.sequence,
          });
        }
      }
      res.end();
      return;
    }

    let closed = false;
    res.on('close', () => {
      closed = true;
      this.logger.log(`SSE connection closed for invocation ${id}`);
    });

    try {
      const logsGenerator = this.invocationsService.subscribeLogs(id, lastId);

      for await (const entry of logsGenerator) {
        if (closed) break;

        if (entry.stream === '_end') {
          sendEvent('end', { message: 'Execution complete' });
          break;
        } else {
          sendEvent('log', {
            timestamp: entry.timestamp,
            stream: entry.stream,
            line: entry.line,
            sequence: entry.sequence,
          });
        }
      }
    } catch (err) {
      if (!closed) {
        if (err instanceof NotFoundException) {
          sendEvent('error', { message: err.message });
        } else {
          this.logger.error(`Error streaming logs: ${err}`);
          sendEvent('error', { message: 'Internal error streaming logs' });
        }
      }
    }

    if (!closed) {
      res.end();
    }
  }
}
