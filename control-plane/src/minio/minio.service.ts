import { Injectable, OnModuleInit } from '@nestjs/common';
import {
  S3Client,
  PutObjectCommand,
  GetObjectCommand,
  ListObjectsV2Command,
} from '@aws-sdk/client-s3';
import { Readable } from 'stream';

function firstEnv(...names: string[]): string {
  for (const name of names) {
    const value = process.env[name];
    if (value && value.trim()) {
      return value.trim();
    }
  }
  return '';
}

function normalizeEndpoint(raw: string): string {
  const value = raw.trim().replace(/\/+$/, '');
  if (!value) {
    return '';
  }
  if (!value.includes('://')) {
    return `http://${value}`;
  }
  return value;
}

@Injectable()
export class MinioService implements OnModuleInit {
  private client: S3Client;
  private bucket: string;

  async onModuleInit() {
    const region = firstEnv('AWS_REGION', 'AWS_DEFAULT_REGION') || 'us-east-1';
    const endpoint = normalizeEndpoint(
      firstEnv('S3_ENDPOINT', 'AWS_ENDPOINT_URL', 'MINIO_ENDPOINT'),
    );
    let accessKey = firstEnv(
      'AWS_ACCESS_KEY_ID',
      'MINIO_ACCESS_KEY',
      'MINIO_ROOT_USER',
    );
    let secretKey = firstEnv(
      'AWS_SECRET_ACCESS_KEY',
      'MINIO_SECRET_KEY',
      'MINIO_ROOT_PASSWORD',
    );
    this.bucket = firstEnv('S3_BUCKET', 'MINIO_BUCKET') || 'nimbus-artifacts';
    if (endpoint && !accessKey && !secretKey) {
      accessKey = 'test';
      secretKey = 'test';
    }
    const pathRaw = (process.env.S3_FORCE_PATH_STYLE || '').trim().toLowerCase();
    const forcePathStyle = pathRaw
      ? !['0', 'false', 'off', 'no'].includes(pathRaw)
      : endpoint !== '';

    const config: ConstructorParameters<typeof S3Client>[0] = {
      region,
      forcePathStyle,
    };
    if (endpoint) {
      config.endpoint = endpoint;
    }
    if (accessKey || secretKey) {
      config.credentials = {
        accessKeyId: accessKey,
        secretAccessKey: secretKey,
      };
    }
    this.client = new S3Client(config);

    console.log(
      `[s3] Configurado para ${endpoint || `aws:${region}`}, bucket: ${this.bucket}`,
    );
  }

  async uploadArtifact(
    functionId: string,
    version: string,
    data: Buffer,
  ): Promise<string> {
    const key = `functions/${functionId}/versions/${version}/code.zip`;

    await this.client.send(
      new PutObjectCommand({
        Bucket: this.bucket,
        Key: key,
        Body: data,
        ContentType: 'application/zip',
      }),
    );

    console.log(`[s3] Artifact subido: ${key}`);
    return key;
  }

  async downloadArtifact(artifactKey: string): Promise<Buffer> {
    const response = await this.client.send(
      new GetObjectCommand({
        Bucket: this.bucket,
        Key: artifactKey,
      }),
    );

    const stream = response.Body as Readable;
    const chunks: Buffer[] = [];

    for await (const chunk of stream) {
      chunks.push(Buffer.from(chunk));
    }

    return Buffer.concat(chunks);
  }

  async getLatestVersion(functionId: string): Promise<string | null> {
    const prefix = `functions/${functionId}/versions/`;

    const response = await this.client.send(
      new ListObjectsV2Command({
        Bucket: this.bucket,
        Prefix: prefix,
        Delimiter: '/',
      }),
    );

    if (!response.CommonPrefixes || response.CommonPrefixes.length === 0) {
      return null;
    }

    const versions = response.CommonPrefixes.map((p) =>
      p.Prefix!.replace(prefix, '').replace('/', ''),
    ).sort((a, b) => b.localeCompare(a));

    return versions[0] || null;
  }

  getArtifactKey(functionId: string, version: string): string {
    return `functions/${functionId}/versions/${version}/code.zip`;
  }
}
