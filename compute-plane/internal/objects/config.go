package objects

import (
	"os"
	"strings"
)

// Config is the S3 client configuration. An empty Endpoint means the AWS SDK
// talks to real Amazon S3 in Region. A set Endpoint (LocalStack, or any other
// S3-compatible API) is used as the base URL.
type Config struct {
	Region         string
	AccessKey      string
	SecretKey      string
	Bucket         string
	Endpoint       string
	ForcePathStyle bool
	// StaticCreds is false when no key was configured and Endpoint is empty,
	// so the SDK can use the default chain (env, shared config, IAM role).
	StaticCreds bool
}

// LoadConfig reads the standard AWS variables and, when those are unset,
// the older MINIO_* names.
//
//	AWS_REGION / AWS_DEFAULT_REGION     default us-east-1
//	AWS_ACCESS_KEY_ID                   fallback MINIO_ACCESS_KEY, MINIO_ROOT_USER
//	AWS_SECRET_ACCESS_KEY               fallback MINIO_SECRET_KEY, MINIO_ROOT_PASSWORD
//	S3_BUCKET                           fallback MINIO_BUCKET, default nimbus-artifacts
//	S3_ENDPOINT                         fallback AWS_ENDPOINT_URL, MINIO_ENDPOINT
//	S3_FORCE_PATH_STYLE                 default true when an endpoint is set, else false
//
// An endpoint without a scheme gets http://, or https:// when MINIO_USE_SSL is on.
func LoadConfig() Config {
	region := firstEnv("AWS_REGION", "AWS_DEFAULT_REGION")
	if region == "" {
		region = "us-east-1"
	}
	key := firstEnv("AWS_ACCESS_KEY_ID", "MINIO_ACCESS_KEY", "MINIO_ROOT_USER")
	secret := firstEnv("AWS_SECRET_ACCESS_KEY", "MINIO_SECRET_KEY", "MINIO_ROOT_PASSWORD")
	bucket := firstEnv("S3_BUCKET", "MINIO_BUCKET")
	if bucket == "" {
		bucket = "nimbus-artifacts"
	}
	endpoint := normalizeEndpoint(firstEnv("S3_ENDPOINT", "AWS_ENDPOINT_URL", "MINIO_ENDPOINT"))
	static := key != "" || secret != ""
	if endpoint != "" && !static {
		key = "test"
		secret = "test"
		static = true
	}
	return Config{
		Region:         region,
		AccessKey:      key,
		SecretKey:      secret,
		Bucket:         bucket,
		Endpoint:       endpoint,
		ForcePathStyle: forcePathStyle(endpoint),
		StaticCreds:    static,
	}
}

func forcePathStyle(endpoint string) bool {
	raw := strings.TrimSpace(os.Getenv("S3_FORCE_PATH_STYLE"))
	if raw == "" {
		return endpoint != ""
	}
	switch strings.ToLower(raw) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

func normalizeEndpoint(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		scheme := "http"
		switch strings.ToLower(strings.TrimSpace(os.Getenv("MINIO_USE_SSL"))) {
		case "1", "true", "on", "yes":
			scheme = "https"
		}
		raw = scheme + "://" + raw
	}
	return strings.TrimRight(raw, "/")
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}
