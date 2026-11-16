import {
  Controller,
  Get,
  Post,
  Body,
  Param,
  Query,
  Headers,
  UploadedFile,
  UseInterceptors,
  BadRequestException,
  HttpException,
  HttpStatus,
  Res,
} from '@nestjs/common';
import { FileInterceptor } from '@nestjs/platform-express';
import { Response } from 'express';
import { FunctionsService } from './functions.service';
import { RunnerService } from './runner.service';
import { CreateFunctionDto } from './dto/create-function.dto';
import { InvokeFunctionDto } from './dto/invoke-function.dto';

@Controller('functions')
export class FunctionsController {
  constructor(
    private readonly functionsService: FunctionsService,
    private readonly runnerService: RunnerService,
  ) {}

  /**
   * POST /functions — Crear metadata de función
   */
  @Post()
  async create(@Body() dto: CreateFunctionDto) {
    const fn = await this.functionsService.create(dto);
    return {
      id: fn.id,
      name: fn.name,
      createdAt: fn.createdAt,
    };
  }

  /**
   * GET /functions — Listar todas las funciones
   */
  @Get()
  async findAll() {
    const functions = await this.functionsService.findAll();
    return functions.map((fn) => ({
      id: fn.id,
      name: fn.name,
      createdAt: fn.createdAt,
      latestVersion: fn.versions[0]?.version || null,
      versionsCount: fn._count.versions,
      invocationsCount: fn._count.invocations,
    }));
  }

  /**
   * GET /functions/:id — Obtener detalles de una función
   */
  @Get(':id')
  async findOne(@Param('id') id: string) {
    const fn = await this.functionsService.findOne(id);
    return {
      id: fn.id,
      name: fn.name,
      createdAt: fn.createdAt,
      updatedAt: fn.updatedAt,
      versions: fn.versions.map((v) => ({
        version: v.version,
        artifactKey: v.artifactKey,
        createdAt: v.createdAt,
      })),
      invocationsCount: fn._count.invocations,
    };
  }

  /**
   * POST /functions/:id/deploy — Subir zip y crear versión
   *
   * Recibe un archivo zip como multipart/form-data (campo "file").
   * Guarda la versión en Postgres y sube el artifact a MinIO.
   */
  @Post(':id/deploy')
  @UseInterceptors(FileInterceptor('file'))
  async deploy(
    @Param('id') id: string,
    @UploadedFile() file: Express.Multer.File,
  ) {
    if (!file) {
      throw new BadRequestException('Se requiere un archivo zip');
    }

    if (
      file.mimetype !== 'application/zip' &&
      file.mimetype !== 'application/x-zip-compressed' &&
      !file.originalname.endsWith('.zip')
    ) {
      throw new BadRequestException('El archivo debe ser un zip');
    }

    const result = await this.functionsService.deploy(id, file.buffer);

    return {
      message: 'Deploy exitoso',
      function: {
        id: result.function.id,
        name: result.function.name,
      },
      version: {
        version: result.version.version,
        artifactKey: result.version.artifactKey,
        createdAt: result.version.createdAt,
      },
    };
  }

  /**
   * POST /functions/:id/invoke — Invocar una función.
   *
   * Sin flags responde 200 cuando la ejecución termina (sync).
   * Con `?async=true` o `X-Invocation-Type: Event` responde 202 y el
   * compute plane ejecuta en segundo plano.
   */
  @Post(':id/invoke')
  async invoke(
    @Param('id') id: string,
    @Body() dto: InvokeFunctionDto,
    @Query('async') asyncQuery: string | undefined,
    @Headers('x-invocation-type') invocationType: string | undefined,
    @Res() res: Response,
  ) {
    const fn = await this.functionsService.findOne(id);

    let version = dto.version
      ? await this.functionsService.getVersionByTag(id, dto.version)
      : await this.functionsService.getLatestVersion(id);

    if (!version) {
      throw new BadRequestException(
        dto.version
          ? `Versión '${dto.version}' no encontrada para función ${fn.name}`
          : `La función ${fn.name} no tiene versiones desplegadas`,
      );
    }

    const event = dto.event || {};

    const invocation = await this.functionsService.createInvocation(
      id,
      version.id,
      event,
    );

    if (wantsAsync(asyncQuery, invocationType)) {
      console.log(
        `[invoke] Encolando ${fn.name} v${version.version} (requestId: ${invocation.requestId})`,
      );
      const enqueued = await this.runnerService.enqueue(
        invocation.id,
        fn.id,
        version.version,
        event,
      );
      if (enqueued.httpStatus === 400) {
        await this.functionsService.updateInvocation(invocation.id, {
          status: 'ERROR',
          executionId: enqueued.executionId,
          error: enqueued.error,
          attempts: 0,
        });
        throw new BadRequestException({
          message: enqueued.error || 'Configuración inválida',
          field: enqueued.field,
          invocationId: invocation.id,
        });
      }
      if (enqueued.httpStatus !== 202) {
        await this.functionsService.updateInvocation(invocation.id, {
          status: 'ERROR',
          executionId: enqueued.executionId,
          error: enqueued.error,
        });
        throw new HttpException(
          {
            statusCode: HttpStatus.BAD_GATEWAY,
            error: 'Bad Gateway',
            message: enqueued.error || 'No se pudo encolar la invocación',
            invocationId: invocation.id,
            requestId: invocation.requestId,
          },
          HttpStatus.BAD_GATEWAY,
        );
      }
      await this.functionsService.updateInvocation(invocation.id, {
        status: 'PENDING',
        executionId: enqueued.executionId,
        attempts: 0,
      });
      res.status(HttpStatus.ACCEPTED).json({
        invocationId: invocation.id,
        requestId: invocation.requestId,
        executionId: enqueued.executionId,
        status: 'PENDING',
        logsUrl: `/invocations/${invocation.id}/logs`,
      });
      return;
    }

    console.log(
      `[invoke] Ejecutando ${fn.name} v${version.version} (requestId: ${invocation.requestId})`,
    );

    await this.functionsService.updateInvocation(invocation.id, {
      status: 'RUNNING',
    });

    const result = await this.runnerService.invoke(
      fn.id,
      version.version,
      event,
    );

    if (result.httpStatus === 429) {
      await this.functionsService.updateInvocation(invocation.id, {
        status: 'ERROR',
        executionId: result.executionId,
        error: result.error,
        durationMs: result.durationMs,
      });
      throw new HttpException(
        {
          statusCode: HttpStatus.TOO_MANY_REQUESTS,
          error: 'Too Many Requests',
          message:
            result.error ||
            'Demasiadas invocaciones en cola. Reintentá más tarde.',
          invocationId: invocation.id,
          requestId: invocation.requestId,
          executionId: result.executionId,
          queuedMs: result.queuedMs ?? 0,
        },
        HttpStatus.TOO_MANY_REQUESTS,
      );
    }

    const status = result.success
      ? 'SUCCESS'
      : result.exitCode === 124
        ? 'TIMEOUT'
        : 'ERROR';

    await this.functionsService.updateInvocation(invocation.id, {
      status,
      executionId: result.executionId,
      output: result.output,
      error: result.error,
      durationMs: result.durationMs,
      attempts: 1,
    });

    res.status(HttpStatus.OK).json({
      invocationId: invocation.id,
      requestId: invocation.requestId,
      executionId: result.executionId,
      status,
      durationMs: result.durationMs,
      output: result.output,
      error: result.error,
      queuedMs: result.queuedMs ?? 0,
      logsUrl: `/invocations/${invocation.id}/logs`,
    });
  }
}

function wantsAsync(query?: string, header?: string): boolean {
  if (header && header.trim().toLowerCase() === 'event') {
    return true;
  }
  if (!query) {
    return false;
  }
  const value = query.trim().toLowerCase();
  return value === '1' || value === 'true' || value === 'yes';
}
