import { Module } from '@nestjs/common';
import { FunctionsController } from './functions.controller';
import { FunctionsService } from './functions.service';
import { MinioModule } from '../minio/minio.module';
import { RunnerService } from './runner.service';
import { AsyncResultConsumer } from './async-result.consumer';

@Module({
  imports: [MinioModule],
  controllers: [FunctionsController],
  providers: [FunctionsService, RunnerService, AsyncResultConsumer],
})
export class FunctionsModule {}
