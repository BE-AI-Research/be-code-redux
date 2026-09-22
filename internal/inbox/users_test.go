package inbox

import (
	"path/filepath"
	"sync"
	"testing"
)

func usersFile(t *testing.T) string { return filepath.Join(t.TempDir(), "users.json") }

func TestResolveConfigNameWinsAndBindsTheIP(t *testing.T) {
	p := usersFile(t)
	r, err := Resolve(p, Terminal{IP: "192.168.1.38", Login: "sbrown", User: "Alice", PID: 1}, nil)
	if err != nil || r.ID != "alice" || r.How != "config" || r.Ask {
		t.Fatalf("%+v %v", r, err)
	}
	// A later terminal from the same IP with no config name is offered alice.
	r, _ = Resolve(p, Terminal{IP: "192.168.1.38", Login: "sbrown", PID: 2}, nil)
	if r.ID != "alice" || r.How != "ip" || r.Ask {
		t.Fatalf("%+v", r)
	}
}

func TestResolveUnknownIPAsksAndBindKeepsIt(t *testing.T) {
	p := usersFile(t)
	r, _ := Resolve(p, Terminal{IP: "10.0.0.5", Login: "x", PID: 3}, nil)
	if !r.Ask || r.ID != "" || len(r.Choices) != 0 {
		t.Fatalf("%+v", r)
	}
	if err := Bind(p, "bob", Terminal{IP: "10.0.0.5", Login: "x", PID: 3}, ""); err != nil {
		t.Fatal(err)
	}
	r, _ = Resolve(p, Terminal{IP: "10.0.0.5", Login: "x", PID: 4}, nil)
	if r.ID != "bob" || r.How != "ip" {
		t.Fatalf("%+v", r)
	}
}

func TestResolveSeveralIDsOnOneIPOffersChoices(t *testing.T) {
	p := usersFile(t)
	tm := Terminal{IP: "10.0.0.9", Login: "shared"}
	Bind(p, "alice", tm, "")
	Bind(p, "bob", tm, "")
	r, _ := Resolve(p, tm, nil)
	if !r.Ask || len(r.Choices) != 2 || r.Choices[0] != "alice" || r.Choices[1] != "bob" {
		t.Fatalf("%+v", r)
	}
}

func TestResolveFallsBackToMACOnlyForAnUnknownIP(t *testing.T) {
	p := usersFile(t)
	Bind(p, "alice", Terminal{IP: "192.168.1.38", Login: "s"}, "aa:bb:cc:dd:ee:ff")
	calls := 0
	lookup := func(ip string) string {
		calls++
		if ip == "192.168.1.77" {
			return "AA:BB:CC:DD:EE:FF" // case must not matter
		}
		return ""
	}
	// Known IP: MAC is never consulted.
	if r, _ := Resolve(p, Terminal{IP: "192.168.1.38"}, lookup); r.ID != "alice" || calls != 0 {
		t.Fatalf("%+v calls=%d", r, calls)
	}
	// New IP, same device: recognised, and the new IP is bound too.
	r, _ := Resolve(p, Terminal{IP: "192.168.1.77", Login: "s"}, lookup)
	if r.ID != "alice" || r.How != "mac" || calls != 1 {
		t.Fatalf("%+v calls=%d", r, calls)
	}
	if r, _ := Resolve(p, Terminal{IP: "192.168.1.77"}, lookup); r.How != "ip" {
		t.Fatalf("the new IP was not bound: %+v", r)
	}
}

func TestBindRejectsBadIDs(t *testing.T) {
	p := usersFile(t)
	if err := Bind(p, "agent", Terminal{IP: "1.2.3.4"}, ""); err == nil {
		t.Fatal("bound the reserved id")
	}
}

func TestConcurrentBindsDoNotLoseEachOther(t *testing.T) {
	p := usersFile(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			Bind(p, "u"+string(rune('a'+i)), Terminal{IP: "10.1.1.1"}, "")
		}(i)
	}
	wg.Wait()
	u, err := Load(p)
	if err != nil || len(u.Users) != 20 {
		t.Fatalf("%d users, %v", len(u.Users), err)
	}
}
