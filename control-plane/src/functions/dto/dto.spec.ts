import { plainToInstance } from 'class-transformer';
import { validate } from 'class-validator';
import { CreateFunctionDto } from './create-function.dto';
import { InvokeFunctionDto } from './invoke-function.dto';

async function errorsFor<T extends object>(cls: new () => T, body: unknown) {
  const errors = await validate(plainToInstance(cls, body));
  return errors.map((e) => e.property);
}

describe('CreateFunctionDto', () => {
  it.each(['hello', 'mi-funcion-2', 'a'])('accepts %s', async (name) => {
    expect(await errorsFor(CreateFunctionDto, { name })).toEqual([]);
  });

  it.each(['', 'Hello', 'mi_funcion', 'con espacio', '../x'])(
    'rejects %p',
    async (name) => {
      expect(await errorsFor(CreateFunctionDto, { name })).toEqual(['name']);
    },
  );

  it('rejects a missing name', async () => {
    expect(await errorsFor(CreateFunctionDto, {})).toEqual(['name']);
  });
});

describe('InvokeFunctionDto', () => {
  it('accepts an empty body', async () => {
    expect(await errorsFor(InvokeFunctionDto, {})).toEqual([]);
  });

  it('accepts an event object and a version tag', async () => {
    expect(
      await errorsFor(InvokeFunctionDto, { event: { a: 1 }, version: 'v1' }),
    ).toEqual([]);
  });

  it('rejects a non-object event and a non-string version', async () => {
    expect(
      await errorsFor(InvokeFunctionDto, { event: 'hola', version: 3 }),
    ).toEqual(['event', 'version']);
  });
});
