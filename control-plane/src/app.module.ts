import { Module } from '@nestjs/common';
import { PrismaModule } from './prisma/prisma.module';
import { MinioModule } from './minio/minio.module';
import { RedisModule } from './redis/redis.module';
import { FunctionsModule } from './functions/functions.module';
import { InvocationsModule } from './invocations/invocations.module';
import { HealthController } from './health.controller';

@Module({
  imports: [
    PrismaModule,
    MinioModule,
    RedisModule,
    FunctionsModule,
    InvocationsModule,
  ],
  controllers: [HealthController],
})
export class AppModule {}
