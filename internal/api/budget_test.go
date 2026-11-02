// internal/api/budget_test.go
package api

import (
	"testing"
	"time"
)

func TestSlidingHourWindow(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	b := newBudgets(func() time.Time { return now })
	for i := 0; i < 3; i++ {
		if retry, ok := b.take("app", 3); !ok || retry != 0 {
			t.Fatalf("request %d must pass", i)
		}
	}
	retry, ok := b.take("app", 3)
	if ok || retry <= 0 || retry > time.Hour {
		t.Fatalf("the fourth must be refused with a Retry-After, got ok=%v retry=%s", ok, retry)
	}
	now = now.Add(time.Hour + time.Second)
	if _, ok := b.take("app", 3); !ok {
		t.Fatal("an hour later the window has slid")
	}
	if _, ok := b.take("other", 3); !ok {
		t.Fatal("windows are per app")
	}
	if _, ok := b.take("free", 0); !ok {
		t.Fatal("zero means unbounded")
	}
}

func TestOneInFlightPerApp(t *testing.T) {
	b := newBudgets(time.Now)
	release, ok := b.begin("app")
	if !ok {
		t.Fatal("first request begins")
	}
	if _, ok := b.begin("app"); ok {
		t.Fatal("a second concurrent request from the same app is refused")
	}
	if r2, ok := b.begin("other"); !ok {
		t.Fatal("other apps are independent")
	} else {
		r2()
	}
	release()
	if r3, ok := b.begin("app"); !ok {
		t.Fatal("after release the app may begin again")
	} else {
		r3()
	}
}
