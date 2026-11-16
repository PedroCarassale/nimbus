-- Paso 4: intentos de una invocación.
-- La columna es opcional: las filas viejas quedan en NULL y el invoke síncrono
-- la completa al terminar.
ALTER TABLE "invocations" ADD COLUMN "attempts" INTEGER;
