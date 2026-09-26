package browser

import (
	"reflect"
	"testing"
)

func TestConsentTiersAndSpecificity(t *testing.T) {
	sites := map[string]string{
		"*.mybank.com": "deny", "github.com": "watch", "staging.acme.lan": "allow",
		"*": "watch", "localhost": "allow",
	}
	// Map iteration order is random: build it many times to show the most
	// specific pattern wins regardless.
	for i := 0; i < 20; i++ {
		c, warns := NewConsent(sites)
		if len(warns) != 0 {
			t.Fatalf("warnings %v", warns)
		}
		for host, want := range map[string]Tier{
			"github.com": TierWatch, "www.mybank.com": TierDeny, "staging.acme.lan": TierAllow,
			"other.test": TierWatch, "localhost": TierAllow, "127.0.0.1": TierWatch,
		} {
			if got := c.Tier(host); got != want {
				t.Fatalf("Tier(%q) = %v, want %v", host, got, want)
			}
		}
	}
}

func TestConsentDefaults(t *testing.T) {
	c, _ := NewConsent(nil)
	for host, want := range map[string]Tier{
		"example.com": TierAsk, "192.168.1.10": TierAsk, "127.0.0.1": TierAllow,
		"[::1]:3000": TierAllow, "app.localhost": TierAllow, "localhost:8080": TierAllow, "": TierAllow,
	} {
		if got := c.Tier(host); got != want {
			t.Errorf("Tier(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestConsentNormalisesHosts(t *testing.T) {
	c, _ := NewConsent(map[string]string{"GitHub.com": "watch"})
	for _, h := range []string{"github.com", "GitHub.com:443", "github.com.", "GITHUB.COM"} {
		if c.Tier(h) != TierWatch {
			t.Errorf("Tier(%q) did not match the github.com rule", h)
		}
	}
	c.Grant("GitHub.com:443")
	if !c.Granted("github.com.") {
		t.Fatal("a grant for GitHub.com:443 does not cover github.com.")
	}
}

func TestConsentGrantsArePerExactHost(t *testing.T) {
	c, _ := NewConsent(nil)
	c.Grant("github.com")
	if !c.Granted("github.com") || c.Granted("gist.github.com") {
		t.Fatal("a grant leaked to another host")
	}
	c.Grant("acme.test")
	if got := c.Grants(); !reflect.DeepEqual(got, []string{"acme.test", "github.com"}) {
		t.Fatalf("grants %v", got)
	}
	if !c.Forget("github.com") || c.Forget("github.com") {
		t.Fatal("Forget reported wrongly")
	}
	if c.Granted("github.com") {
		t.Fatal("still granted after Forget")
	}
}

func TestConsentWarnsOnUnknownTier(t *testing.T) {
	c, warns := NewConsent(map[string]string{"x": "maybe"})
	want := `browser.sites["x"]: "maybe" is not one of allow|watch|deny; ignored`
	if len(warns) != 1 || warns[0] != want {
		t.Fatalf("warnings %q", warns)
	}
	if c.Tier("x") != TierAsk {
		t.Fatal("a bad tier did not fall back to the default")
	}
}

func TestHostOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://GitHub.com:443/acme": "github.com", "about:blank": "", "http://[::1]:3000/": "::1", "": "",
	} {
		if got := HostOf(in); got != want {
			t.Errorf("HostOf(%q) = %q, want %q", in, got, want)
		}
	}
}
