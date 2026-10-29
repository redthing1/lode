package workspace

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/redthing1/lode/internal/model"
)

type ignoreRule struct {
	pattern            string
	include, directory bool
}

// Defaults are deliberately limited to OS housekeeping, not project formats.
const defaultIgnores = ".DS_Store\nThumbs.db\ndesktop.ini\n"

func parseIgnores(text string) ([]ignoreRule, error) {
	var rules []ignoreRule
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		for strings.HasSuffix(line, " ") {
			backslashes := 0
			for j := len(line) - 2; j >= 0 && line[j] == '\\'; j-- {
				backslashes++
			}
			if backslashes%2 != 0 {
				break
			}
			line = line[:len(line)-1]
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rule := ignoreRule{}
		if strings.HasPrefix(line, "!") {
			rule.include, line = true, line[1:]
		}
		rule.directory = strings.HasSuffix(line, "/")
		line = strings.TrimSuffix(line, "/")
		anchored := strings.Contains(line, "/")
		line = strings.TrimPrefix(line, "/")
		// Braces are literal in ignore files, not doublestar alternatives.
		var glob strings.Builder
		escaped := false
		for _, r := range line {
			if !escaped && (r == '{' || r == '}') {
				glob.WriteByte('\\')
			}
			glob.WriteRune(r)
			if r == '\\' && !escaped {
				escaped = true
			} else {
				escaped = false
			}
		}
		rule.pattern = glob.String()
		if line == "" || !doublestar.ValidatePattern(rule.pattern) {
			return nil, fmt.Errorf("line %d: invalid ignore pattern", i+1)
		}
		if !anchored {
			rule.pattern = "**/" + rule.pattern
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

func ignoredPath(rules []ignoreRule, name string, directory bool) bool {
	// An excluded parent must be re-included before a child can be included.
	parts := strings.Split(name, "/")
	for n := 1; n <= len(parts); n++ {
		prefix := strings.Join(parts[:n], "/")
		for i := len(rules) - 1; i >= 0; i-- {
			rule := rules[i]
			if rule.directory && n == len(parts) && !directory {
				continue
			}
			if match, _ := doublestar.Match(rule.pattern, prefix); match {
				if !rule.include {
					return true
				}
				break
			}
		}
	}
	return false
}

func (w *Workspace) ignoreNew(incoming []model.Entry) (func(string, bool) bool, error) {
	rules, _ := parseIgnores(defaultIgnores)
	for _, name := range []string{".lodeignore", ".lode/ignore"} {
		info, err := w.root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s must be a regular file", name)
		}
		f, err := w.root.Open(name)
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
		err = errors.Join(err, f.Close())
		if err != nil {
			return nil, err
		}
		if len(b) > 1<<20 {
			return nil, fmt.Errorf("%s exceeds 1 MiB", name)
		}
		parsed, err := parseIgnores(string(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		rules = append(rules, parsed...)
	}
	// Baseline and incoming paths must remain visible for edits and collisions.
	protected := map[string]bool{}
	for _, entries := range [][]model.Entry{w.state.Baseline, incoming} {
		for _, e := range entries {
			for name := e.Path; name != "."; name = path.Dir(name) {
				if protected[name] {
					break
				}
				protected[name] = true
			}
		}
	}
	return func(name string, directory bool) bool {
		return !protected[name] && ignoredPath(rules, name, directory)
	}, nil
}
