import { IsOptional, IsObject, IsString } from 'class-validator';

export class InvokeFunctionDto {
  @IsOptional()
  @IsObject()
  event?: Record<string, unknown>;

  @IsOptional()
  @IsString()
  version?: string;
}
