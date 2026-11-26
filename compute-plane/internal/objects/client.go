package objects

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// API is the subset of the S3 client the compute plane uses.
type API interface {
	HeadBucket(ctx context.Context, params *s3.HeadBucketInput, optFns ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	CreateBucket(ctx context.Context, params *s3.CreateBucketInput, optFns ...func(*s3.Options)) (*s3.CreateBucketOutput, error)
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

// Client downloads function zips. Endpoint empty means real AWS S3.
type Client struct {
	api    API
	bucket string
	cfg    Config
}

// New builds an AWS SDK client from cfg and waits until the bucket exists,
// creating it when the credentials are allowed to. Compose creates the bucket
// in an init container; this retry covers a process that starts first.
func New(ctx context.Context, cfg Config) (*Client, error) {
	api, err := newAPI(ctx, cfg)
	if err != nil {
		return nil, err
	}
	client := &Client{api: api, bucket: cfg.Bucket, cfg: cfg}
	if err := client.ensureBucket(ctx); err != nil {
		return nil, err
	}
	return client, nil
}

func newAPI(ctx context.Context, cfg Config) (*s3.Client, error) {
	opts := []func(*config.LoadOptions) error{
		config.WithRegion(cfg.Region),
	}
	if cfg.StaticCreds {
		opts = append(opts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("config S3: %w", err)
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.ForcePathStyle
	}), nil
}

func (c *Client) ensureBucket(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var lastErr error
	for {
		_, err := c.api.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(c.bucket)})
		if err == nil {
			return nil
		}
		if !bucketMissing(err) {
			lastErr = err
		} else {
			input := &s3.CreateBucketInput{Bucket: aws.String(c.bucket)}
			if c.cfg.Region != "" && c.cfg.Region != "us-east-1" {
				input.CreateBucketConfiguration = &types.CreateBucketConfiguration{
					LocationConstraint: types.BucketLocationConstraint(c.cfg.Region),
				}
			}
			_, mkErr := c.api.CreateBucket(ctx, input)
			if mkErr == nil || bucketAlreadyExists(mkErr) {
				log.Printf("[s3] Bucket %s listo", c.bucket)
				return nil
			}
			lastErr = mkErr
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("s3 no quedó listo: %w", lastErr)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func bucketMissing(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchBucket", "404":
			return true
		}
	}
	return false
}

func bucketAlreadyExists(err error) bool {
	var owned *types.BucketAlreadyOwnedByYou
	var exists *types.BucketAlreadyExists
	return errors.As(err, &owned) || errors.As(err, &exists)
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

// DownloadArtifact fetches functions/{id}/versions/{version}/code.zip and
// returns the directory that contains code.zip.
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
	where := c.cfg.Endpoint
	if where == "" {
		where = "aws:" + c.cfg.Region
	}
	log.Printf("[s3] Descargando %s/%s desde %s a %s", c.bucket, objectKey, where, zipPath)

	object, err := c.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		os.RemoveAll(tempDir)
		return "", fmt.Errorf("error obteniendo objeto: %w", err)
	}
	defer object.Body.Close()

	file, err := os.Create(zipPath)
	if err != nil {
		os.RemoveAll(tempDir)
		return "", fmt.Errorf("error creando archivo: %w", err)
	}
	defer file.Close()

	written, err := io.Copy(file, object.Body)
	if err != nil {
		os.RemoveAll(tempDir)
		return "", fmt.Errorf("error descargando artifact: %w", err)
	}
	log.Printf("[s3] Descargado %d bytes a %s", written, zipPath)
	return tempDir, nil
}
