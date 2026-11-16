package executor

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWarmBootstrapProtocol(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	bootstrap := filepath.Clean("../../../runtime-node/bootstrap.js")
	if _, err := os.Stat(bootstrap); err != nil {
		t.Skip("bootstrap.js is not available")
	}

	root := t.TempDir()
	source := []byte(`
		let n = 0;
		exports.handler = async (event) => {
			n += 1;
			console.log("hello " + event.name);
			console.error("err " + event.name);
			if (event.boom) {
				throw new Error("boom");
			}
			return { n, name: event.name, traceId: process.env.NIMBUS_TRACE_ID || "" };
		};
	`)
	if err := os.WriteFile(filepath.Join(root, "index.js"), source, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(node, bootstrap)
	cmd.Env = append(os.Environ(),
		"NIMBUS_WARM=1",
		"NIMBUS_TASK_ROOT="+root,
		"NIMBUS_HANDLER=handler",
		"NIMBUS_MEMORY_MB=128",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stdin.Close()
		_ = cmd.Wait()
	}()

	lines := make(chan string, 32)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	read := func() map[string]interface{} {
		t.Helper()
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("runtime closed stdout")
			}
			var frame map[string]interface{}
			if err := json.Unmarshal([]byte(line), &frame); err != nil {
				t.Fatalf("frame %q: %v", line, err)
			}
			return frame
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for a frame")
			return nil
		}
	}

	ready := read()
	if ready["type"] != "ready" {
		t.Fatalf("ready frame %#v", ready)
	}

	send := func(event map[string]interface{}, id string) {
		t.Helper()
		payload, _ := json.Marshal(map[string]interface{}{
			"type":        "invoke",
			"executionId": id,
			"event":       event,
		})
		if _, err := stdin.Write(append(payload, '\n')); err != nil {
			t.Fatal(err)
		}
	}

	collect := func(id string) (logs []map[string]interface{}, done map[string]interface{}) {
		t.Helper()
		for {
			frame := read()
			if frame["executionId"] != id && frame["type"] != "fatal" {
				t.Fatalf("frame for %v while running %s: %#v", frame["executionId"], id, frame)
			}
			switch frame["type"] {
			case "log":
				logs = append(logs, frame)
			case "done":
				return logs, frame
			default:
				t.Fatalf("unexpected frame %#v", frame)
			}
		}
	}

	send(map[string]interface{}{"name": "Ada"}, "exec-a")
	logs, done := collect("exec-a")
	if len(logs) != 2 || logs[0]["stream"] != "stdout" || logs[0]["line"] != "hello Ada" {
		t.Fatalf("logs %#v", logs)
	}
	if logs[1]["stream"] != "stderr" || logs[1]["line"] != "err Ada" {
		t.Fatalf("stderr %#v", logs[1])
	}
	if done["ok"] != true || done["exitCode"].(float64) != 0 {
		t.Fatalf("done %#v", done)
	}
	out := done["output"].(map[string]interface{})
	if out["n"].(float64) != 1 || out["name"] != "Ada" {
		t.Fatalf("output %#v", out)
	}

	send(map[string]interface{}{"name": "Bea"}, "exec-b")
	_, done = collect("exec-b")
	out = done["output"].(map[string]interface{})
	if out["n"].(float64) != 2 {
		t.Fatalf("state did not survive inside the process: %#v", out)
	}

	send(map[string]interface{}{"boom": true, "name": "Cia"}, "exec-c")
	logs, done = collect("exec-c")
	if done["ok"] != false || done["exitCode"].(float64) != 1 {
		t.Fatalf("error done %#v", done)
	}
	found := false
	for _, frame := range logs {
		if frame["stream"] == "stderr" && frame["line"] == "[nimbus] Error ejecutando handler: boom" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing handler error log: %#v", logs)
	}

	send(map[string]interface{}{"name": "Dora"}, "exec-d")
	_, done = collect("exec-d")
	out = done["output"].(map[string]interface{})
	// The failed call still incremented n before throwing. The process stayed up.
	if out["n"].(float64) != 4 {
		t.Fatalf("handler error discarded the process: %#v", out)
	}

	traceID := "0123456789abcdef0123456789abcdef"
	traceParent := "00-" + traceID + "-0123456789abcdef-01"
	payload, _ := json.Marshal(map[string]interface{}{
		"type":        "invoke",
		"executionId": "exec-e",
		"event":       map[string]interface{}{"name": "Eve"},
		"traceId":     traceID,
		"traceparent": traceParent,
	})
	if _, err := stdin.Write(append(payload, '\n')); err != nil {
		t.Fatal(err)
	}
	logs, done = collect("exec-e")
	found = false
	for _, frame := range logs {
		line, _ := frame["line"].(string)
		if strings.Contains(line, "traceId="+traceID) && strings.Contains(line, "traceparent="+traceParent) {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing trace log: %#v", logs)
	}
	out = done["output"].(map[string]interface{})
	if out["traceId"] != traceID || out["n"].(float64) != 5 {
		t.Fatalf("trace env was not visible to the handler: %#v", out)
	}
}
