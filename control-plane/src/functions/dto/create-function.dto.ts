import { IsString, IsNotEmpty, Matches } from 'class-validator';

export class CreateFunctionDto {
  @IsString()
  @IsNotEmpty()
  @Matches(/^[a-z0-9-]+$/, {
    message: 'name debe contener solo letras minúsculas, números y guiones',
  })
  name: string;
}
