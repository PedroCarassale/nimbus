package objects

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestArtifactParentDirDefault(t *testing.T) {
	t.Setenv("NIMBUS_WORK_DIR", "")
	got, err := artifactParentDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != os.TempDir() {
		t.Fatalf("parent = %q, want temp dir", got)
	}
}

func TestArtifactParentDirFromEnv(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "nimbus-work")
	t.Setenv("NIMBUS_WORK_DIR", nested)

	got, err := artifactParentDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != nested {
		t.Fatalf("parent = %q", got)
	}
	info, err := os.Stat(nested)
	if err != nil || !info.IsDir() {
		t.Fatalf("dir not created: %v", err)
	}
}

func TestDownloadUsesPathStyleEndpoint(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/nimbus-artifacts":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/nimbus-artifacts/functions/fn/versions/v1/code.zip"):
			w.Header().Set("Content-Length", "3")
			_, _ = io.WriteString(w, "zip")
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	awsCfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		t.Fatal(err)
	}
	api := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(srv.URL)
		o.UsePathStyle = true
	})
	client := &Client{api: api, bucket: "nimbus-artifacts", cfg: Config{
		Region: "us-east-1", Endpoint: srv.URL, Bucket: "nimbus-artifacts", ForcePathStyle: true,
	}}
	if err := client.ensureBucket(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NIMBUS_WORK_DIR", t.TempDir())
	dir, err := client.DownloadArtifact(context.Background(), "fn", "v1")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "code.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "zip" {
		t.Fatalf("body %q", body)
	}
	if !strings.HasPrefix(gotPath, "/nimbus-artifacts/functions/fn/versions/v1/code.zip") {
		t.Fatalf("path style request was %q", gotPath)
	}
}
