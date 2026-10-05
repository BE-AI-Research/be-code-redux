package provider

import (
	"fmt"
	"net/http"
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
