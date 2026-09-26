import { Module } from '@nestjs/common';
import { PrismaModule } from './prisma/prisma.module';
import { MinioModule } from './minio/minio.module';
import { FunctionsModule } from './functions/functions.module';
import { HealthController } from './health.controller';

@Module({
  imports: [PrismaModule, MinioModule, FunctionsModule],
  controllers: [HealthController],
})
export class AppModule {}
