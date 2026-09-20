package tools

import (
	"reflect"
	"testing"
)

func TestTaskkillArgsKillTheWholeTreeByForce(t *testing.T) {
	got := taskkillArgs(4242)
	want := []string{"/T", "/F", "/PID", "4242"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
