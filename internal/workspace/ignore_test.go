package workspace

import "testing"

func TestIgnorePatterns(t *testing.T) {
	for _, tc := range []struct {
		pattern, path      string
		directory, ignored bool
	}{
		{"# comment\n\n*.tmp\r\n", "assets/a.tmp", false, true},
		{"*.tmp\n!important.tmp", "assets/important.tmp", false, false},
		{"cache/", "cache", false, false},
		{"cache/", "cache", true, true},
		{"cache/", "assets/cache/a", false, true},
		{"/cache/", "assets/cache/a", false, false},
		{"/cache/", "cache/a", false, true},
		{"cache/\n!cache/keep", "cache/keep", false, true},
		{"cache/*\n!cache/keep", "cache/keep", false, false},
		{"**/a/b", "x/a/a/b", false, true},
		{"a/**/b", "a/b", false, true},
		{"a/**/b", "a/x/y/b", false, true},
		{"x?.[ab]", "dir/x1.a", false, true},
		{"\\#literal\n\\!literal", "#literal", false, true},
		{"\\#literal\n\\!literal", "!literal", false, true},
		{"trail\\  ", "trail ", false, true},
		{"trail\\  ", "trail", false, false},
		{"{a,b}", "a", false, false},
		{"{a,b}", "{a,b}", false, true},
		{"[{]", "{", false, true},
		{defaultIgnores + "!.DS_Store\n", "deep/.DS_Store", false, false},
	} {
		rules, err := parseIgnores(tc.pattern)
		if err != nil {
			t.Fatal(err)
		}
		if got := ignoredPath(rules, tc.path, tc.directory); got != tc.ignored {
			t.Errorf("%q against %q (directory %v): %v, want %v", tc.pattern, tc.path, tc.directory, got, tc.ignored)
		}
	}
	if _, err := parseIgnores("valid\nbad["); err == nil {
		t.Fatal("malformed ignore accepted")
	}
}
