import { Injectable } from '@nestjs/common';
import { spawn } from 'child_process';
import * as fs from 'fs';
import * as path from 'path';
import * as os from 'os';

export interface RunnerResult {
  success: boolean;
  output: unknown;
  error?: string;
  exitCode: number;
  durationMs: number;
}

/**
 * RunnerService — Bridge temporal para Slice 3
 *
 * Este servicio ejecuta funciones usando los scripts de Slice 1-2 (run-artifact.sh).
 * En Slice 4, este bridge será reemplazado por llamadas Nest → Go (compute plane).
 *
 * TODO(slice-4): Reemplazar este servicio con cliente HTTP/gRPC hacia compute-plane Go.
 * El compute plane manejará Docker, timeouts, y orquestación de forma más robusta.
 */
@Injectable()
export class RunnerService {
  private readonly scriptsPath: string;

  constructor() {
    this.scriptsPath =
      process.env.SCRIPTS_PATH || '/app/scripts';
  }

  /**
   * Ejecuta una función descargando el artifact de MinIO y corriendo run-artifact.sh
   *
   * @param functionId - ID de la función en Postgres
   * @param version - Tag de versión (e.g. "20240115-120000") o "latest"
   * @param event - Evento JSON a pasar al handler
   */
  async invoke(
    functionId: string,
    version: string,
    event: Record<string, unknown>,
  ): Promise<RunnerResult> {
    const startTime = Date.now();

    const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), 'nimbus-invoke-'));
    const eventPath = path.join(tempDir, 'event.json');

    try {
      fs.writeFileSync(eventPath, JSON.stringify(event));

      const result = await this.runArtifactScript(
        functionId,
        version,
        eventPath,
      );

      const durationMs = Date.now() - startTime;

      return {
        ...result,
        durationMs,
      };
    } finally {
      fs.rmSync(tempDir, { recursive: true, force: true });
    }
  }

  private runArtifactScript(
    functionId: string,
    version: string,
    eventPath: string,
  ): Promise<Omit<RunnerResult, 'durationMs'>> {
    return new Promise((resolve) => {
      const scriptPath = path.join(this.scriptsPath, 'run-artifact.sh');

      const proc = spawn(scriptPath, [functionId, version, eventPath], {
        env: {
          ...process.env,
          AWS_ACCESS_KEY_ID: process.env.MINIO_ACCESS_KEY || 'minioadmin',
          AWS_SECRET_ACCESS_KEY: process.env.MINIO_SECRET_KEY || 'minioadmin',
          AWS_ENDPOINT_URL: process.env.MINIO_ENDPOINT || 'http://minio:9000',
          MINIO_BUCKET: process.env.MINIO_BUCKET || 'nimbus-artifacts',
        },
        stdio: ['pipe', 'pipe', 'pipe'],
      });

      let stdout = '';
      let stderr = '';

      proc.stdout.on('data', (data) => {
        stdout += data.toString();
      });

      proc.stderr.on('data', (data) => {
        stderr += data.toString();
      });

      proc.on('close', (code) => {
        const exitCode = code ?? 1;

        if (exitCode === 124) {
          resolve({
            success: false,
            output: null,
            error: 'Timeout excedido',
            exitCode,
          });
          return;
        }

        const output = this.parseOutput(stdout);

        if (exitCode === 0 && output !== null) {
          resolve({
            success: true,
            output,
            exitCode,
          });
        } else {
          resolve({
            success: false,
            output,
            error: stderr || 'Error en la ejecución',
            exitCode,
          });
        }
      });

      proc.on('error', (err) => {
        resolve({
          success: false,
          output: null,
          error: `Error ejecutando script: ${err.message}`,
          exitCode: 1,
        });
      });
    });
  }

  private parseOutput(stdout: string): unknown {
    const lines = stdout.trim().split('\n');

    for (let i = lines.length - 1; i >= 0; i--) {
      const line = lines[i].trim();
      if (line.startsWith('{') || line.startsWith('[')) {
        try {
          return JSON.parse(line);
        } catch {
          continue;
        }
      }
    }

    return null;
  }
}
