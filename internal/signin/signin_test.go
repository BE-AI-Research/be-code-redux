// internal/signin/signin_test.go
package signin

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeOpenRouter records the authorize request and answers the exchange
// only when the verifier matches the challenge it saw.
type fakeOpenRouter struct {
	srv       *httptest.Server
	challenge string
	callback  string
	refuse    string // when set, the exchange answers 400 with this error.message ("" for a bodyless 500)
	refuseRaw bool
	wantCode  string        // the code the exchange accepts; "" means "the-code"
	delay     time.Duration // how long the exchange takes to answer
}

func newFake(t *testing.T) *fakeOpenRouter {
	t.Helper()
	f := &fakeOpenRouter{}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		f.challenge = r.URL.Query().Get("code_challenge")
		f.callback = r.URL.Query().Get("callback_url")
		if r.URL.Query().Get("code_challenge_method") != "S256" {
			t.Errorf("method %q", r.URL.Query().Get("code_challenge_method"))
		}
	})
	mux.HandleFunc("/api/v1/auth/keys", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Code, CodeVerifier, Method string }
		var raw map[string]string
		json.NewDecoder(r.Body).Decode(&raw)
		body.Code, body.CodeVerifier, body.Method = raw["code"], raw["code_verifier"], raw["code_challenge_method"]
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		want := f.wantCode
		if want == "" {
			want = "the-code"
		}
		if f.refuseRaw {
			w.WriteHeader(500)
			return
		}
		if f.refuse != "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": f.refuse}})
			return
		}
		sum := sha256.Sum256([]byte(body.CodeVerifier))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge || body.Code != want || body.Method != "S256" {
			w.WriteHeader(403)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"key": "sk-or-test"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOpenRouter) options(t *testing.T, pasted <-chan string) Options {
	t.Helper()
	return Options{
		AuthorizeURL: f.srv.URL + "/auth",
		ExchangeURL:  f.srv.URL + "/api/v1/auth/keys",
		Open: func(u string) error {
			// Stand in for the browser: hit /auth so the fake sees the challenge, then redirect to the callback.
			resp, err := http.Get(u)
			if err != nil {
				return err
			}
			resp.Body.Close()
			go func() {
				time.Sleep(20 * time.Millisecond)
				r, err := http.Get(f.callback + "?code=the-code")
				if err == nil {
					r.Body.Close()
				}
			}()
			return nil
		},
		Show:    func(string) {},
		Pasted:  pasted,
		Timeout: 5 * time.Second,
	}
}

func TestBrowserPath(t *testing.T) {
	f := newFake(t)
	key, err := SignIn(context.Background(), f.options(t, nil))
	if err != nil || key != "sk-or-test" {
		t.Fatalf("%q %v", key, err)
	}
	if !strings.HasPrefix(f.callback, "http://127.0.0.1:") || !strings.Contains(f.callback, "/cb/") {
		t.Fatalf("callback %q", f.callback)
	}
}

func TestPastedURLDecodesCode(t *testing.T) {
	f := newFake(t)
	// A code with characters a query must escape, so the decode is
	// really exercised (M2, final review).
	f.wantCode = "a/b+c="
	pasted := make(chan string, 1)
	o := f.options(t, pasted)
	o.Open = func(u string) error {
		resp, err := http.Get(u) // the fake must still see the challenge
		if err == nil {
			resp.Body.Close()
		}
		pasted <- "  " + f.callback + "?code=" + url.QueryEscape(f.wantCode) + "&x=1\n"
		return nil
	}
	key, err := SignIn(context.Background(), o)
	if err != nil || key != "sk-or-test" {
		t.Fatalf("%q %v", key, err)
	}
}

func TestPastedBareCode(t *testing.T) {
	f := newFake(t)
	pasted := make(chan string, 1)
	o := f.options(t, pasted)
	o.Open = func(u string) error {
		resp, err := http.Get(u)
		if err == nil {
			resp.Body.Close()
		}
		pasted <- " the-code \n"
		return nil
	}
	if key, err := SignIn(context.Background(), o); err != nil || key != "sk-or-test" {
		t.Fatalf("%q %v", key, err)
	}
}

func TestWrongPathCallbackIgnored(t *testing.T) {
	f := newFake(t)
	o := f.options(t, nil)
	o.Timeout = 300 * time.Millisecond
	o.Open = func(u string) error {
		resp, err := http.Get(u)
		if err == nil {
			resp.Body.Close()
		}
		base := f.callback[:strings.LastIndex(f.callback, "/cb/")]
		r, err := http.Get(base + "/cb/0000000000000000?code=the-code")
		if err == nil {
			r.Body.Close()
		}
		return nil
	}
	if _, err := SignIn(context.Background(), o); !errors.Is(err, ErrCancelled) {
		t.Fatalf("a callback on another path must not count: %v", err)
	}
}

func TestExchangeRefusals(t *testing.T) {
	f := newFake(t)
	f.refuse = "bad code"
	_, err := SignIn(context.Background(), f.options(t, nil))
	if err == nil || !strings.Contains(err.Error(), "bad code") || strings.Contains(err.Error(), "the-code") {
		t.Fatalf("refusal must carry error.message and never the code: %v", err)
	}
	f.refuse, f.refuseRaw = "", true
	_, err = SignIn(context.Background(), f.options(t, nil))
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("bodyless refusal: %v", err)
	}
}

func TestTimeoutAndCancel(t *testing.T) {
	f := newFake(t)
	o := f.options(t, nil)
	o.Open = func(string) error { return nil } // nobody comes back
	o.Timeout = 200 * time.Millisecond
	if _, err := SignIn(context.Background(), o); !errors.Is(err, ErrCancelled) {
		t.Fatalf("timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	o.Timeout = 5 * time.Second
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := SignIn(ctx, o); !errors.Is(err, ErrCancelled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestLaunchFailureShowsURL(t *testing.T) {
	f := newFake(t)
	pasted := make(chan string, 1)
	o := f.options(t, pasted)
	shown := ""
	o.Open = func(string) error { return errors.New("no browser") }
	o.Show = func(u string) {
		shown = u
		resp, err := http.Get(u)
		if err == nil {
			resp.Body.Close()
		}
		pasted <- "the-code"
	}
	if key, err := SignIn(context.Background(), o); err != nil || key != "sk-or-test" || !strings.Contains(shown, "code_challenge=") {
		t.Fatalf("%q %v shown=%q", key, err, shown)
	}
}

func TestCodeFromPaste(t *testing.T) {
	for in, want := range map[string]string{
		"abc":                                    "abc",
		"  abc\n":                                "abc",
		"http://127.0.0.1:1/cb/x?code=a%20b&y=1": "a b",
		"http://127.0.0.1:1/cb/x?y=1":            "",
		"":                                       "",
	} {
		if got := CodeFromPaste(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestMain(m *testing.M) {
	os.Setenv("DISPLAY", ":0") // so the Open path runs on a headless box
	os.Exit(m.Run())
}

// R5 (final review): the exchange has its own fifteen-second bound, not
// what is left of the wait — a code arriving at the end of the wait still
// completes its exchange.
func TestExchangeOutlivesTheWait(t *testing.T) {
	f := newFake(t)
	f.delay = 300 * time.Millisecond
	pasted := make(chan string, 1)
	o := f.options(t, pasted)
	o.Timeout = 100 * time.Millisecond
	o.Open = func(u string) error {
		resp, err := http.Get(u)
		if err == nil {
			resp.Body.Close()
		}
		pasted <- "the-code"
		return nil
	}
	if key, err := SignIn(context.Background(), o); err != nil || key != "sk-or-test" {
		t.Fatalf("exchange cut short by the wait: got %d chars, %v", len(key), err)
	}
}
