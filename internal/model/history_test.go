package model

import (
	"errors"
	"reflect"
	"strconv"
	"testing"
)

func TestCommonBasesLinearForkAndUnrelatedHistories(t *testing.T) {
	parents := map[string][]string{
		"root":      nil,
		"base":      {"root"},
		"left":      {"base"},
		"left-tip":  {"left"},
		"right":     {"base"},
		"right-tip": {"right"},
		"other":     nil,
	}
	for _, tc := range []struct {
		left, right string
		want        []string
	}{
		{"left-tip", "right-tip", []string{"base"}},
		{"left-tip", "left", []string{"left"}},
		{"left-tip", "other", nil},
	} {
		got, err := CommonBases(parents, tc.left, tc.right)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("CommonBases(%q,%q) = %v, %v; want %v", tc.left, tc.right, got, err, tc.want)
		}
	}
}

func TestCommonBasesPreservesCrisscrossResults(t *testing.T) {
	parents := map[string][]string{
		"base-a":   nil,
		"base-b":   nil,
		"common-a": {"base-a"},
		"common-b": {"base-b"},
		"left":     {"common-a", "common-b"},
		"right":    {"common-a", "common-b"},
	}
	got, err := CommonBases(parents, "left", "right")
	if err != nil || !reflect.DeepEqual(got, []string{"common-a", "common-b"}) {
		t.Fatalf("CommonBases() = %v, %v; want both best bases", got, err)
	}
}

func TestCommonBasesRejectsMalformedReachableHistory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		parents map[string][]string
		left    string
		right   string
	}{
		{"empty tip", map[string][]string{"a": nil}, "", "a"},
		{"missing tip", map[string][]string{}, "missing", "missing"},
		{"missing parent", map[string][]string{"a": {"missing"}}, "a", "a"},
		{"empty parent", map[string][]string{"a": {""}}, "a", "a"},
		{"duplicate parent", map[string][]string{"a": {"b", "b"}, "b": nil}, "a", "a"},
		{"cycle", map[string][]string{"a": {"b"}, "b": {"c"}, "c": {"a"}}, "a", "a"},
		{"too many parents", map[string][]string{"a": {"b", "c", "d"}, "b": nil, "c": nil, "d": nil}, "a", "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := CommonBases(tc.parents, tc.left, tc.right); !errors.Is(err, ErrInvalidHistory) || got != nil {
				t.Fatalf("CommonBases() = %v, %v; want ErrInvalidHistory", got, err)
			}
		})
	}
}

func TestCommonBasesIgnoresUnreachableRecordsAndHandlesDeepHistory(t *testing.T) {
	parents := map[string][]string{
		"left":        nil,
		"right":       nil,
		"unreachable": {"missing"},
	}
	if got, err := CommonBases(parents, "left", "right"); err != nil || len(got) != 0 {
		t.Fatalf("unrelated tips with junk record: got=%v err=%v", got, err)
	}

	const depth = 12000
	deep := make(map[string][]string, depth)
	previous := ""
	for i := 0; i < depth; i++ {
		id := "commit-" + strconv.Itoa(i)
		if previous == "" {
			deep[id] = nil
		} else {
			deep[id] = []string{previous}
		}
		previous = id
	}
	got, err := CommonBases(deep, previous, previous)
	if err != nil || !reflect.DeepEqual(got, []string{previous}) {
		t.Fatalf("deep CommonBases() = %v, %v; want tip %q", got, err, previous)
	}
}
