package inbox

import "testing"

const procArp = `IP address       HW type     Flags       HW address            Mask     Device
192.168.1.38     0x1         0x2         aa:bb:cc:dd:ee:ff     *        eth0
192.168.1.1      0x1         0x2         11:22:33:44:55:66     *        eth0
192.168.1.99     0x1         0x0         00:00:00:00:00:00     *        eth0
`

const arpA = `Interface: 192.168.1.10 --- 0x5
  Internet Address      Physical Address      Type
  192.168.1.38          aa-bb-cc-dd-ee-ff     dynamic
  192.168.1.1           11-22-33-44-55-66     dynamic
? (192.168.1.40) at aa:bb:cc:00:11:22 on en0 ifscope [ethernet]
`

func TestParseProcArp(t *testing.T) {
	if got := parseProcArp(procArp, "192.168.1.38"); got != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("got %q", got)
	}
	if got := parseProcArp(procArp, "192.168.1.99"); got != "" {
		t.Fatalf("an incomplete entry (flags 0x0, zero MAC) must be a miss, got %q", got)
	}
	if got := parseProcArp(procArp, "10.0.0.1"); got != "" {
		t.Fatalf("miss returned %q", got)
	}
}

func TestParseArpA(t *testing.T) {
	if got := parseArpA(arpA, "192.168.1.38"); got != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("windows form: got %q", got)
	}
	if got := parseArpA(arpA, "192.168.1.40"); got != "aa:bb:cc:00:11:22" {
		t.Fatalf("macOS form: got %q", got)
	}
	if got := parseArpA(arpA, "1.1.1.1"); got != "" {
		t.Fatalf("miss returned %q", got)
	}
}

func TestMACForLoopbackIsAMiss(t *testing.T) {
	if got := MACFor("127.0.0.1"); got != "" {
		t.Fatalf("got %q", got)
	}
}
