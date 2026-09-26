-- Nimbus Functions — Migración inicial
-- Slice 3: Tablas del control plane

-- Tabla de funciones
CREATE TABLE "functions" (
    "id" UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    "name" VARCHAR(255) NOT NULL UNIQUE,
    "created_at" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updated_at" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Tabla de versiones
CREATE TABLE "versions" (
    "id" UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    "function_id" UUID NOT NULL,
    "version" VARCHAR(50) NOT NULL,
    "artifact_key" VARCHAR(500) NOT NULL,
    "created_at" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT "versions_function_id_fkey" FOREIGN KEY ("function_id") REFERENCES "functions"("id") ON DELETE CASCADE ON UPDATE CASCADE,
    CONSTRAINT "versions_function_id_version_key" UNIQUE ("function_id", "version")
);

-- Tipo enum para status de invocaciones
CREATE TYPE "InvocationStatus" AS ENUM ('PENDING', 'RUNNING', 'SUCCESS', 'ERROR', 'TIMEOUT');

-- Tabla de invocaciones
CREATE TABLE "invocations" (
    "id" UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    "function_id" UUID NOT NULL,
    "version_id" UUID,
    "status" "InvocationStatus" NOT NULL DEFAULT 'PENDING',
    "request_id" UUID NOT NULL UNIQUE,
    "input" JSONB,
    "output" JSONB,
    "error" TEXT,
    "duration_ms" INTEGER,
    "created_at" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updated_at" TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT "invocations_function_id_fkey" FOREIGN KEY ("function_id") REFERENCES "functions"("id") ON DELETE CASCADE ON UPDATE CASCADE,
    CONSTRAINT "invocations_version_id_fkey" FOREIGN KEY ("version_id") REFERENCES "versions"("id") ON DELETE SET NULL ON UPDATE CASCADE
);

-- Índices para consultas frecuentes
CREATE INDEX "functions_name_idx" ON "functions"("name");
CREATE INDEX "versions_function_id_idx" ON "versions"("function_id");
CREATE INDEX "versions_created_at_idx" ON "versions"("created_at" DESC);
CREATE INDEX "invocations_function_id_idx" ON "invocations"("function_id");
CREATE INDEX "invocations_request_id_idx" ON "invocations"("request_id");
CREATE INDEX "invocations_created_at_idx" ON "invocations"("created_at" DESC);
