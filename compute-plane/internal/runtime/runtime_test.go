package runtime

import "testing"

func TestNormalizeDefaultsAndRejects(t *testing.T) {
	for _, raw := range []string{"", "  ", "nodejs20"} {
		got, err := Normalize(raw)
		if err != nil || got != NodeJS20 {
			t.Fatalf("Normalize(%q) = %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{Python312, Go1x} {
		got, err := Normalize(raw)
		if err != nil || got != raw {
			t.Fatalf("Normalize(%q) = %q, %v", raw, got, err)
		}
	}
	_, err := Normalize("ruby")
	if err == nil {
		t.Fatal("ruby should be rejected")
	}
}

func TestTaskPathAndImage(t *testing.T) {
	if got := TaskPath(NodeJS20, "src/app.main", "src/app"); got != "/var/task/src/app.js" {
		t.Fatalf("node path %s", got)
	}
	if got := TaskPath(Python312, "index.handler", "index"); got != "/var/task/index.py" {
		t.Fatalf("python path %s", got)
	}
	if got := TaskPath(Go1x, "bootstrap", "bootstrap"); got != "/var/task/bootstrap" {
		t.Fatalf("go path %s", got)
	}
	if got := TaskPath("", "handler", "index"); got != "/var/task/index.js" {
		t.Fatalf("empty runtime path %s", got)
	}
	if Image(Python312) != "nimbus-python" || Image(Go1x) != "nimbus-go" || Image("") != "nimbus-node" {
		t.Fatalf("images %s %s %s", Image(Python312), Image(Go1x), Image(""))
	}
	if !RunsAsNode(Python312) || !RunsAsNode(Go1x) || RunsAsNode(NodeJS20) || RunsAsNode("") {
		t.Fatal("non-root cold path is python and go only")
	}
	if Label("nope") != NodeJS20 || Label(Go1x) != Go1x {
		t.Fatal("label must stay inside the known set")
	}
}
