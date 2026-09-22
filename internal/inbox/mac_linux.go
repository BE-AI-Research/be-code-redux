//go:build linux

package inbox

import "os"

func macFor(ip string) string {
	b, err := os.ReadFile("/proc/net/arp")
	if err != nil {
		return ""
	}
	return parseProcArp(string(b), ip)
}
