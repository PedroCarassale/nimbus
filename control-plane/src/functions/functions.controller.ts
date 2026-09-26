import {
  Controller,
  Get,
  Post,
  Body,
  Param,
  UploadedFile,
  UseInterceptors,
  BadRequestException,
  HttpCode,
} from '@nestjs/common';
import { FileInterceptor } from '@nestjs/platform-express';
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
   * POST /functions/:id/invoke — Invocar función sincronamente
   *
   * Para Slice 3: usa el bridge temporal (RunnerService) que ejecuta
   * los scripts de Slice 1-2. En Slice 4 esto se reemplaza con Nest → Go.
   *
   * @param id - ID de la función
   * @param dto - Evento y versión opcional
   */
  @Post(':id/invoke')
  @HttpCode(200)
  async invoke(@Param('id') id: string, @Body() dto: InvokeFunctionDto) {
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

    console.log(
      `[invoke] Ejecutando ${fn.name} v${version.version} (requestId: ${invocation.requestId})`,
    );

    const result = await this.runnerService.invoke(
      fn.id,
      version.version,
      event,
    );

    await this.functionsService.updateInvocation(invocation.id, {
      status: 'RUNNING',
      executionId: result.executionId,
    });

    const status = result.success
      ? 'SUCCESS'
      : result.exitCode === 124
        ? 'TIMEOUT'
        : 'ERROR';

    await this.functionsService.updateInvocation(invocation.id, {
      status,
      output: result.output,
      error: result.error,
      durationMs: result.durationMs,
    });

    return {
      invocationId: invocation.id,
      requestId: invocation.requestId,
      executionId: result.executionId,
      status,
      durationMs: result.durationMs,
      output: result.output,
      error: result.error,
      logsUrl: `/invocations/${invocation.id}/logs`,
    };
  }
}
