import {
  Injectable,
  NotFoundException,
  ConflictException,
} from '@nestjs/common';
import { Prisma } from '@prisma/client';
import { PrismaService } from '../prisma/prisma.service';
import { MinioService } from '../minio/minio.service';
import { CreateFunctionDto } from './dto/create-function.dto';
import { v4 as uuidv4 } from 'uuid';

@Injectable()
export class FunctionsService {
  constructor(
    private readonly prisma: PrismaService,
    private readonly minio: MinioService,
  ) {}

  async create(dto: CreateFunctionDto) {
    const existing = await this.prisma.function.findUnique({
      where: { name: dto.name },
    });

    if (existing) {
      throw new ConflictException(`Función '${dto.name}' ya existe`);
    }

    const fn = await this.prisma.function.create({
      data: { name: dto.name },
    });

    console.log(`[functions] Función creada: ${fn.name} (${fn.id})`);
    return fn;
  }

  async findAll() {
    return this.prisma.function.findMany({
      include: {
        versions: {
          orderBy: { createdAt: 'desc' },
          take: 1,
        },
        _count: {
          select: { versions: true, invocations: true },
        },
      },
      orderBy: { createdAt: 'desc' },
    });
  }

  async findOne(id: string) {
    const fn = await this.prisma.function.findUnique({
      where: { id },
      include: {
        versions: {
          orderBy: { createdAt: 'desc' },
        },
        _count: {
          select: { invocations: true },
        },
      },
    });

    if (!fn) {
      throw new NotFoundException(`Función ${id} no encontrada`);
    }

    return fn;
  }

  async deploy(id: string, zipBuffer: Buffer) {
    const fn = await this.prisma.function.findUnique({ where: { id } });

    if (!fn) {
      throw new NotFoundException(`Función ${id} no encontrada`);
    }

    // El tag viejo era YYYYMMDD-HHMMSS. Dos deploys de la misma función en el
    // mismo segundo chocaban con @@unique(functionId, version) y el segundo
    // PutObject pisaba el zip de la versión que ya estaba en la base.
    for (let attempt = 0; attempt < 5; attempt++) {
      const versionTag = this.generateVersionTag();
      const taken = await this.prisma.version.findFirst({
        where: { functionId: id, version: versionTag },
        select: { id: true },
      });
      if (taken) continue;

      const artifactKey = await this.minio.uploadArtifact(id, versionTag, zipBuffer);
      try {
        const version = await this.prisma.version.create({
          data: {
            functionId: id,
            version: versionTag,
            artifactKey,
          },
        });

        console.log(`[functions] Deploy: ${fn.name} v${versionTag}`);

        return {
          function: fn,
          version,
        };
      } catch (err) {
        if (
          err instanceof Prisma.PrismaClientKnownRequestError &&
          err.code === 'P2002'
        ) {
          continue;
        }
        throw err;
      }
    }

    throw new ConflictException(
      `No se pudo asignar un tag de versión libre para '${fn.name}'`,
    );
  }

  async getLatestVersion(functionId: string) {
    const version = await this.prisma.version.findFirst({
      where: { functionId },
      orderBy: { createdAt: 'desc' },
    });

    return version;
  }

  async getVersionByTag(functionId: string, versionTag: string) {
    return this.prisma.version.findFirst({
      where: {
        functionId,
        version: versionTag,
      },
    });
  }

  async createInvocation(
    functionId: string,
    versionId: string | null,
    input: unknown,
  ) {
    return this.prisma.invocation.create({
      data: {
        functionId,
        versionId,
        requestId: uuidv4(),
        input: input as object,
        status: 'PENDING',
      },
    });
  }

  async findInvocation(id: string) {
    return this.prisma.invocation.findUnique({ where: { id } });
  }

  async updateInvocation(
    id: string,
    data: {
      status: 'PENDING' | 'RUNNING' | 'SUCCESS' | 'ERROR' | 'TIMEOUT';
      executionId?: string;
      output?: unknown;
      error?: string | null;
      durationMs?: number | null;
      attempts?: number | null;
      clearOutput?: boolean;
    },
  ) {
    const patch: Prisma.InvocationUpdateInput = { status: data.status };
    if (data.executionId !== undefined) {
      patch.executionId = data.executionId;
    }
    if (data.clearOutput) {
      patch.output = Prisma.DbNull;
    } else if (data.output !== undefined) {
      patch.output =
        data.output === null
          ? Prisma.DbNull
          : (data.output as Prisma.InputJsonValue);
    }
    if (data.error !== undefined) {
      patch.error = data.error;
    }
    if (data.durationMs !== undefined) {
      patch.durationMs = data.durationMs;
    }
    if (data.attempts !== undefined) {
      patch.attempts = data.attempts;
    }
    return this.prisma.invocation.update({
      where: { id },
      data: patch,
    });
  }

  private generateVersionTag(): string {
    const now = new Date();
    const pad = (n: number, width = 2) => n.toString().padStart(width, '0');
    const stamp =
      `${now.getFullYear()}${pad(now.getMonth() + 1)}${pad(now.getDate())}` +
      `-${pad(now.getHours())}${pad(now.getMinutes())}${pad(now.getSeconds())}` +
      pad(now.getMilliseconds(), 3);
    const suffix = Math.random().toString(36).slice(2, 6);
    return `${stamp}-${suffix}`;
  }
}
