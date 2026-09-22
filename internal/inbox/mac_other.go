//go:build !linux

package inbox

import (
	"context"
	"os/exec"
	"time"

	"github.com/brown-enterprises/be-code/internal/procattr"
)

func macFor(ip string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "arp", "-a")
	procattr.Hide(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return parseArpA(string(out), ip)
}
