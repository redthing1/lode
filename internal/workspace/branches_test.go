package workspace

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

func TestSwitchKeepsIndependentEditAndReportsConflict(t *testing.T) {
	s, _, localDir, w := makeTransitionPair(t, map[string][]byte{"independent": []byte("base"), "conflict": []byte("base"), "remote": []byte("base")}, nil)
	root := w.State().Base
	if err := s.p.CreateBranch(context.Background(), "topic", root); err != nil {
		t.Fatal(err)
	}
	topicDir := filepath.Join(t.TempDir(), "topic")
	topic, err := Clone(context.Background(), s.srv.URL, s.p.Token(), topicDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := topic.Switch(context.Background(), "topic", nil); err != nil {
		t.Fatal(err)
	}
	write(t, topicDir, "conflict", []byte("remote conflict"))
	write(t, topicDir, "remote", []byte("remote only"))
	topicCommit := save(t, topic).ID
	closeWorkspace(t, topic)
	write(t, localDir, "independent", []byte("local independent"))
	write(t, localDir, "conflict", []byte("local conflict"))
	if err := w.Switch(context.Background(), "topic", nil); err == nil {
		t.Fatal("switch with an unresolved conflict succeeded")
	}
	// No part of the switch is applied while one selected file conflicts.
	if got := string(read(t, localDir, "independent")); got != "local independent" {
		t.Fatalf("independent edit changed on rejected switch: %q", got)
	}
	if err := w.Switch(context.Background(), "topic", []model.Choice{{Path: "conflict", Ours: true}}); err != nil {
		t.Fatal(err)
	}
	if got := string(read(t, localDir, "independent")); got != "local independent" {
		t.Fatalf("independent local edit lost: %q", got)
	}
	if got := string(read(t, localDir, "conflict")); got != "local conflict" {
		t.Fatalf("chosen local conflict = %q", got)
	}
	if w.State().Base != topicCommit || w.State().Branch != "topic" {
		t.Fatalf("switched state = %q/%q, want topic/%q", w.State().Branch, w.State().Base, topicCommit)
	}
	if got := string(read(t, localDir, "remote")); got != "remote only" {
		t.Fatalf("remote-only file = %q", got)
	}
	changes, err := w.Status(context.Background())
	if err != nil || len(changes) != 2 {
		t.Fatalf("status after switch = %#v, %v", changes, err)
	}
}

func TestMergePublishesWithoutChangingDirtyWorkspaceAndHasTwoParents(t *testing.T) {
	s, sourceDir, localDir, main := makeTransitionPair(t, map[string][]byte{"main": []byte("root main"), "topic": []byte("root topic")}, model.Selection{{Path: "main", Include: true}})
	root := main.State().Base
	if err := s.p.CreateBranch(context.Background(), "topic", root); err != nil {
		t.Fatal(err)
	}
	topicDir := filepath.Join(t.TempDir(), "topic")
	topic, err := Clone(context.Background(), s.srv.URL, s.p.Token(), topicDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := topic.Switch(context.Background(), "topic", nil); err != nil {
		t.Fatal(err)
	}
	write(t, topicDir, "topic", []byte("topic saved"))
	topicCommit := save(t, topic)
	closeWorkspace(t, topic)
	write(t, sourceDir, "main", []byte("main saved"))
	mainCommit := publishWorkspace(t, sourceDir)
	if err := main.Update(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	localBefore := read(t, localDir, "main")
	dirty := []byte("unsaved local edit")
	write(t, localDir, "main", dirty)
	stateBefore := main.State()
	merged, err := main.Merge(context.Background(), "topic", "tester", "join topic", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Parents) != 2 || merged.Parents[0] != mainCommit || merged.Parents[1] != topicCommit.ID {
		t.Fatalf("merge parents = %#v", merged.Parents)
	}
	if got := read(t, localDir, "main"); !bytes.Equal(got, dirty) {
		t.Fatalf("merge changed local dirty file: %q", got)
	}
	if main.State().Base != stateBefore.Base || main.State().Branch != stateBefore.Branch || !bytes.Equal(localBefore, []byte("main saved")) {
		t.Fatalf("merge changed workspace baseline: before=%#v after=%#v", stateBefore, main.State())
	}
	if got := string(readProjectFile(t, s.p, merged.ID, "main")); got != "main saved" {
		t.Fatalf("merged main content = %q", got)
	}
	if got := string(readProjectFile(t, s.p, merged.ID, "topic")); got != "topic saved" {
		t.Fatalf("merged topic content = %q", got)
	}
	// Merging itself needs no file reads: unloaded source content is published server-side.
	if _, err := os.Stat(filepath.Join(localDir, "topic")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("merge materialized unloaded source path: %v", err)
	}
}

func TestMergeNoOpClearsSaveJournal(t *testing.T) {
	s, _, localDir, w := makeTransitionPair(t, map[string][]byte{"a": []byte("root")}, nil)
	root := w.State().Base
	if err := s.p.CreateBranch(context.Background(), "already", root); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Merge(context.Background(), "already", "tester", "noop", "", nil); !errors.Is(err, project.ErrAlreadyMerged) {
		t.Fatalf("no-op merge outcome = %v", err)
	}
	if _, err := os.Stat(filepath.Join(localDir, ".lode", "save.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resolved no-op merge journal remains: %v", err)
	}
}

func TestLostAcceptedMergeReplyResumesOriginalSourceAfterSourceMoves(t *testing.T) {
	var lose atomic.Bool
	s := newWorkspaceServer(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/v1/save" && lose.Swap(false) {
				recorder := httptest.NewRecorder()
				next.ServeHTTP(recorder, r)
				if recorder.Code >= 200 && recorder.Code < 300 {
					http.Error(w, "lost accepted merge reply", http.StatusBadGateway)
					return
				}
				for key, values := range recorder.Header() {
					for _, value := range values {
						w.Header().Add(key, value)
					}
				}
				w.WriteHeader(recorder.Code)
				_, _ = w.Write(recorder.Body.Bytes())
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	rootDir := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(rootDir, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, rootDir, "main", []byte("root main"))
	write(t, rootDir, "topic", []byte("root topic"))
	rootWS := s.attach(t, rootDir)
	root := save(t, rootWS).ID
	closeWorkspace(t, rootWS)
	if err := s.p.CreateBranch(context.Background(), "topic", root); err != nil {
		t.Fatal(err)
	}
	topicDir := filepath.Join(t.TempDir(), "topic")
	topic, err := Clone(context.Background(), s.srv.URL, s.p.Token(), topicDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := topic.Switch(context.Background(), "topic", nil); err != nil {
		t.Fatal(err)
	}
	write(t, topicDir, "topic", []byte("first topic"))
	firstTopic := save(t, topic).ID
	mainDir := filepath.Join(t.TempDir(), "main")
	main, err := Clone(context.Background(), s.srv.URL, s.p.Token(), mainDir, model.Selection{{Path: "main", Include: true}})
	if err != nil {
		t.Fatal(err)
	}
	write(t, rootDir, "main", []byte("main advanced"))
	mainHead := publishWorkspace(t, rootDir)
	if err := main.Update(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	dirty := []byte("unsaved main edit")
	write(t, mainDir, "main", dirty)
	base := main.State().Base
	lose.Store(true)
	if _, err := main.Merge(context.Background(), "topic", "tester", "merge with lost reply", "", nil); err == nil {
		t.Fatal("expected lost accepted merge reply")
	}
	if _, err := os.Stat(filepath.Join(mainDir, ".lode", "save.json")); err != nil {
		t.Fatalf("merge intent missing: %v", err)
	}
	write(t, topicDir, "topic", []byte("second topic"))
	secondTopic := save(t, topic).ID
	if firstTopic == secondTopic {
		t.Fatal("topic did not advance")
	}
	if err := main.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	history, err := s.p.History(context.Background(), "main", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || len(history[0].Parents) != 2 || history[0].Parents[0] != mainHead || history[0].Parents[1] != firstTopic {
		t.Fatalf("resumed merge history = %#v", history)
	}
	if main.State().Base != base {
		t.Fatalf("merge resume changed workspace base: %q -> %q", base, main.State().Base)
	}
	if got := read(t, mainDir, "main"); !bytes.Equal(got, dirty) {
		t.Fatalf("merge resume changed local file: %q", got)
	}
	if got := string(readProjectFile(t, s.p, history[0].ID, "topic")); got != "first topic" {
		t.Fatalf("resumed source contents = %q", got)
	}
	closeWorkspace(t, topic)
	closeWorkspace(t, main)
}

func TestRestoreSelectedHistoricalPathKeepsCurrentBase(t *testing.T) {
	_, sourceDir, localDir, w := makeTransitionPair(t, map[string][]byte{"a": []byte("old a"), "b": []byte("old b")}, model.Selection{{Include: true}, {Path: "b", Include: false}})
	historical := w.State().Base
	write(t, sourceDir, "a", []byte("new a"))
	current := publishWorkspace(t, sourceDir)
	if err := w.Update(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	base := w.State().Base
	if err := w.Restore(context.Background(), historical, model.Selection{{Path: "a", Include: true}}, false); err != nil {
		t.Fatal(err)
	}
	if got := string(read(t, localDir, "a")); got != "old a" {
		t.Fatalf("restored a = %q", got)
	}
	if w.State().Base != base || base != current {
		t.Fatalf("restore changed base: got %q, current version %q", w.State().Base, current)
	}
	changes, err := w.Status(context.Background())
	if err != nil || len(changes) != 1 || changes[0].Path != "a" {
		t.Fatalf("restore status = %#v, %v", changes, err)
	}
	if _, err := os.Stat(filepath.Join(localDir, "b")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restore materialized unselected b: %v", err)
	}
}

func TestRestoreDirtyRefusesUnlessForced(t *testing.T) {
	_, _, localDir, w := makeTransitionPair(t, map[string][]byte{"a": []byte("base")}, nil)
	historical := w.State().Base
	dirty := []byte("local dirty")
	write(t, localDir, "a", dirty)
	if err := w.Restore(context.Background(), historical, nil, false); err == nil {
		t.Fatal("restore discarded dirty file without force")
	}
	if got := read(t, localDir, "a"); !bytes.Equal(got, dirty) {
		t.Fatalf("refused restore changed file: %q", got)
	}
	if err := w.Restore(context.Background(), historical, nil, true); err != nil {
		t.Fatal(err)
	}
	if got := string(read(t, localDir, "a")); got != "base" {
		t.Fatalf("forced restore = %q", got)
	}
	if w.State().Base != historical {
		t.Fatal("restore changed baseline")
	}
}

func TestRestoreResumeFetchesOriginalHistoricalSource(t *testing.T) {
	var failB atomic.Bool
	s := newWorkspaceServer(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/v1/read" && r.URL.Query().Get("path") == "b" && failB.Swap(false) {
				http.Error(w, "injected", http.StatusBadGateway)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	sourceDir := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(sourceDir, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, sourceDir, "a", []byte("historical a"))
	write(t, sourceDir, "b", []byte("historical b"))
	source := s.attach(t, sourceDir)
	historical := save(t, source).ID
	closeWorkspace(t, source)
	localDir := filepath.Join(t.TempDir(), "local")
	w, err := Clone(context.Background(), s.srv.URL, s.p.Token(), localDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	write(t, sourceDir, "a", []byte("current a"))
	write(t, sourceDir, "b", []byte("current b"))
	current := publishWorkspace(t, sourceDir)
	if err := w.Update(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	base := w.State().Base
	failB.Store(true)
	if err := w.Restore(context.Background(), historical, nil, true); err == nil {
		t.Fatal("expected interrupted restore")
	}
	if got := string(read(t, localDir, "a")); got != "historical a" {
		t.Fatalf("first restore action = %q", got)
	}
	plan := transitionJournal(t, localDir)
	if plan.Source != historical {
		t.Fatalf("journal source = %q, want historical %q", plan.Source, historical)
	}
	closeWorkspace(t, w)
	w, err = Open(localDir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWorkspace(t, w)
	if err := w.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := string(read(t, localDir, "a")); got != "historical a" {
		t.Fatalf("resumed a = %q", got)
	}
	if got := string(read(t, localDir, "b")); got != "historical b" {
		t.Fatalf("resumed b = %q", got)
	}
	if w.State().Base != base || base != current {
		t.Fatalf("resumed restore changed base: %q", w.State().Base)
	}
}

func TestRestoreCancelPreservesPreRestoreBytes(t *testing.T) {
	var failB atomic.Bool
	s := newWorkspaceServer(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/v1/read" && r.URL.Query().Get("path") == "b" && failB.Swap(false) {
				http.Error(w, "injected", http.StatusBadGateway)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	sourceDir := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(sourceDir, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, sourceDir, "a", []byte("before a"))
	write(t, sourceDir, "b", []byte("before b"))
	source := s.attach(t, sourceDir)
	historical := save(t, source).ID
	closeWorkspace(t, source)
	localDir := filepath.Join(t.TempDir(), "local")
	w, err := Clone(context.Background(), s.srv.URL, s.p.Token(), localDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeWorkspace(t, w)
	write(t, sourceDir, "a", []byte("later a"))
	write(t, sourceDir, "b", []byte("later b"))
	publishWorkspace(t, sourceDir)
	if err := w.Update(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	beforeA, beforeB := read(t, localDir, "a"), read(t, localDir, "b")
	failB.Store(true)
	if err := w.Restore(context.Background(), historical, nil, true); err == nil {
		t.Fatal("expected interrupted restore")
	}
	if err := w.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := read(t, localDir, "a"); !bytes.Equal(got, beforeA) {
		t.Fatalf("cancelled restore a = %q", got)
	}
	if got := read(t, localDir, "b"); !bytes.Equal(got, beforeB) {
		t.Fatalf("cancelled restore b = %q", got)
	}
}
