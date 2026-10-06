package mods

import (
	"strings"
	"testing"
)

// A capability name is a global key in the registry, so a duplicate is a
// runtime failure that two separately-correct packages can cause together.
// The table exists to make that visible by reading — which only works if
// something checks it.
func TestEveryCapabilityNameIsUnique(t *testing.T) {
	seen := map[string]int{}
	for _, name := range All {
		if strings.TrimSpace(string(name)) == "" {
			t.Fatal("the table contains an empty capability name")
		}
		seen[string(name)]++
	}
	for name, count := range seen {
		if count != 1 {
			t.Fatalf("capability %q appears %d times in the table", name, count)
		}
	}
	if len(seen) != len(All) {
		t.Fatalf("the table holds %d entries but only %d distinct names", len(All), len(seen))
	}
}

// Every name is namespaced, so a service cannot collide with a roost-kit
// infrastructure capability — a service named "chat" and a hypothetical kit
// transport named "chat" would otherwise be the same key.
func TestEveryCapabilityNameIsNamespaced(t *testing.T) {
	for _, name := range All {
		if !strings.HasPrefix(string(name), "service.") {
			t.Fatalf("capability %q is not namespaced under \"service.\"", name)
		}
	}
}

func TestKeyPrefixRejectsWhitespace(t *testing.T) {
	for _, prefix := range []string{"roost mail", "roost\tmail"} {
		if err := CheckKeyPrefix("mail", prefix); err == nil || !strings.Contains(err.Error(), "mail.key_prefix") {
			t.Fatalf("CheckKeyPrefix(%q) = %v", prefix, err)
		}
	}
	if err := CheckKeyPrefix("mail", "roost:mail"); err != nil {
		t.Fatal(err)
	}
}

func TestClusterKeyPrefixUsesFirstRedisHashTag(t *testing.T) {
	for _, prefix := range []string{"plain", "{}", "{open", "{}:{valid}", "{valid}", "before}{valid}:suffix", "{{nested}"} {
		for _, cluster := range []bool{false, true} {
			t.Run(prefix+"/"+map[bool]string{false: "single", true: "cluster"}[cluster], func(t *testing.T) {
				var addrs []string
				if cluster {
					addrs = []string{"127.0.0.1:1"}
				}
				err := ValidateClusterKeyPrefix(addrs, "rank", prefix)
				invalid := cluster && (prefix == "plain" || prefix == "{}" || prefix == "{open" || prefix == "{}:{valid}")
				if (err != nil) != invalid {
					t.Fatalf("prefix=%q cluster=%v err=%v", prefix, cluster, err)
				}
			})
		}
	}
}
