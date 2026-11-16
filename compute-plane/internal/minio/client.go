package minio

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

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

	if err := ensureBucket(client, bucket); err != nil {
		return nil, err
	}

	return &Client{client: client, bucket: bucket}, nil
}

// ensureBucket waits until the API answers and creates the bucket when it
// is missing. Compose starts this process in parallel with bucket init.
func ensureBucket(client *minio.Client, bucket string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var lastErr error
	for {
		exists, err := client.BucketExists(ctx, bucket)
		if err == nil && exists {
			return nil
		}
		if err == nil {
			mkErr := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{})
			if mkErr == nil || bucketAlreadyExists(mkErr) {
				log.Printf("[minio] Bucket %s listo", bucket)
				return nil
			}
			lastErr = mkErr
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("minio no quedó listo: %w", lastErr)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func bucketAlreadyExists(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "BucketAlreadyOwnedByYou" || resp.Code == "BucketAlreadyExists"
}

// artifactParentDir is the directory that holds extracted function code.
// NIMBUS_WORK_DIR must be the same absolute path inside this process and on
// the host Docker daemon, because docker run -v is resolved by the daemon.
func artifactParentDir() (string, error) {
	dir := os.Getenv("NIMBUS_WORK_DIR")
	if dir == "" {
		return os.TempDir(), nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("error creando NIMBUS_WORK_DIR: %w", err)
	}
	return dir, nil
}

func (c *Client) DownloadArtifact(ctx context.Context, functionID, version string) (string, error) {
	objectKey := fmt.Sprintf("functions/%s/versions/%s/code.zip", functionID, version)

	parent, err := artifactParentDir()
	if err != nil {
		return "", err
	}

	tempDir, err := os.MkdirTemp(parent, "nimbus-artifact-")
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
