import {
  Injectable,
  NotFoundException,
  ConflictException,
} from '@nestjs/common';
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

    const versionTag = this.generateVersionTag();
    const artifactKey = await this.minio.uploadArtifact(id, versionTag, zipBuffer);

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

  async updateInvocation(
    id: string,
    data: {
      status: 'RUNNING' | 'SUCCESS' | 'ERROR' | 'TIMEOUT';
      output?: unknown;
      error?: string;
      durationMs?: number;
    },
  ) {
    return this.prisma.invocation.update({
      where: { id },
      data: {
        status: data.status,
        output: data.output as object | undefined,
        error: data.error,
        durationMs: data.durationMs,
      },
    });
  }

  private generateVersionTag(): string {
    const now = new Date();
    const pad = (n: number) => n.toString().padStart(2, '0');
    return `${now.getFullYear()}${pad(now.getMonth() + 1)}${pad(now.getDate())}-${pad(now.getHours())}${pad(now.getMinutes())}${pad(now.getSeconds())}`;
  }
}
