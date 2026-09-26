import { Injectable, Logger } from '@nestjs/common';
import { v4 as uuidv4 } from 'uuid';

export interface RunnerResult {
  success: boolean;
  output: unknown;
  error?: string;
  exitCode: number;
  durationMs: number;
}

interface ComputePlaneRequest {
  executionId: string;
  functionId: string;
  version: string;
  event: Record<string, unknown>;
  timeoutSec?: number;
  memoryMb?: number;
  handler?: string;
}

interface ComputePlaneResponse {
  executionId: string;
  success: boolean;
  output: unknown;
  error?: string;
  exitCode: number;
  durationMs: number;
}

/**
 * RunnerService — Cliente HTTP hacia Go compute-plane (Slice 4)
 *
 * Este servicio envía solicitudes de ejecución al compute-plane Go,
 * que maneja Docker, timeouts, y concurrencia con Redis.
 *
 * Reemplaza el bridge temporal de Slice 3 que usaba run-artifact.sh.
 */
@Injectable()
export class RunnerService {
  private readonly logger = new Logger(RunnerService.name);
  private readonly computePlaneUrl: string;

  constructor() {
    this.computePlaneUrl =
      process.env.COMPUTE_PLANE_URL || 'http://compute-plane:8080';
  }

  /**
   * Ejecuta una función enviando la solicitud al compute-plane Go.
   *
   * @param functionId - ID de la función en Postgres
   * @param version - Tag de versión (e.g. "20240115-120000")
   * @param event - Evento JSON a pasar al handler
   */
  async invoke(
    functionId: string,
    version: string,
    event: Record<string, unknown>,
  ): Promise<RunnerResult> {
    const executionId = uuidv4();
    const startTime = Date.now();

    this.logger.log(
      `Enviando ejecución ${executionId} al compute-plane (fn: ${functionId}, v: ${version})`,
    );

    try {
      const request: ComputePlaneRequest = {
        executionId,
        functionId,
        version,
        event,
        timeoutSec: parseInt(process.env.NIMBUS_TIMEOUT_SEC || '5', 10),
        memoryMb: parseInt(
          (process.env.NIMBUS_MEMORY || '128m').replace('m', ''),
          10,
        ),
        handler: process.env.NIMBUS_HANDLER || 'handler',
      };

      const response = await fetch(`${this.computePlaneUrl}/executions`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
        },
        body: JSON.stringify(request),
      });

      if (response.status === 429) {
        const errorBody = await response.json();
        return {
          success: false,
          output: null,
          error: errorBody.error || 'Función ocupada, reintente más tarde',
          exitCode: 1,
          durationMs: Date.now() - startTime,
        };
      }

      const result: ComputePlaneResponse = await response.json();

      this.logger.log(
        `Ejecución ${executionId} completada (success: ${result.success}, exit: ${result.exitCode}, duration: ${result.durationMs}ms)`,
      );

      return {
        success: result.success,
        output: result.output,
        error: result.error,
        exitCode: result.exitCode,
        durationMs: result.durationMs,
      };
    } catch (error) {
      const errorMessage =
        error instanceof Error ? error.message : 'Error desconocido';
      this.logger.error(`Error llamando compute-plane: ${errorMessage}`);

      return {
        success: false,
        output: null,
        error: `Error comunicándose con compute-plane: ${errorMessage}`,
        exitCode: 1,
        durationMs: Date.now() - startTime,
      };
    }
  }

  /**
   * Cancela una ejecución en progreso.
   *
   * @param executionId - ID de la ejecución a cancelar
   */
  async cancel(executionId: string): Promise<boolean> {
    try {
      const response = await fetch(
        `${this.computePlaneUrl}/executions/${executionId}/cancel`,
        {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
          },
        },
      );

      if (!response.ok) {
        this.logger.warn(`No se pudo cancelar ejecución ${executionId}`);
        return false;
      }

      const result = await response.json();
      return result.cancelled === true;
    } catch (error) {
      this.logger.error(`Error cancelando ejecución: ${error}`);
      return false;
    }
  }
}
