package inbox

import (
	"regexp"
	"strings"
)

// MACFor is the hardware address the ARP table holds for ip, or "". Best
// effort by design (spec §3.4): only a device on this subnet appears, and it
// is consulted only for an IP nobody has bound. Never for loopback.
func MACFor(ip string) string {
	if ip == "" || strings.HasPrefix(ip, "127.") || ip == "::1" {
		return ""
	}
	return macFor(ip)
}

var macRe = regexp.MustCompile(`(?i)\b([0-9a-f]{2})[:-]([0-9a-f]{2})[:-]([0-9a-f]{2})[:-]([0-9a-f]{2})[:-]([0-9a-f]{2})[:-]([0-9a-f]{2})\b`)

func normMAC(s string) string {
	m := macRe.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	mac := strings.ToLower(strings.Join(m[1:], ":"))
	if mac == "00:00:00:00:00:00" {
		return ""
	}
	return mac
}

// parseProcArp reads Linux's /proc/net/arp: columns IP, HW type, flags,
// HW address; flags 0x0 is an incomplete entry.
func parseProcArp(text, ip string) string {
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[0] != ip {
			continue
		}
		if f[2] == "0x0" {
			return ""
		}
		return normMAC(f[3])
	}
	return ""
}

// parseArpA reads `arp -a` on Windows ("192.168.1.38  aa-bb-cc-dd-ee-ff
// dynamic") and macOS ("? (192.168.1.40) at aa:bb:cc:00:11:22 on en0").
func parseArpA(text, ip string) string {
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, ip) {
			continue
		}
		fields := strings.Fields(line)
		hit := false
		for _, f := range fields {
			if f == ip || f == "("+ip+")" {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		return normMAC(line)
	}
	return ""
}
