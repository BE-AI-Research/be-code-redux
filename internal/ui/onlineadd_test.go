// internal/ui/onlineadd_test.go
package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/setup"
	"github.com/brown-enterprises/be-code/internal/signin"
)

type scriptedPrompter struct {
	mu      sync.Mutex
	answers []string
	secrets []string
	said    []string
}

func (s *scriptedPrompter) Ask(ctx context.Context, q string) (string, bool) {
	if strings.HasPrefix(q, "waiting for the browser") {
		// The paste prompt: nobody types; it ends when the flow cancels it.
		<-ctx.Done()
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.said = append(s.said, q)
	if len(s.answers) == 0 {
		return "", false
	}
	a := s.answers[0]
	s.answers = s.answers[1:]
	return a, true
}
func (s *scriptedPrompter) AskSecret(_ context.Context, q string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.said = append(s.said, q)
	if len(s.secrets) == 0 {
		return "", false
	}
	a := s.secrets[0]
	s.secrets = s.secrets[1:]
	return a, true
}
func (s *scriptedPrompter) Say(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.said = append(s.said, line)
}
func (s *scriptedPrompter) all() string {
	return strings.Join(s.said, "\n")
}

func stubOnlineUI(t *testing.T) {
	t.Helper()
	ov, os_, ob := onlineValidate, onlineSignIn, onlineOpenBrowser
	onlineValidate = func(ctx context.Context, ch setup.Choice, key string) ([]provider.ModelInfo, error) {
		if strings.TrimSpace(key) != "good" && key != "sk-or-test" {
			return nil, setup.ErrKeyRejected
		}
		return []provider.ModelInfo{{ID: "m1"}, {ID: "m2"}}, nil
	}
	onlineSignIn = func(ctx context.Context, o signin.Options) (string, error) { return "sk-or-test", nil }
	onlineOpenBrowser = func(string) error { t.Fatal("no browser in tests"); return nil }
	op := onlineOpenPage
	onlineOpenPage = func(string) bool { t.Fatal("no keys page in tests without a stub"); return false }
	t.Cleanup(func() { onlineValidate, onlineSignIn, onlineOpenBrowser, onlineOpenPage = ov, os_, ob, op })
}

func TestOnlineAddPlainFlow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubOnlineUI(t)
	cfg := config.Default()
	// answers: preset number for groq (5th: after the four quick sign-in rows), model 2, default no
	p := &scriptedPrompter{answers: []string{"5", "2", "n"}, secrets: []string{"bad", "good\n"}}
	name, model, err := OnlineAdd(context.Background(), cfg, p, "")
	if err != nil || name != "groq" || model != "m2" {
		t.Fatalf("%q %q %v\n%s", name, model, err, p.all())
	}
	out := p.all()
	if strings.Contains(out, "good") || strings.Contains(out, "bad") {
		t.Fatalf("a key was printed:\n%s", out)
	}
	for _, want := range []string{"key rejected by groq", "key entered (hidden)", "[1] m1", "groq added (model m2)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	re, _ := config.Load()
	if re.Providers["groq"].DefaultModel != "m2" || re.ProviderKey("groq") != "good" || re.DefaultProvider == "groq" {
		t.Fatalf("%+v key=%q default=%s", re.Providers["groq"], re.ProviderKey("groq"), re.DefaultProvider)
	}
}

func TestOnlineAddPlainBrowserAndCancel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubOnlineUI(t)
	cfg := config.Default()
	p := &scriptedPrompter{answers: []string{"1", "1", "y"}} // openrouter, browser, default yes
	name, _, err := OnlineAdd(context.Background(), cfg, p, "openrouter")
	if err != nil || name != "openrouter" {
		t.Fatalf("%q %v\n%s", name, err, p.all())
	}
	re, _ := config.Load()
	if re.DefaultProvider != "openrouter" || re.ProviderKey("openrouter") != "sk-or-test" {
		t.Fatalf("%+v", re)
	}
	// Withdrawn at the key step: nothing written.
	home2 := t.TempDir()
	t.Setenv("HOME", home2)
	p = &scriptedPrompter{answers: []string{"5"}} // groq: no keys page to open
	if name, _, err := OnlineAdd(context.Background(), config.Default(), p, ""); name != "" || err != nil {
		t.Fatalf("cancel: %q %v", name, err)
	}
	if _, err := os.Stat(filepath.Join(home2, ".be-code", "credentials.json")); !os.IsNotExist(err) {
		t.Fatal("cancel wrote a credential")
	}
	if !strings.Contains(p.all(), "cancelled; nothing was stored") {
		t.Fatalf("%s", p.all())
	}
}

func TestOnlineAddPlainNonTTYRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r := newTestREPL(t)
	// os.Stdin under `go test` is not a terminal.
	out := capture(t, func() { r.command(context.Background(), "/online add groq") })
	if !strings.Contains(out, "needs a terminal") {
		t.Fatalf("%q", out)
	}
}

func TestOnlineAddPlainListsQuickSignInFirstAndOpensTheKeysPage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubOnlineUI(t)
	opened := ""
	onlineOpenPage = func(u string) bool { opened = u; return true }
	cfg := config.Default()
	// [2] openai, model 1, default no
	p := &scriptedPrompter{answers: []string{"2", "1", "n"}, secrets: []string{"good"}}
	name, _, err := OnlineAdd(context.Background(), cfg, p, "")
	if err != nil || name != "openai" {
		t.Fatalf("%q %v\n%s", name, err, p.all())
	}
	out := p.all()
	for _, want := range []string{
		"[1] openrouter   sign in with browser, or paste a key",
		"[2] openai       opens the API keys page, then paste a key",
		"[3] anthropic    opens the API keys page, then paste a key",
		"[4] gemini       opens the API keys page, then paste a key",
		"[5] groq         paste a key",
		"[8] custom OpenAI-compatible endpoint",
		"opened https://platform.openai.com/api-keys in your browser; create a key there and paste it below",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if opened != "https://platform.openai.com/api-keys" {
		t.Fatalf("opened %q", opened)
	}
}

func TestOnlineAddPlainAsksForAWorkspace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubOnlineUI(t)
	onlineOpenPage = func(string) bool { return false }
	secrets := 0
	onlineValidate = func(ctx context.Context, ch setup.Choice, key string) ([]provider.ModelInfo, error) {
		if ch.Headers[setup.WorkspaceHeader] != "wrkspc_01" {
			return nil, &setup.WorkspaceNeeded{Provider: ch.Name}
		}
		return []provider.ModelInfo{{ID: "claude-x"}}, nil
	}
	p := &scriptedPrompter{answers: []string{"wrkspc_01", "1", "n"}, secrets: []string{"good"}}
	name, model, err := OnlineAdd(context.Background(), config.Default(), p, "anthropic")
	if err != nil || name != "anthropic" || model != "claude-x" {
		t.Fatalf("%q %q %v\n%s", name, model, err, p.all())
	}
	if !strings.Contains(p.all(), setup.WorkspacePrompt) || len(p.secrets) != 0 {
		t.Fatalf("prompted wrong:\n%s", p.all())
	}
	_ = secrets
	re, _ := config.Load()
	if re.Providers["anthropic"].Headers[setup.WorkspaceHeader] != "wrkspc_01" {
		t.Fatalf("header not saved: %+v", re.Providers["anthropic"])
	}
}
