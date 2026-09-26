-- Slice 5: Add execution_id to invocations for log stream correlation
ALTER TABLE "invocations" ADD COLUMN "execution_id" TEXT;
