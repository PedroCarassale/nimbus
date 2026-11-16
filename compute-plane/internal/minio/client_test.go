package minio

import (
	"os"
	"path/filepath"
	"testing"
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
