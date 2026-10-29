package project

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/model"
)

func TestVerifyRejectsBrokenTreesAndReferences(t *testing.T) {
	for _, damage := range []string{"hash", "native entry", "missing group", "wrong slot", "branch", "tag", "expired metadata"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			f := openFixture(t, t.TempDir(), true)
			v := publish(t, f.store, "", []model.Entry{file("a", "bytes"), file("nested/.lode", "ordinary nested file")}, map[string]string{"a": "bytes", "nested/.lode": "ordinary nested file"})
			if err := f.store.Verify(ctx); err != nil {
				t.Fatal("valid tree", err)
			}
			stored, err := f.store.version(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			entries, err := f.store.directory(ctx, stored.root)
			if err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "hash", "native entry", "wrong slot":
				if damage == "hash" {
					entries[0].SHA256 = strings.Repeat("0", 64)
				} else if damage == "native entry" {
					entries[0].Size++
				}
				d, err := indexDirectory(stored.root, entries)
				if err != nil {
					t.Fatal(err)
				}
				if damage == "wrong slot" {
					i := groupIndex("a")
					d.slots[i], d.slots[(i+1)%groupCount] = d.slots[(i+1)%groupCount], d.slots[i]
				}
				tx, err := f.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if err := f.store.writeDirectories(ctx, tx, []directory{d}); err != nil {
					t.Fatal(err)
				}
				body, _ := json.Marshal(d.slots)
				if _, err := tx.Exec("UPDATE directories SET body=? WHERE project=? AND id=?", body, f.store.id, stored.root); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			case "missing group":
				_, err = f.db.Exec("DELETE FROM groups")
			case "branch":
				_, err = f.db.Exec("UPDATE branches SET tip='missing'")
			case "tag":
				_, err = f.db.Exec("INSERT INTO tags VALUES(?,'bad','')", f.store.id)
			case "expired metadata":
				_, err = f.db.Exec("UPDATE versions SET retained=0; UPDATE branches SET tip=''; DELETE FROM directories")
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.Verify(ctx); err == nil {
				t.Fatal("accepted damaged project")
			}
		})
	}
}

func TestVerifyDoesNotRequireExpiredContents(t *testing.T) {
	ctx := context.Background()
	f := openFixture(t, t.TempDir(), true)
	old := publish(t, f.store, "", []model.Entry{file("a", "old")}, map[string]string{"a": "old"})
	latest := publish(t, f.store, old.ID, []model.Entry{file("a", "new")}, map[string]string{"a": "new"})
	if err := f.store.SetRetention(ctx, model.Retention{Versions: 1, HistoryDays: 90}); err != nil {
		t.Fatal(err)
	}
	preview, err := f.store.PreviewCleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Cleanup(ctx, preview.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.File(ctx, old.ID, "a"); !errors.Is(err, ErrExpired) {
		t.Fatalf("expiry changed: %v", err)
	}
	if got := read(t, f.store, latest.ID, "a"); got != "new" {
		t.Fatal("retained file changed")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := f.store.Verify(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled verification: %v", err)
	}
}
