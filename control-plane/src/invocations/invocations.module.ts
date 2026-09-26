import { Module } from '@nestjs/common';
import { InvocationsController } from './invocations.controller';
import { InvocationsService } from './invocations.service';

@Module({
  controllers: [InvocationsController],
  providers: [InvocationsService],
})
export class InvocationsModule {}
