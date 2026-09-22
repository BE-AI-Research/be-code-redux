package subagent

import "testing"

func TestCleanScope(t *testing.T) {
	got, err := CleanScope([]string{" internal/scan/ ", "./docs", "a b/c.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "internal/scan" || got[1] != "docs" || got[2] != "a b/c.go" {
		t.Fatalf("got %q", got)
	}
	for _, bad := range [][]string{{"../x"}, {"/etc"}, {"."}, {""}, {"a  b"}} {
		if _, err := CleanScope(bad); err == nil {
			t.Fatalf("%q should be refused", bad)
		}
	}
}

func TestInScopeWithinOverlap(t *testing.T) {
	scope := []string{"internal/scan", "README.md"}
	if !InScope(scope, "internal/scan/token.go") || !InScope(scope, "README.md") {
		t.Fatal("inside paths refused")
	}
	if InScope(scope, "internal/scanner/x.go") || InScope(scope, "README.md.bak") || InScope(scope, "cmd/x.go") {
		t.Fatal("outside paths accepted")
	}
	if !Within([]string{"docs/api"}, []string{"docs"}) || Within([]string{"docs"}, []string{"docs/api"}) {
		t.Fatal("Within wrong")
	}
	if !Within([]string{"anything"}, nil) {
		t.Fatal("empty max_scope means the whole workspace")
	}
	if !Overlap([]string{"internal"}, []string{"internal/scan"}) || Overlap([]string{"internal/a"}, []string{"internal/b"}) {
		t.Fatal("Overlap wrong")
	}
}

func TestLaneKey(t *testing.T) {
	if LaneKey("http://192.168.1.150:11434/v1") != "http://192.168.1.150:11434" {
		t.Fatal(LaneKey("http://192.168.1.150:11434/v1"))
	}
	if LaneKey("HTTP://Host:11434") != "http://host:11434" {
		t.Fatal("case folding")
	}
	if LaneKey("not a url") != "not a url" {
		t.Fatal("unparseable keeps the string")
	}
}
