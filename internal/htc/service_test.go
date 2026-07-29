package htc

import (
	"sort"
	"strings"
	"testing"
)

// Every registry entry has to be complete and unique. A duplicate key or
// port would make one of the two entries unreachable, and a blank field
// would show up as an empty column in a listing rather than failing here.
func TestServiceRegistryIsWellFormed(t *testing.T) {
	seenKey := map[string]bool{}
	seenPort := map[string]bool{}
	for _, s := range serviceList {
		if s.Key == "" || s.Port == "" || s.Desc == "" {
			t.Errorf("incomplete entry: %+v", s)
		}
		if seenKey[s.Key] {
			t.Errorf("duplicate key %q", s.Key)
		}
		if seenPort[s.Port] {
			t.Errorf("duplicate port %q", s.Port)
		}
		seenKey[s.Key] = true
		seenPort[s.Port] = true

		// Keys go on the command line, so keep them shell-friendly.
		if strings.ContainsAny(s.Key, " \t$@") {
			t.Errorf("key %q needs quoting on a command line", s.Key)
		}
	}
}

func TestServiceRegistryIsFullyIndexed(t *testing.T) {
	if len(servicesByKey) != len(serviceList) || len(servicesByPort) != len(serviceList) {
		t.Fatalf("indexes hold %d/%d entries, registry has %d",
			len(servicesByKey), len(servicesByPort), len(serviceList))
	}
	for _, s := range serviceList {
		if got, ok := LookupService(s.Key); !ok || got.Port != s.Port {
			t.Errorf("LookupService(%q) = %+v, %v", s.Key, got, ok)
		}
		if got, ok := LookupService(s.Port); !ok || got.Key != s.Key {
			t.Errorf("LookupService(%q) = %+v, %v", s.Port, got, ok)
		}
		if got, ok := ServiceForPort(s.Port); !ok || got.Key != s.Key {
			t.Errorf("ServiceForPort(%q) = %+v, %v", s.Port, got, ok)
		}
	}
}

// An unknown port must come back as unknown. Falling back to some arbitrary
// known service would put a confident, wrong label on it.
func TestServiceForUnknownPort(t *testing.T) {
	if s, ok := ServiceForPort("iywys@$somethingNew"); ok {
		t.Errorf("unknown port resolved to %+v", s)
	}
	if _, ok := LookupService(""); ok {
		t.Error("empty name resolved to a service")
	}
}

func TestServicesSorted(t *testing.T) {
	got := Services()
	if len(got) != len(serviceList) {
		t.Fatalf("Services returned %d, want %d", len(got), len(serviceList))
	}
	if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i].Key < got[j].Key }) {
		t.Error("Services is not sorted by key")
	}
	// The caller gets a copy; mutating it must not corrupt the registry.
	got[0].Port = "clobbered"
	if _, ok := LookupService(serviceList[0].Port); !ok {
		t.Error("mutating the returned slice affected the registry")
	}
}

// The HID channel constant the input code dials has to be the same string
// the registry publishes, or the two would drift apart silently.
func TestHIDPortMatchesRegistry(t *testing.T) {
	s, ok := LookupService("hid")
	if !ok {
		t.Fatal("no hid entry in the registry")
	}
	if s.Port != hidPortName {
		t.Errorf("registry hid port = %q, dial constant = %q", s.Port, hidPortName)
	}
}
