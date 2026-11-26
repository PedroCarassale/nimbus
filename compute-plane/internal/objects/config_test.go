package objects

import "testing"

func TestLoadConfigRealAWSWhenEndpointEmpty(t *testing.T) {
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA_TEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("S3_BUCKET", "prod-artifacts")
	t.Setenv("S3_ENDPOINT", "")
	t.Setenv("AWS_ENDPOINT_URL", "")
	t.Setenv("MINIO_ENDPOINT", "")
	t.Setenv("S3_FORCE_PATH_STYLE", "")

	cfg := LoadConfig()
	if cfg.Endpoint != "" {
		t.Fatalf("endpoint = %q, want empty so the SDK uses real S3", cfg.Endpoint)
	}
	if cfg.ForcePathStyle {
		t.Fatal("path style should be off for real S3")
	}
	if cfg.Region != "eu-west-1" || cfg.Bucket != "prod-artifacts" || cfg.AccessKey != "AKIA_TEST" {
		t.Fatalf("cfg %+v", cfg)
	}
	if !cfg.StaticCreds {
		t.Fatal("explicit keys should be static credentials")
	}
}

func TestLoadConfigLocalStackDefaultsPathStyle(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("S3_BUCKET", "nimbus-artifacts")
	t.Setenv("S3_ENDPOINT", "http://localstack:4566/")
	t.Setenv("S3_FORCE_PATH_STYLE", "")
	t.Setenv("MINIO_ENDPOINT", "http://minio:9000")

	cfg := LoadConfig()
	if cfg.Endpoint != "http://localstack:4566" {
		t.Fatalf("endpoint = %q", cfg.Endpoint)
	}
	if !cfg.ForcePathStyle {
		t.Fatal("LocalStack needs path-style addressing")
	}
	if cfg.Region != "us-east-1" {
		t.Fatalf("region = %q", cfg.Region)
	}
}

func TestLoadConfigPathStyleCanBeDisabled(t *testing.T) {
	t.Setenv("S3_ENDPOINT", "http://s3.internal")
	t.Setenv("S3_FORCE_PATH_STYLE", "false")
	t.Setenv("AWS_ACCESS_KEY_ID", "k")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "s")
	cfg := LoadConfig()
	if cfg.ForcePathStyle {
		t.Fatal("S3_FORCE_PATH_STYLE=false should win")
	}
}

func TestLoadConfigMinIOFallback(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("S3_BUCKET", "")
	t.Setenv("S3_ENDPOINT", "")
	t.Setenv("AWS_ENDPOINT_URL", "")
	t.Setenv("MINIO_ACCESS_KEY", "minioadmin")
	t.Setenv("MINIO_SECRET_KEY", "minioadmin")
	t.Setenv("MINIO_BUCKET", "legacy")
	t.Setenv("MINIO_ENDPOINT", "minio:9000")
	t.Setenv("MINIO_USE_SSL", "")
	t.Setenv("S3_FORCE_PATH_STYLE", "")

	cfg := LoadConfig()
	if cfg.Endpoint != "http://minio:9000" {
		t.Fatalf("endpoint = %q", cfg.Endpoint)
	}
	if cfg.AccessKey != "minioadmin" || cfg.Bucket != "legacy" || !cfg.ForcePathStyle {
		t.Fatalf("cfg %+v", cfg)
	}
}

func TestLoadConfigIAMChainWhenNoKeysAndNoEndpoint(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("MINIO_ACCESS_KEY", "")
	t.Setenv("MINIO_SECRET_KEY", "")
	t.Setenv("MINIO_ROOT_USER", "")
	t.Setenv("MINIO_ROOT_PASSWORD", "")
	t.Setenv("S3_ENDPOINT", "")
	t.Setenv("AWS_ENDPOINT_URL", "")
	t.Setenv("MINIO_ENDPOINT", "")

	cfg := LoadConfig()
	if cfg.StaticCreds || cfg.AccessKey != "" {
		t.Fatalf("expected default credential chain, got %+v", cfg)
	}
	if cfg.Endpoint != "" {
		t.Fatalf("endpoint = %q", cfg.Endpoint)
	}
}
