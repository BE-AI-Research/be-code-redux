package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
)

func TestWebSearchGooglePSE(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`{"items":[{"title":"Go slices","link":"https://go.dev/blog/slices","snippet":"Slices are views."},{"title":"Spec","link":"https://go.dev/ref/spec","snippet":"The Go spec."}]}`))
	}))
	defer srv.Close()
	t.Setenv("TEST_PSE_KEY", "k123")
	reg, _ := NewRegistry(t.TempDir(), nil)
	reg.AddTool(NewWebSearch(WebSearchConfig{CX: "cx9", APIKeyEnv: "TEST_PSE_KEY", MaxResults: 5, Endpoint: srv.URL}))
	res := reg.Dispatch(context.Background(), provider.ToolCall{Name: "web_search", Arguments: `{"query":"go slices"}`})
	if res.IsError {
		t.Fatal(res.Content)
	}
	for _, want := range []string{"Go slices", "https://go.dev/blog/slices", "Slices are views.", "2."} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("result lacks %q:\n%s", want, res.Content)
		}
	}
	for _, want := range []string{"key=k123", "cx=cx9", "q=go+slices"} {
		if !strings.Contains(gotQuery, want) {
			t.Fatalf("request lacks %q: %s", want, gotQuery)
		}
	}
}

func TestWebSearchMissingKeyIsClearError(t *testing.T) {
	reg, _ := NewRegistry(t.TempDir(), nil)
	reg.AddTool(NewWebSearch(WebSearchConfig{CX: "cx9", APIKeyEnv: "UNSET_PSE_KEY_XYZ"}))
	res := reg.Dispatch(context.Background(), provider.ToolCall{Name: "web_search", Arguments: `{"query":"x"}`})
	if !res.IsError || !strings.Contains(res.Content, "UNSET_PSE_KEY_XYZ") {
		t.Fatalf("expected an error naming the env var, got %+v", res)
	}
}

func TestWebFetchStripsHTMLAndCaps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>T</title><style>p{}</style><script>var x=1;</script></head><body><h1>Hello &amp; welcome</h1><p>First para.</p><p>` + strings.Repeat("filler ", 20000) + `</p></body></html>`))
	}))
	defer srv.Close()
	reg, _ := NewRegistry(t.TempDir(), nil)
	reg.SetMaxOutput(2000)
	reg.AddTool(NewWebFetch())
	res := reg.Dispatch(context.Background(), provider.ToolCall{Name: "web_fetch", Arguments: `{"url":"` + srv.URL + `/page"}`})
	if res.IsError {
		t.Fatal(res.Content)
	}
	if strings.Contains(res.Content, "<p>") || strings.Contains(res.Content, "var x=1") || !strings.Contains(res.Content, "Hello & welcome") {
		t.Fatalf("html not stripped: %.200s", res.Content)
	}
	if len(res.Content) > 2500 {
		t.Fatalf("output not capped: %d", len(res.Content))
	}
}

// Pages with a <main>/<article> region return that region first, so the
// model sees the document rather than the site navigation.
func TestWebFetchPrefersMainContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body><nav><a href="/">Home</a><a href="/x">Skip to Main Content</a></nav><main><h1>Effective Go</h1><p>Introduction text.</p></main><footer>Copyright</footer></body></html>`))
	}))
	defer srv.Close()
	reg, _ := NewRegistry(t.TempDir(), nil)
	reg.AddTool(NewWebFetch())
	res := reg.Dispatch(context.Background(), provider.ToolCall{Name: "web_fetch", Arguments: `{"url":"` + srv.URL + `"}`})
	body := res.Content[strings.Index(res.Content, "\n")+1:]
	if !strings.HasPrefix(body, "Effective Go") {
		t.Fatalf("main content not first:\n%s", body)
	}
	if strings.Contains(body, "Skip to Main Content") {
		t.Fatalf("navigation not dropped:\n%s", body)
	}
}
