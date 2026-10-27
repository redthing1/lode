package model

import (
	"reflect"
	"testing"
)

func TestSelectionIntersection(t *testing.T) {
	selected := Selection{{Include: true}, {Path: "assets/cache", Include: false}, {Path: "assets/cache/keep", Include: true}}
	requested := Selection{{Path: "assets", Include: true}, {Path: "assets/cache/keep/private", Include: false}}
	got := selected.Intersect(requested)
	for path, want := range map[string]bool{
		"": false, "other": false, "assets": true, "assets/a": true,
		"assets/cache": false, "assets/cache/a": false,
		"assets/cache/keep": true, "assets/cache/keep/a": true,
		"assets/cache/keep/private": false, "assets/cache/keep/private/a": false,
	} {
		if got.Includes(path) != want {
			t.Errorf("intersection includes %q = %v, want %v", path, got.Includes(path), want)
		}
	}
	if !reflect.DeepEqual(got, requested.Intersect(selected)) {
		t.Fatal("intersection is not canonical")
	}
	if (Selection{}).Intersect(selected).Descends("") {
		t.Fatal("intersection with empty selection includes paths")
	}
}
