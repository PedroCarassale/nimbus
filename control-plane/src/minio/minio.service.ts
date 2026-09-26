import { Injectable, OnModuleInit } from '@nestjs/common';
import {
  S3Client,
  PutObjectCommand,
  GetObjectCommand,
  ListObjectsV2Command,
} from '@aws-sdk/client-s3';
import { Readable } from 'stream';

@Injectable()
export class MinioService implements OnModuleInit {
  private client: S3Client;
  private bucket: string;

  async onModuleInit() {
    const endpoint = process.env.MINIO_ENDPOINT || 'http://localhost:9000';
    const accessKey = process.env.MINIO_ACCESS_KEY || 'minioadmin';
    const secretKey = process.env.MINIO_SECRET_KEY || 'minioadmin';
    this.bucket = process.env.MINIO_BUCKET || 'nimbus-artifacts';

    this.client = new S3Client({
      endpoint,
      region: 'us-east-1',
      credentials: {
        accessKeyId: accessKey,
        secretAccessKey: secretKey,
      },
      forcePathStyle: true,
    });

    console.log(`[minio] Configurado para ${endpoint}, bucket: ${this.bucket}`);
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

    console.log(`[minio] Artifact subido: ${key}`);
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
