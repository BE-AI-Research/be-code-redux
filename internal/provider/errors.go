package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// HTTPError is a non-2xx answer from an OpenAI-compatible server, kept
// typed so the agent can tell a rate limit (wait as long as the server
// asked) from a rejected key (never retry) without parsing text. Error()
// keeps the text the string classifier has always matched on.
type HTTPError struct {
	Provider   string
	Code       int
	Body       string
	RetryAfter time.Duration // from the Retry-After header; 0 when absent or unusable
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.Provider, e.Code, e.Body)
}

// ParseRetryAfter reads a Retry-After header: delay-seconds or an
// HTTP-date. Anything unparseable, negative or already past is 0; clamping
// an absurdly large value is the caller's job.
func ParseRetryAfter(h string, now time.Time) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if n, err := strconv.ParseInt(h, 10, 64); err == nil {
		if n <= 0 {
			return 0
		}
		const maxSecs = int64(1<<63-1) / int64(time.Second)
		if n > maxSecs {
			n = maxSecs
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// keyLike matches what an API key looks like inside a provider's error
// text (OpenAI echoes a partly masked key back: "sk-proj-abc***xyz").
// Nothing shaped like one is ever shown.
var keyLike = regexp.MustCompile(`\b(?:sk|gsk|key)[-_][A-Za-z0-9*_-]{6,}|\bAIza[0-9A-Za-z_-]{20,}`)

// ProviderMessage is the human-readable message inside the error body,
// when the body is the JSON shape OpenAI-compatible servers (and
// Anthropic) answer with: {"error":{"type":…,"message":…}} or
// {"error":"…"}. It is sanitised for a terminal: control characters
// removed, anything key-shaped redacted, at most 160 characters. Empty
// when the body is not that shape (HTML, plain text).
func (e *HTTPError) ProviderMessage() string {
	body := e.Body
	if i := strings.IndexByte(body, '{'); i > 0 {
		body = body[i:]
	}
	var shape struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal([]byte(body), &shape) != nil || len(shape.Error) == 0 {
		return ""
	}
	var msg string
	var obj struct{ Type, Message string }
	if json.Unmarshal(shape.Error, &obj) == nil && obj.Message != "" {
		msg = obj.Message
		if obj.Type != "" {
			msg = obj.Type + ": " + msg
		}
	} else if json.Unmarshal(shape.Error, &msg) != nil {
		return ""
	}
	msg = keyLike.ReplaceAllString(msg, "[redacted]")
	msg = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, msg)
	msg = strings.TrimSpace(msg)
	if len(msg) > 160 {
		msg = msg[:160] + "…"
	}
	return msg
}
