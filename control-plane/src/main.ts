import { NestFactory } from '@nestjs/core';
import { ValidationPipe } from '@nestjs/common';
import { AppModule } from './app.module';
import { httpMetrics } from './observability/metrics';
import { setupTracing, tracingMiddleware } from './observability/tracing';

async function bootstrap() {
  setupTracing();
  const app = await NestFactory.create(AppModule);
  const http = app.getHttpAdapter().getInstance();
  http.use(tracingMiddleware);
  http.use(httpMetrics);

  app.useGlobalPipes(
    new ValidationPipe({
      whitelist: true,
      forbidNonWhitelisted: true,
      transform: true,
    }),
  );

  app.enableCors();

  const port = process.env.PORT || 3000;
  await app.listen(port);
  console.log(`[control-plane] Nimbus Control Plane escuchando en puerto ${port}`);
}

bootstrap();
