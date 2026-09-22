package inbox

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
)

// Device is one place a user has been seen from.
type Device struct {
	IP        string    `json:"ip"`
	Login     string    `json:"login,omitempty"`
	MAC       string    `json:"mac,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

type UserRecord struct {
	Devices []Device `json:"devices"`
}

// Users is ~/.be-code/users.json: which devices may use which IDs.
type Users struct {
	Users map[string]*UserRecord `json:"users"`
}

// Terminal is what the session host knows about an attached terminal.
type Terminal struct {
	IP    string
	Login string
	User  string // chat.name from the terminal's own config, may be ""
	PID   int
}

// Resolution is Resolve's answer. Ask means the terminal must be prompted:
// with Choices when several IDs are bound to its IP, with none when it is
// new.
type Resolution struct {
	ID      string
	How     string // "config" | "ip" | "mac" | "" (Ask); "asked" is set by the caller after a prompt
	Choices []string
	Ask     bool
}

// UsersPath is ~/.be-code/users.json.
func UsersPath() (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "users.json"), nil
}

// Load reads the file; a missing file is an empty record.
func Load(path string) (*Users, error) {
	u := &Users{Users: map[string]*UserRecord{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return u, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, u); err != nil {
		return nil, err
	}
	if u.Users == nil {
		u.Users = map[string]*UserRecord{}
	}
	return u, nil
}

func save(path string, u *Users) error {
	b, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, b, 0o600)
}

// idsForIP lists the IDs with a device at ip, sorted.
func (u *Users) idsForIP(ip string) []string {
	var out []string
	for id, r := range u.Users {
		for _, d := range r.Devices {
			if d.IP == ip {
				out = append(out, id)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

func (u *Users) idForMAC(mac string) string {
	mac = strings.ToLower(mac)
	if mac == "" {
		return ""
	}
	for id, r := range u.Users {
		for _, d := range r.Devices {
			if strings.ToLower(d.MAC) == mac {
				return id
			}
		}
	}
	return ""
}

// touch records the terminal under id: a device row for its IP is updated
// or added. Caller holds the lock.
func (u *Users) touch(id string, t Terminal, mac string) {
	r := u.Users[id]
	if r == nil {
		r = &UserRecord{}
		u.Users[id] = r
	}
	now := time.Now()
	for i := range r.Devices {
		if r.Devices[i].IP == t.IP {
			r.Devices[i].LastSeen = now
			if t.Login != "" {
				r.Devices[i].Login = t.Login
			}
			if mac != "" {
				r.Devices[i].MAC = strings.ToLower(mac)
			}
			return
		}
	}
	r.Devices = append(r.Devices, Device{IP: t.IP, Login: t.Login, MAC: strings.ToLower(mac), FirstSeen: now, LastSeen: now})
}

// Bind records id as usable from the terminal's IP (the answer to a prompt,
// or the config name). mac may be "".
func Bind(path, id string, t Terminal, mac string) error {
	id, err := ValidID(id)
	if err != nil {
		return err
	}
	return withLock(path, func() error {
		u, err := Load(path)
		if err != nil {
			return err
		}
		u.touch(id, t, mac)
		return save(path, u)
	})
}

// Resolve decides who a terminal is (spec §3.3): the config name; else the
// one ID bound to its IP, or a choice among several; else — and only then —
// a MAC lookup for a device whose IP changed; else a prompt. lookupMAC may
// be nil. A config name or a MAC match binds the IP on the way.
//
// The one case that returns both a Resolution and an error is a config name
// that is not a valid ID: the error says why, for the person to read, and
// Ask is true so the terminal is prompted as if it had no name. Every other
// error comes with a zero Resolution.
func Resolve(path string, t Terminal, lookupMAC func(ip string) string) (Resolution, error) {
	if t.User != "" {
		id, err := ValidID(t.User)
		if err != nil {
			return Resolution{Ask: true}, err
		}
		if err := Bind(path, id, t, ""); err != nil {
			return Resolution{}, err
		}
		return Resolution{ID: id, How: "config"}, nil
	}
	u, err := Load(path)
	if err != nil {
		return Resolution{}, err
	}
	switch ids := u.idsForIP(t.IP); len(ids) {
	case 1:
		// Only last_seen moves here; the ID is known whatever the disk
		// says, so a failed touch is not worth refusing a name over. The
		// MAC branch below is different: its Bind is what makes the new IP
		// known next time, so its failure is returned.
		_ = Bind(path, ids[0], t, "")
		return Resolution{ID: ids[0], How: "ip"}, nil
	case 0:
	default:
		return Resolution{Ask: true, Choices: ids}, nil
	}
	if lookupMAC != nil {
		if mac := lookupMAC(t.IP); mac != "" {
			if id := u.idForMAC(mac); id != "" {
				if err := Bind(path, id, t, mac); err != nil {
					return Resolution{}, err
				}
				return Resolution{ID: id, How: "mac"}, nil
			}
		}
	}
	return Resolution{Ask: true}, nil
}
