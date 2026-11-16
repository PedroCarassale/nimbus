package executor

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/logs"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestApplyDefaults(t *testing.T) {
	timeout, memory, handler := applyDefaults(0, 0, "")
	if timeout != 5 || memory != 128 || handler != "handler" {
		t.Fatalf("defaults = %d %d %q", timeout, memory, handler)
	}

	timeout, memory, handler = applyDefaults(12, 256, "main")
	if timeout != 12 || memory != 256 || handler != "main" {
		t.Fatalf("explicit = %d %d %q", timeout, memory, handler)
	}
}

func TestDockerRunArgsIsolation(t *testing.T) {
	args := dockerRunArgs("nimbus-abc", "/tmp/nimbus-work/job/work", "nimbus-node", "handler", 128)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"run",
		"--name nimbus-abc",
		"--rm",
		"--memory=128m",
		"--pids-limit=256",
		"--network=none",
		"--read-only",
		"-v /tmp/nimbus-work/job/work:/var/task:ro",
		"-e NIMBUS_HANDLER_FUNCTION=handler",
		"-e NIMBUS_EVENT_PATH=/var/task/event.json",
		"nimbus-node",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q in %s", want, joined)
		}
	}
}

func TestContainerNameSanitizes(t *testing.T) {
	name := containerName("abc/123 def")
	if name != "nimbus-abc-123-def" {
		t.Fatalf("name = %q", name)
	}
	long := containerName(strings.Repeat("a", 80))
	if len(long) > 63 {
		t.Fatalf("name too long: %d", len(long))
	}
	if strings.HasSuffix(long, "-") || strings.HasSuffix(long, ".") {
		t.Fatalf("name has a trailing separator: %q", long)
	}
}

func TestExitCodeFrom(t *testing.T) {
	if got := exitCodeFrom(nil, false); got != 0 {
		t.Fatalf("nil err = %d", got)
	}
	if got := exitCodeFrom(errors.New("boom"), false); got != 1 {
		t.Fatalf("generic err = %d", got)
	}
	if got := exitCodeFrom(errors.New("ignored"), true); got != 124 {
		t.Fatalf("timeout = %d", got)
	}

	cmd := exec.Command("sh", "-c", "exit 3")
	err := cmd.Run()
	if got := exitCodeFrom(err, false); got != 3 {
		t.Fatalf("exit 3 = %d (err %v)", got, err)
	}
}

func TestParseOutputPrettyJSON(t *testing.T) {
	stdout := "{\n  \"statusCode\": 200,\n  \"body\": {\n    \"message\": \"¡Hola, Nimbus!\"\n  }\n}\n"
	got, ok := parseOutput(stdout)
	if !ok {
		t.Fatal("expected pretty JSON to parse")
	}
	obj, ok := got.(map[string]interface{})
	if !ok {
		t.Fatalf("type %T", got)
	}
	if obj["statusCode"].(float64) != 200 {
		t.Fatalf("statusCode = %v", obj["statusCode"])
	}
	body := obj["body"].(map[string]interface{})
	if body["message"] != "¡Hola, Nimbus!" {
		t.Fatalf("message = %v", body["message"])
	}
}

func TestParseOutputLastJSONAfterLogs(t *testing.T) {
	stdout := "boot\n{\"debug\":true}\n{\n  \"ok\": true\n}\n"
	got, ok := parseOutput(stdout)
	if !ok {
		t.Fatal("expected JSON")
	}
	obj := got.(map[string]interface{})
	if obj["ok"] != true {
		t.Fatalf("got %#v", got)
	}
}

func TestParseOutputSingleLineAndScalars(t *testing.T) {
	cases := []struct {
		in   string
		want interface{}
	}{
		{`{"a":1}`, map[string]interface{}{"a": float64(1)}},
		{`[1,2]`, nil},
		{`"hola"`, "hola"},
		{`null`, nil},
		{`true`, true},
	}
	for _, tc := range cases {
		got, ok := parseOutput(tc.in)
		if !ok {
			t.Fatalf("parse %q failed", tc.in)
		}
		if tc.in == `[1,2]` {
			arr, ok := got.([]interface{})
			if !ok || len(arr) != 2 {
				t.Fatalf("array %#v", got)
			}
			continue
		}
		if tc.in == `{"a":1}` {
			obj := got.(map[string]interface{})
			if obj["a"].(float64) != 1 {
				t.Fatalf("object %#v", got)
			}
			continue
		}
		if got != tc.want {
			t.Fatalf("parse %q = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

func TestParseOutputRejectsGarbage(t *testing.T) {
	if _, ok := parseOutput(""); ok {
		t.Fatal("empty should not parse")
	}
	if _, ok := parseOutput("not json\nstill not"); ok {
		t.Fatal("text should not parse")
	}
}

func TestUnzipAndZipSlip(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "code.zip")

	buf, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(buf)
	w, err := zw.Create("index.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("exports.handler = async () => ({ ok: true });\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	buf.Close()

	dest := filepath.Join(dir, "work")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unzip(zipPath, dest); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dest, "index.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "exports.handler") {
		t.Fatalf("extracted %q", body)
	}

	slipZip := filepath.Join(dir, "slip.zip")
	f, err := os.Create(slipZip)
	if err != nil {
		t.Fatal(err)
	}
	zw = zip.NewWriter(f)
	hdr := &zip.FileHeader{Name: "../evil.txt", Method: zip.Store}
	ew, err := zw.CreateHeader(hdr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ew.Write([]byte("pwned")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	slipDest := filepath.Join(dir, "slip")
	if err := os.MkdirAll(slipDest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unzip(slipZip, slipDest); err == nil {
		t.Fatal("expected zip-slip to be rejected")
	}
	if _, err := os.Stat(filepath.Join(dir, "evil.txt")); !os.IsNotExist(err) {
		t.Fatalf("evil file leaked: %v", err)
	}
}

type fakeStore struct {
	dir string
	err error
}

func (f fakeStore) DownloadArtifact(context.Context, string, string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.dir, nil
}

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()})
}

func TestExecuteDownloadErrorIsLogged(t *testing.T) {
	rdb := testRedis(t)
	exec := New(fakeStore{err: errors.New("minio down")}, rdb)
	result := exec.Execute(context.Background(), &ExecutionRequest{
		ExecutionID: "exec-1",
		FunctionID:  "fn-1",
		Version:     "v1",
	})
	if result.Success {
		t.Fatal("expected failure")
	}
	if result.ExitCode != 1 {
		t.Fatalf("exit = %d", result.ExitCode)
	}
	if !strings.Contains(result.Error, "descargando artifact") {
		t.Fatalf("error = %q", result.Error)
	}

	entries, err := logs.NewStreamReader(rdb, "exec-1").ReadAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("entries = %#v", entries)
	}
	last := entries[len(entries)-1]
	if last.Stream != "_end" {
		t.Fatalf("last stream = %q", last.Stream)
	}
}

func TestExecuteInvalidZip(t *testing.T) {
	rdb := testRedis(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "code.zip"), []byte("not a zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := New(fakeStore{dir: dir}, rdb)
	result := exec.Execute(context.Background(), &ExecutionRequest{
		ExecutionID: "exec-2",
		FunctionID:  "fn-1",
		Version:     "v1",
		Event:       map[string]interface{}{"name": "Nimbus"},
	})
	if result.Success {
		t.Fatal("expected failure")
	}
	if !strings.Contains(result.Error, "extrayendo zip") {
		t.Fatalf("error = %q", result.Error)
	}
}
