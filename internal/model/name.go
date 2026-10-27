package model

import "unicode/utf8"

// ValidName accepts a canonical relative branch name.
func ValidName(name string) bool {
	return len(name) <= 255 && utf8.ValidString(name) && name != "" && name[0] != '-' && ValidPath(name)
}
