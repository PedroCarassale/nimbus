package fnconfig

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func resolveJSON(t *testing.T, body string) (Resolved, error) {
	t.Helper()
	var wire struct {
		TimeoutSec json.RawMessage `json:"timeoutSec"`
		MemoryMB   json.RawMessage `json:"memoryMb"`
		CPUs       json.RawMessage `json:"cpus"`
		Handler    json.RawMessage `json:"handler"`
		EnvVars    json.RawMessage `json:"envVars"`
	}
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		t.Fatal(err)
	}
	return Resolve(Raw{
		TimeoutSec: wire.TimeoutSec,
		MemoryMB:   wire.MemoryMB,
		CPUs:       wire.CPUs,
		Handler:    wire.Handler,
		EnvVars:    wire.EnvVars,
	}, Builtin())
}

func mustResolve(t *testing.T, body string) Resolved {
	t.Helper()
	got, err := resolveJSON(t, body)
	if err != nil {
		t.Fatalf("resolve %s: %v", body, err)
	}
	return got
}

func requireField(t *testing.T, err error, field, contains string) {
	t.Helper()
	var fe *FieldError
	if !errors.As(err, &fe) {
		t.Fatalf("error %v is not a FieldError", err)
	}
	if fe.Field != field {
		t.Fatalf("field = %q, want %q (%s)", fe.Field, field, fe.Message)
	}
	if !strings.Contains(fe.Message, contains) {
		t.Fatalf("message %q does not contain %q", fe.Message, contains)
	}
	if strings.Contains(fe.Message, "super-secret") {
		t.Fatalf("validation error leaked a value: %s", fe.Message)
	}
}

func TestResolveDefaultsWhenOmitted(t *testing.T) {
	got := mustResolve(t, `{}`)
	if got.TimeoutSec != 5 || got.MemoryMB != 128 || got.Handler != "handler" {
		t.Fatalf("defaults: %+v", got)
	}
	if got.HandlerFile != "index" || got.HandlerExport != "handler" {
		t.Fatalf("handler split: %+v", got)
	}
	if got.CPUsMilli != 0 || got.CPUsFlag != "" {
		t.Fatalf("cpus should be unset: %+v", got)
	}
	if len(got.Env) != 0 {
		t.Fatalf("env = %+v", got.Env)
	}

	got = mustResolve(t, `{"timeoutSec":0,"memoryMb":0,"cpus":0,"handler":"","envVars":null}`)
	if got.TimeoutSec != 5 || got.MemoryMB != 128 || got.Handler != "handler" || got.CPUsMilli != 0 {
		t.Fatalf("zeros should fall back to defaults: %+v", got)
	}
}

func TestResolveExplicitValues(t *testing.T) {
	got := mustResolve(t, `{
		"timeoutSec": 30,
		"memoryMb": 256,
		"cpus": 0.5,
		"handler": "src/app.main",
		"envVars": {"B": "two", "A": "one"}
	}`)
	if got.TimeoutSec != 30 || got.MemoryMB != 256 || got.CPUsFlag != "0.5" || got.CPUsMilli != 500 {
		t.Fatalf("numbers: %+v", got)
	}
	if got.Handler != "src/app.main" || got.HandlerFile != "src/app" || got.HandlerExport != "main" {
		t.Fatalf("handler: %+v", got)
	}
	if len(got.Env) != 2 || got.Env[0].Name != "A" || got.Env[0].Value != "one" || got.Env[1].Name != "B" {
		t.Fatalf("env not sorted: %+v", got.Env)
	}
}

func TestResolveBounds(t *testing.T) {
	cases := []struct {
		body  string
		field string
	}{
		{`{"timeoutSec":301}`, "timeoutSec"},
		{`{"timeoutSec":-1}`, "timeoutSec"},
		{`{"timeoutSec":1.5}`, "timeoutSec"},
		{`{"timeoutSec":"5"}`, "timeoutSec"},
		{`{"memoryMb":63}`, "memoryMb"},
		{`{"memoryMb":2049}`, "memoryMb"},
		{`{"memoryMb":128.2}`, "memoryMb"},
		{`{"cpus":0.05}`, "cpus"},
		{`{"cpus":4.1}`, "cpus"},
		{`{"cpus":-0.5}`, "cpus"},
		{`{"cpus":"1"}`, "cpus"},
	}
	for _, tc := range cases {
		_, err := resolveJSON(t, tc.body)
		if err == nil {
			t.Fatalf("expected error for %s", tc.body)
		}
		requireField(t, err, tc.field, tc.field)
	}

	edges := mustResolve(t, `{"timeoutSec":1,"memoryMb":64,"cpus":0.1}`)
	if edges.TimeoutSec != 1 || edges.MemoryMB != 64 || edges.CPUsMilli != 100 {
		t.Fatalf("lower edge: %+v", edges)
	}
	edges = mustResolve(t, `{"timeoutSec":300,"memoryMb":2048,"cpus":4}`)
	if edges.TimeoutSec != 300 || edges.MemoryMB != 2048 || edges.CPUsFlag != "4" {
		t.Fatalf("upper edge: %+v", edges)
	}
}

func TestSplitHandler(t *testing.T) {
	cases := []struct {
		spec, file, export string
	}{
		{"handler", "index", "handler"},
		{"main", "index", "main"},
		{"index.handler", "index", "handler"},
		{"src/app.main", "src/app", "main"},
		{"src/app", "src/app", "handler"},
		{"a/b/c.run", "a/b/c", "run"},
	}
	for _, tc := range cases {
		file, exportName, err := SplitHandler(tc.spec)
		if err != nil {
			t.Fatalf("%s: %v", tc.spec, err)
		}
		if file != tc.file || exportName != tc.export {
			t.Fatalf("%s → %s %s, want %s %s", tc.spec, file, exportName, tc.file, tc.export)
		}
	}

	for _, bad := range []string{"", "../etc.passwd", "/etc/passwd.main", `src\app.main`, "index.handler.extra", ".handler", "src//app.main", "a b.main"} {
		if _, _, err := SplitHandler(bad); err == nil {
			t.Fatalf("expected rejection of %q", bad)
		}
	}
}

func TestEnvValidation(t *testing.T) {
	got := mustResolve(t, `{"envVars":{"GREETING":"super-secret","EMPTY":""}}`)
	if len(got.Env) != 2 || got.Env[1].Value != "super-secret" {
		t.Fatalf("env: %+v", got.Env)
	}

	cases := []struct {
		body     string
		contains string
	}{
		{`{"envVars":["A"]}`, "objeto"},
		{`{"envVars":{"1BAD":"x"}}`, "no es válido"},
		{`{"envVars":{"HAS-DASH":"x"}}`, "no es válido"},
		{`{"envVars":{"NIMBUS_SECRET":"super-secret"}}`, "reservada"},
		{`{"envVars":{"nimbus_secret":"super-secret"}}`, "reservada"},
		{`{"envVars":{"OK":1}}`, "string"},
		{`{"envVars":{"OK":"line\nbreak"}}`, "saltos de línea"},
	}
	for _, tc := range cases {
		_, err := resolveJSON(t, tc.body)
		if err == nil {
			t.Fatalf("expected error for %s", tc.body)
		}
		requireField(t, err, "envVars", tc.contains)
	}

	long := strings.Repeat("a", EnvValueMax+1)
	_, err := resolveJSON(t, `{"envVars":{"OK":"`+long+`"}}`)
	requireField(t, err, "envVars", "supera")

	tooMany := `{"envVars":{`
	for i := 0; i < EnvMaxCount+1; i++ {
		if i > 0 {
			tooMany += ","
		}
		tooMany += `"` + string(rune('A'+i%26)) + strings.Repeat("N", i) + `":"v"`
	}
	tooMany += `}}`
	_, err = resolveJSON(t, tooMany)
	requireField(t, err, "envVars", "máximo")
}

func TestLoadDefaultsFromEnv(t *testing.T) {
	t.Setenv("NIMBUS_TIMEOUT_SEC", "12")
	t.Setenv("NIMBUS_MEMORY_MB", "256")
	t.Setenv("NIMBUS_HANDLER", "src/app.main")
	t.Setenv("NIMBUS_CPUS", "1.5")
	d := LoadDefaults()
	if d.TimeoutSec != 12 || d.MemoryMB != 256 || d.Handler != "src/app.main" || d.CPUsMilli != 1500 {
		t.Fatalf("defaults: %+v", d)
	}
	if d.CPUsLabel() != "1.5" {
		t.Fatalf("label %q", d.CPUsLabel())
	}

	got, err := Resolve(Raw{}, d)
	if err != nil {
		t.Fatal(err)
	}
	if got.TimeoutSec != 12 || got.MemoryMB != 256 || got.HandlerExport != "main" || got.CPUsFlag != "1.5" {
		t.Fatalf("resolved defaults: %+v", got)
	}
}

func TestLoadDefaultsRejectsOutOfRangeEnv(t *testing.T) {
	t.Setenv("NIMBUS_TIMEOUT_SEC", "999")
	t.Setenv("NIMBUS_MEMORY", "8m")
	t.Setenv("NIMBUS_HANDLER", "../x.y")
	t.Setenv("NIMBUS_CPUS", "9")
	d := LoadDefaults()
	if d.TimeoutSec != DefaultTimeoutSec || d.MemoryMB != DefaultMemoryMB || d.Handler != DefaultHandler || d.CPUsMilli != 0 {
		t.Fatalf("invalid env should fall back: %+v", d)
	}
}

func TestLoadDefaultsMemorySuffix(t *testing.T) {
	t.Setenv("NIMBUS_MEMORY", "512m")
	d := LoadDefaults()
	if d.MemoryMB != 512 {
		t.Fatalf("memory = %d", d.MemoryMB)
	}
	t.Setenv("NIMBUS_MEMORY_MB", "64")
	t.Setenv("NIMBUS_MEMORY", "512m")
	d = LoadDefaults()
	if d.MemoryMB != 64 {
		t.Fatalf("NIMBUS_MEMORY_MB should win, got %d", d.MemoryMB)
	}
}

func TestCompleteBackstop(t *testing.T) {
	got, err := (Resolved{}).Complete()
	if err != nil {
		t.Fatal(err)
	}
	if got.TimeoutSec != 5 || got.MemoryMB != 128 || got.HandlerExport != "handler" {
		t.Fatalf("%+v", got)
	}
	_, err = (Resolved{TimeoutSec: 9000}).Complete()
	requireField(t, err, "timeoutSec", "300")
}
