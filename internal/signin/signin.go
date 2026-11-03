// internal/signin/signin.go
// Package signin is OpenRouter's PKCE sign-in, which hands a third-party
// app a user-scoped API key with no client registration (spec §4). A
// leaf: stdlib plus procattr for the browser launch. The endpoints are
// Options fields so tests run the whole flow against an in-process fake.
package signin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Options configures one SignIn. Zero values take the OpenRouter defaults.
type Options struct {
	AuthorizeURL string
	ExchangeURL  string
	Open         func(url string) error
	Show         func(url string)
	Pasted       <-chan string
	Timeout      time.Duration
	Client       *http.Client
}

// ErrCancelled is the answer to a timeout, a context cancel, or a wait
// that ended with nothing usable: the one message the UIs print.
var ErrCancelled = errors.New("sign-in cancelled; nothing was stored")

const (
	defaultAuthorize = "https://openrouter.ai/auth"
	defaultExchange  = "https://openrouter.ai/api/v1/auth/keys"
	defaultTimeout   = 5 * time.Minute
	exchangeTimeout  = 15 * time.Second
)

// SignIn runs the flow and returns the key OpenRouter issued.
func SignIn(ctx context.Context, o Options) (string, error) {
	if o.AuthorizeURL == "" {
		o.AuthorizeURL = defaultAuthorize
	}
	if o.ExchangeURL == "" {
		o.ExchangeURL = defaultExchange
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: exchangeTimeout}
	}
	if o.Show == nil {
		o.Show = func(string) {}
	}
	verifier, challenge, err := pkce()
	if err != nil {
		return "", err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("sign-in: %w", err)
	}
	defer ln.Close()
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	path := "/cb/" + hex.EncodeToString(id[:])
	codes := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if code == "" {
			http.Error(w, "no code in the callback", http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, "You can return to BE-Code.")
		select {
		case codes <- code:
		default:
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	callback := fmt.Sprintf("http://%s%s", ln.Addr().String(), path)
	q := url.Values{}
	q.Set("callback_url", callback)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	authURL := o.AuthorizeURL + "?" + q.Encode()
	if o.Open == nil || !displayAvailable() {
		o.Show(authURL)
	} else if err := o.Open(authURL); err != nil {
		o.Show(authURL)
	}

	wctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	var code string
	for code == "" {
		select {
		case code = <-codes:
		case s, ok := <-o.Pasted:
			if !ok {
				return "", ErrCancelled
			}
			code = CodeFromPaste(s)
		case <-wctx.Done():
			return "", ErrCancelled
		}
	}
	// The exchange has its own bound (spec §4 step 5), under the caller's
	// context, not the wait's: a code that arrives near the end of the five
	// minutes must still get its full fifteen seconds.
	ectx, ecancel := context.WithTimeout(ctx, exchangeTimeout)
	defer ecancel()
	return exchange(ectx, o, code, verifier)
}

// CodeFromPaste reads the code out of what a person pasted: the whole
// redirect URL (the browser's address bar after a failed localhost
// redirect) or the bare code.
func CodeFromPaste(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return ""
		}
		return u.Query().Get("code")
	}
	return s
}

func pkce() (verifier, challenge string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func exchange(ctx context.Context, o Options, code, verifier string) (string, error) {
	body, _ := json.Marshal(map[string]string{"code": code, "code_verifier": verifier, "code_challenge_method": "S256"})
	req, err := http.NewRequestWithContext(ctx, "POST", o.ExchangeURL, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("sign-in: exchanging the code: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct{ Message string } `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			return "", fmt.Errorf("sign-in refused: %s", e.Error.Message)
		}
		return "", fmt.Errorf("sign-in refused: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Key == "" {
		return "", errors.New("sign-in: the exchange returned no key")
	}
	return out.Key, nil
}
