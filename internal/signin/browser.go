// internal/signin/browser.go
package signin

import (
	"os"
	"os/exec"
	"runtime"

	"github.com/brown-enterprises/be-code/internal/procattr"
)

// OpenBrowser opens url in the person's default browser. The command is
// started, not waited for: xdg-open returns at once, and a browser that
// was not running keeps the process otherwise.
func OpenBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	procattr.Hide(cmd)
	return cmd.Start()
}

// displayAvailable is false on a Linux box with no display: the URL is
// shown instead of launched, and the pasted redirect finishes the flow.
func displayAvailable() bool {
	if runtime.GOOS != "linux" {
		return true
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}
