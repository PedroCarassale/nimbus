package firecracker

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/PedroCarassale/nimbus/compute-plane/internal/fcwire"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/runtime"
	"github.com/PedroCarassale/nimbus/compute-plane/internal/warm"
)

func argvFor(rt string) []string {
	switch runtime.Canonical(rt) {
	case runtime.Python312:
		return []string{"/usr/local/bin/python3", "-B", "/var/runtime/bootstrap.py"}
	case runtime.Go1x:
		return []string{"/var/runtime/bootstrap"}
	default:
		return []string{"/usr/local/bin/node", "/var/runtime/bootstrap.js"}
	}
}

// buildGuestConfig is what init reads. User variables are copied first and
// platform keys are written after them, so a request cannot replace NIMBUS_*.
func buildGuestConfig(spec warm.Spec) fcwire.Config {
	env := map[string]string{
		"PATH":                    "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME":                    "/tmp",
		"NIMBUS_WARM":             "1",
		"PYTHONDONTWRITEBYTECODE": "1",
		"PYTHONUNBUFFERED":        "1",
	}
	for _, ev := range spec.Config.Env {
		env[ev.Name] = ev.Value
	}
	cfg := spec.Config
	env["NIMBUS_WARM"] = "1"
	env["HOME"] = "/tmp"
	env["NIMBUS_HANDLER"] = cfg.Handler
	env["NIMBUS_HANDLER_FUNCTION"] = cfg.HandlerExport
	env["NIMBUS_HANDLER_PATH"] = runtime.TaskPath(cfg.Runtime, cfg.Handler, cfg.HandlerFile)
	env["NIMBUS_MEMORY_MB"] = strconv.Itoa(cfg.MemoryMB)
	env["NIMBUS_TIMEOUT_SEC"] = strconv.Itoa(cfg.TimeoutSec)
	if spec.TraceID != "" {
		env["NIMBUS_TRACE_ID"] = spec.TraceID
	}
	if spec.TraceParent != "" {
		env["TRACEPARENT"] = spec.TraceParent
	}
	return fcwire.Config{
		Argv: argvFor(cfg.Runtime),
		Env:  env,
		UID:  1000,
		GID:  1000,
	}
}

// buildCodeImage writes an ext4 the guest mounts read-only at the second drive.
// Layout: /nimbus.json and /task/<zip contents>. mkfs -d avoids a loop mount.
func buildCodeImage(workDir, dest string, cfg fcwire.Config) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(dest), "stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	task := filepath.Join(stage, fcwire.TaskDir)
	if err := copyTree(workDir, task); err != nil {
		return err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, fcwire.ConfigName), raw, 0o644); err != nil {
		return err
	}
	if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	if err := f.Truncate(64 << 20); err != nil {
		f.Close()
		return err
	}
	f.Close()
	cmd := exec.Command("mkfs.ext4", "-F", "-L", "nimbus-code", "-d", stage, dest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		os.Remove(dest)
		return fmt.Errorf("mkfs.ext4: %v: %s", err, trim(out, 400))
	}
	return os.Chmod(dest, 0o644)
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("el código tiene un archivo que no es regular: %s", rel)
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func trim(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
