package live

import "io"

// CRLFWriter translates bare LF into CRLF on its way to w.
//
// Every attached terminal is in raw mode for the life of an attach (see
// Attach), and raw mode turns off the kernel's output post-processing: a bare
// "\n" then moves the cursor down without returning it to column 0, so a
// multi-line message written straight to the host's fan-out output
// staircases across the screen. Bubble Tea's own frames already carry "\r\n",
// but the plain lines the host prints around the program — "finishing
// session...", the resume line — do not. Wrap the writer for those.
//
// A "\r\n" already in the stream is passed through unchanged rather than
// turned into "\r\r\n", so text that is already correct stays correct.
type CRLFWriter struct {
	w      io.Writer
	lastCR bool // the previous byte written was '\r'
}

// NewCRLFWriter wraps w so that bare LF becomes CRLF.
func NewCRLFWriter(w io.Writer) *CRLFWriter { return &CRLFWriter{w: w} }

func (c *CRLFWriter) Write(p []byte) (int, error) {
	out := make([]byte, 0, len(p)+8)
	for _, b := range p {
		if b == '\n' && !c.lastCR {
			out = append(out, '\r')
		}
		c.lastCR = b == '\r'
		out = append(out, b)
	}
	if _, err := c.w.Write(out); err != nil {
		return 0, err
	}
	// Report the caller's own byte count: the translation is invisible to it,
	// and a short count would look like a failed write.
	return len(p), nil
}
