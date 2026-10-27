package model

import "testing"

func TestValidName(t *testing.T) {
	for _, name := range []string{"main", "feature/one", "é"} {
		if !ValidName(name) {
			t.Errorf("ValidName(%q) = false", name)
		}
	}
	for _, name := range []string{"", "-option", "/root", "a/../b", "a//b", ".lode", "a\n"} {
		if ValidName(name) {
			t.Errorf("ValidName(%q) = true", name)
		}
	}
	if ValidName(string(make([]byte, 256))) {
		t.Error("accepted name longer than 255 bytes")
	}
}
