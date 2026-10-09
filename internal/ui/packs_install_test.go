package ui

import (
	"strings"
	"testing"
)

// Every subcommand that can raise a question must be blocking: Session.Ask
// relocks the mutex Update holds, and an inline question froze every attached
// terminal with the alt screen up.
func TestInstallingSubcommandsAreBlocking(t *testing.T) {
	for _, verb := range []string{"add", "update", "remove"} {
		if !IsPacksBlocking([]string{verb}) {
			t.Errorf("/packs %s must be blocking: it can raise pack_install", verb)
		}
	}
	if IsPacksBlocking(nil) {
		t.Error("a bare /packs listing must stay inline")
	}
}

func TestPacksUsageNamesTheNewVerbs(t *testing.T) {
	// The usage string is what a person sees after a typo, so it has to list
	// everything that exists.
	usage := packsUsage()
	for _, want := range []string{"add", "update", "remove", "on", "off", "default", "reload"} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage does not mention %q: %s", want, usage)
		}
	}
}

func TestSlashTableMentionsTheNewVerbs(t *testing.T) {
	var row string
	for _, c := range SlashCommandTable {
		if c.Name == "/packs" {
			row = c.Desc // SlashCommandInfo{Name, Desc, Args} — internal/ui/common.go:56
		}
	}
	if row == "" {
		t.Fatal("no /packs row in SlashCommandTable")
	}
	for _, want := range []string{"add", "remove"} {
		if !strings.Contains(row, want) {
			t.Errorf("the /packs row does not mention %q: %s", want, row)
		}
	}
}
