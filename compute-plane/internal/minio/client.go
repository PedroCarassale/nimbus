package minio

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Client struct {
	client *minio.Client
	bucket string
}

func New(endpoint, accessKey, secretKey, bucket string, useSSL bool) (*Client, error) {
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("error creando cliente MinIO: %w", err)
	}

	ctx := context.Background()
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("error verificando bucket: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("bucket '%s' no existe", bucket)
	}

	return &Client{client: client, bucket: bucket}, nil
}

func (c *Client) DownloadArtifact(ctx context.Context, functionID, version string) (string, error) {
	objectKey := fmt.Sprintf("functions/%s/versions/%s/code.zip", functionID, version)

	tempDir, err := os.MkdirTemp("", "nimbus-artifact-")
	if err != nil {
		return "", fmt.Errorf("error creando directorio temporal: %w", err)
	}

	zipPath := filepath.Join(tempDir, "code.zip")

	log.Printf("[minio] Descargando %s/%s a %s", c.bucket, objectKey, zipPath)

	object, err := c.client.GetObject(ctx, c.bucket, objectKey, minio.GetObjectOptions{})
	if err != nil {
		os.RemoveAll(tempDir)
		return "", fmt.Errorf("error obteniendo objeto: %w", err)
	}
	defer object.Close()

	file, err := os.Create(zipPath)
	if err != nil {
		os.RemoveAll(tempDir)
		return "", fmt.Errorf("error creando archivo: %w", err)
	}
	defer file.Close()

	written, err := io.Copy(file, object)
	if err != nil {
		os.RemoveAll(tempDir)
		return "", fmt.Errorf("error descargando artifact: %w", err)
	}

	log.Printf("[minio] Descargado %d bytes a %s", written, zipPath)

	return tempDir, nil
}
