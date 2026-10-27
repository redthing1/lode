package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/transport"
)

// BenchmarkWorkspaceCost is an opt-in real-service cost harness. Run with
// `go test ./internal/workspace -run '^$' -bench '^BenchmarkWorkspaceCost$' -benchtime=1x`.
// Setup is excluded from timing. strace records actual local file reads and
// state- and progress-temp writes from the isolated workspace child; HTTP
// values count body bytes at a local reverse proxy, not headers or TCP framing.
func BenchmarkWorkspaceCost(b *testing.B) {
	if runtime.GOOS != "linux" {
		b.Skip("the local workspace and strace cost harness currently requires Linux")
	}
	strace, err := exec.LookPath("strace")
	if err != nil {
		b.Skip("strace is unavailable")
	}
	for _, count := range []int{100, 1000} {
		for _, selection := range []string{"full", "one"} {
			name := fmt.Sprintf("tree-%d/selection-%s", count, selection)
			b.Run(name, func(b *testing.B) {
				var initialHTTP, initialFiles, updateHTTP, updateFiles uint64
				var localReads, stateWrites, progressWrites, stateDerived, progressDerived, combinedDerived, finalState, actions, moves, links, operationNanos, childWallNanos uint64
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					fx := newCostFixture(b, count, selection, false)
					initialHTTP += sumWire(fx.initialWire)
					initialFiles += fx.initialWire["/v1/file"].Response
					if err := fx.advanceAll(); err != nil {
						b.Fatal(err)
					}
					before := fx.counter.snapshot()
					tracePath := costTraceFile(b, fmt.Sprintf("update-%d-%s-", count, selection), "update.strace")
					metricsPath := filepath.Join(b.TempDir(), "child.json")
					b.StartTimer()
					started := time.Now()
					output, err := runCostChild(strace, fx.workspaceDir, "update", tracePath, metricsPath)
					wallNanos := uint64(time.Since(started).Nanoseconds())
					b.StopTimer()
					if err != nil {
						b.Fatalf("workspace child update failed: %v\n%s", err, output)
					}
					after := fx.counter.snapshot()
					updateWire := wireDelta(before, after)
					updateHTTP += sumWire(updateWire)
					updateFiles += updateWire["/v1/file"].Response
					child, err := readCostChild(metricsPath)
					if err != nil {
						b.Fatal(err)
					}
					readBytes, writeBytes, progressBytes := parseCostTrace(tracePath, fx.workspaceDir)
					localReads += readBytes
					stateWrites += writeBytes
					progressWrites += progressBytes
					stateDerived += child.DerivedPlanStateWriteBytes
					progressDerived += child.DerivedProgressWriteBytes
					combinedDerived += child.DerivedPlanStateWriteBytes + child.DerivedProgressWriteBytes
					finalState += child.FinalStateBytes
					actions += uint64(child.ActionCount)
					moves += uint64(child.MoveCount)
					links += uint64(child.LinkCount)
					operationNanos += child.OperationNanos
					if err := fx.close(); err != nil {
						b.Fatal(err)
					}
					childWallNanos += wallNanos
				}
				if b.N > 0 {
					b.ReportMetric(float64(initialHTTP)/float64(b.N), "initial-http-body-bytes/op")
					b.ReportMetric(float64(initialFiles)/float64(b.N), "initial-file-body-bytes/op")
					b.ReportMetric(float64(updateHTTP)/float64(b.N), "update-http-body-bytes/op")
					b.ReportMetric(float64(updateFiles)/float64(b.N), "update-file-body-bytes/op")
					b.ReportMetric(float64(localReads)/float64(b.N), "local-file-read-bytes/op")
					b.ReportMetric(float64(stateWrites)/float64(b.N), "state-write-bytes/op")
					b.ReportMetric(float64(progressWrites)/float64(b.N), "progress-write-bytes/op")
					b.ReportMetric(float64(stateWrites+progressWrites)/float64(b.N), "metadata-temp-write-bytes/op")
					b.ReportMetric(float64(stateDerived)/float64(b.N), "state-derived-bytes/op")
					b.ReportMetric(float64(progressDerived)/float64(b.N), "progress-derived-bytes/op")
					b.ReportMetric(float64(combinedDerived)/float64(b.N), "metadata-derived-bytes/op")
					b.ReportMetric(float64(int64(stateDerived)-int64(stateWrites))/float64(b.N), "state-derived-minus-observed-bytes/op")
					b.ReportMetric(float64(int64(progressDerived)-int64(progressWrites))/float64(b.N), "progress-derived-minus-observed-bytes/op")
					b.ReportMetric(float64(int64(combinedDerived)-int64(stateWrites+progressWrites))/float64(b.N), "metadata-derived-minus-observed-bytes/op")
					b.ReportMetric(float64(finalState)/float64(b.N), "state-final-bytes/op")
					b.ReportMetric(float64(actions)/float64(b.N), "file-actions/op")
					b.ReportMetric(float64(moves)/float64(b.N), "move-hooks/op")
					b.ReportMetric(float64(links)/float64(b.N), "link-hooks/op")
					b.ReportMetric(float64(operationNanos)/float64(b.N)/1e6, "update-operation-ms/op")
					b.ReportMetric(float64(childWallNanos)/float64(b.N)/1e6, "strace-child-wall-ms/op")
				}
			})
		}
	}
	b.Run("one-edit-in-8MiB-file", func(b *testing.B) {
		var requestBytes, sourceReadBytes, stateWrites, progressWrites, operationNanos, childWallNanos uint64
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			fx := newCostFixture(b, 1, "full", true)
			if err := fx.editLargeFile(); err != nil {
				b.Fatal(err)
			}
			before := fx.counter.snapshot()
			tracePath := costTraceFile(b, "commit-8mib-", "commit.strace")
			metricsPath := filepath.Join(b.TempDir(), "child.json")
			b.StartTimer()
			started := time.Now()
			output, err := runCostChild(strace, fx.workspaceDir, "commit", tracePath, metricsPath)
			wallNanos := uint64(time.Since(started).Nanoseconds())
			b.StopTimer()
			if err != nil {
				b.Fatalf("workspace child commit failed: %v\n%s", err, output)
			}
			after := fx.counter.snapshot()
			requestBytes += wireDelta(before, after)["/v1/changes"].Request
			child, err := readCostChild(metricsPath)
			if err != nil {
				b.Fatal(err)
			}
			readBytes, writeBytes, progressBytes := parseCostTrace(tracePath, fx.workspaceDir)
			sourceReadBytes += readBytes
			stateWrites += writeBytes
			progressWrites += progressBytes
			operationNanos += child.OperationNanos
			childWallNanos += wallNanos
			if err := fx.close(); err != nil {
				b.Fatal(err)
			}
		}
		if b.N > 0 {
			b.ReportMetric(float64(requestBytes)/float64(b.N), "changes-request-body-bytes/op")
			b.ReportMetric(float64(sourceReadBytes)/float64(b.N), "local-file-read-bytes/op")
			b.ReportMetric(float64(stateWrites)/float64(b.N), "state-write-bytes/op")
			b.ReportMetric(float64(progressWrites)/float64(b.N), "progress-write-bytes/op")
			b.ReportMetric(float64(stateWrites+progressWrites)/float64(b.N), "metadata-temp-write-bytes/op")
			b.ReportMetric(float64(operationNanos)/float64(b.N)/1e6, "commit-operation-ms/op")
			b.ReportMetric(float64(childWallNanos)/float64(b.N)/1e6, "strace-child-wall-ms/op")
		}
	})
}

type costFixture struct {
	local        *project.Local
	upstream     *httptest.Server
	proxy        *httptest.Server
	counter      *bodyCounter
	workspaceDir string
	initialWire  map[string]bodyCounts
	files        map[string][]byte
	large        bool
	baseID       string
}

func newCostFixture(tb testing.TB, fileCount int, selected string, large bool) *costFixture {
	tb.Helper()
	ctx := context.Background()
	root := tb.TempDir()
	store := filepath.Join(root, "store")
	if err := project.InitializeLocal(ctx, store); err != nil {
		tb.Fatal(err)
	}
	local, err := project.OpenLocal(ctx, store)
	if err != nil {
		tb.Fatal(err)
	}
	handler, err := transport.NewHandler(local.Service, local.Token)
	if err != nil {
		_ = local.Close()
		tb.Fatal(err)
	}
	upstream := httptest.NewServer(handler)
	destination, err := url.Parse(upstream.URL)
	if err != nil {
		tb.Fatal(err)
	}
	counter := &bodyCounter{counts: make(map[string]bodyCounts)}
	proxy := httptest.NewServer(&countingProxy{counter: counter, next: httputil.NewSingleHostReverseProxy(destination)})
	workspaceDir := filepath.Join(root, "workspace")
	fx := &costFixture{local: local, upstream: upstream, proxy: proxy, counter: counter, workspaceDir: workspaceDir, large: large}
	if large {
		fx.files = map[string][]byte{"opaque.bin": bytes.Repeat([]byte("L"), 8<<20)}
		fileCount = 1
	} else {
		fx.files = make(map[string][]byte, fileCount)
		for i := 0; i < fileCount; i++ {
			p := fmt.Sprintf("file-%06d.bin", i)
			fx.files[p] = []byte(fmt.Sprintf("initial payload for %06d: opaque binary fixture bytes 0123456789abcdef\n", i))
		}
	}
	commit, err := fx.applyFiles("cost-base", "", "initial cost fixture", fx.files)
	if err != nil {
		tb.Fatal(err)
	}
	fx.baseID = commit.ID
	var rules []project.SelectionRule
	if selected == "one" {
		path := "file-000000.bin"
		if large {
			path = "opaque.bin"
		}
		rules = []project.SelectionRule{{Path: path, Included: true}}
	}
	options := Options{Directory: workspaceDir, Remote: proxy.URL, Token: local.Token, Branch: "main", Selection: rules}
	before := counter.snapshot()
	w, err := Create(ctx, options)
	if err != nil {
		tb.Fatal(err)
	}
	if w.Base() != commit.ID {
		tb.Fatalf("workspace base = %q, want %q", w.Base(), commit.ID)
	}
	if err := w.Close(); err != nil {
		tb.Fatal(err)
	}
	fx.initialWire = wireDelta(before, counter.snapshot())
	tb.Cleanup(func() { _ = fx.close() })
	return fx
}

func (f *costFixture) applyFiles(operation, base, message string, files map[string][]byte) (project.Commit, error) {
	changes := make([]project.Change, 0, len(files))
	for path, body := range files {
		digest := sha256.Sum256(body)
		changes = append(changes, project.Change{Path: path, Kind: model.File, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(body))})
	}
	commit, err := f.local.Service.Apply(context.Background(), project.ChangeRequest{OperationID: operation, Branch: "main", Base: base, Author: "cost-harness", Message: message, Changes: changes}, func(path model.Path) (io.ReadCloser, error) {
		body, ok := files[path.String()]
		if !ok {
			return nil, fmt.Errorf("missing cost payload %q", path)
		}
		return io.NopCloser(bytes.NewReader(body)), nil
	})
	return commit, err
}

func (f *costFixture) advanceAll() error {
	updates := make(map[string][]byte, len(f.files))
	for path, old := range f.files {
		body := append([]byte(nil), old...)
		if len(body) == 0 {
			body = []byte("x")
		} else {
			body[0] ^= 0x01
		}
		updates[path] = body
	}
	_, err := f.applyFiles("cost-target", f.baseID, "update all fixture files", updates)
	return err
}

func (f *costFixture) editLargeFile() error {
	path := filepath.Join(f.workspaceDir, "opaque.bin")
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	body[len(body)/2] ^= 0x01
	return os.WriteFile(path, body, 0644)
}

func (f *costFixture) close() error {
	f.proxy.Close()
	f.upstream.Close()
	return f.local.Close()
}

type costChildMetrics struct {
	OperationNanos             uint64
	ActionCount                int
	MoveCount                  int
	LinkCount                  int
	DerivedPlanStateWriteBytes uint64
	DerivedProgressWriteBytes  uint64
	FinalStateBytes            uint64
}

// TestWorkspaceCostChild is invoked as an isolated subprocess under strace by
// BenchmarkWorkspaceCost. Its no-op normal test invocation keeps the harness
// outside the default unit-test path.
func TestWorkspaceCostChild(t *testing.T) {
	if os.Getenv("LODE_COST_HELPER") != "1" {
		return
	}
	directory := os.Getenv("LODE_COST_WORKSPACE")
	metricsPath := os.Getenv("LODE_COST_METRICS")
	operation := os.Getenv("LODE_COST_OPERATION")
	w, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	metrics := costChildMetrics{}
	var initialTransition *state
	captureTransition := func() {
		if initialTransition != nil || w.state.Pending == nil || w.state.Pending.Transition == nil {
			return
		}
		cloned := w.state
		pendingCopy := *w.state.Pending
		transitionCopy := *w.state.Pending.Transition
		pendingCopy.Transition = &transitionCopy
		cloned.Pending = &pendingCopy
		initialTransition = &cloned
	}
	w.moveEntry = func(from, to string) error {
		captureTransition()
		metrics.MoveCount++
		return moveNoReplace(w.root, from, to)
	}
	w.linkEntry = func(from, to string) error {
		captureTransition()
		metrics.LinkCount++
		return linkNoReplace(w.root, from, to)
	}
	started := time.Now()
	switch operation {
	case "update":
		err = w.Update(context.Background())
	case "commit":
		_, err = w.Commit(context.Background(), "cost-harness", "one small file edit")
	default:
		err = fmt.Errorf("unknown cost operation %q", operation)
	}
	metrics.OperationNanos = uint64(time.Since(started).Nanoseconds())
	if err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	if initialTransition != nil {
		metrics.ActionCount = len(initialTransition.Pending.Transition.Actions)
		metrics.DerivedPlanStateWriteBytes = derivedTransitionStateWriteBytes(*initialTransition, w.state)
		metrics.DerivedProgressWriteBytes = derivedTransitionProgressWriteBytes(*initialTransition)
	}
	stateInfo, err := os.Stat(filepath.Join(directory, ".lode", "state.json"))
	if err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	metrics.FinalStateBytes = uint64(stateInfo.Size())
	closeErr := w.Close()
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	encoded, err := json.Marshal(metrics)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metricsPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCostTraceCountsSplitWorkspaceSyscalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.strace")
	workspaceDir := filepath.Join(t.TempDir(), "workspace")
	trace := strings.Join([]string{
		`write(3<` + workspaceDir + `/.lode/state-a.tmp>, "", 50) = 50`,
		`[pid 123] write(4<` + workspaceDir + `/.lode/state-b.tmp>, "", 100 <unfinished ...>`,
		`[pid 123] <... write resumed>) = 100`,
		`write(8<` + workspaceDir + `/.lode/progress-a.tmp>, "", 40) = 40`,
		`[pid 125] write(9<` + workspaceDir + `/.lode/progress-b.tmp>, "", 60 <unfinished ...>`,
		`[pid 125] <... write resumed>) = 60`,
		`126 write(10<` + workspaceDir + `/.lode/state-c.tmp>, "", 25 <unfinished ...>`,
		`126 <... write resumed>) = 25`,
		`127 write(11<` + workspaceDir + `/.lode/progress-c.tmp>, "", 15) = 15`,
		`read(5<` + workspaceDir + `/file.bin>, "", 300) = 300`,
		`128 read(6<` + workspaceDir + `/case-sensitive.bin>, "", 55) = 55`,
		`[pid 124] readv(6<` + workspaceDir + `/other.bin>, [], 2 <unfinished ...>`,
		`[pid 124] <... readv resumed>) = 200`,
		`read(7<` + workspaceDir + `/.lode/state.json>, "", 400) = 400`,
	}, "\n")
	if err := os.WriteFile(path, []byte(trace), 0600); err != nil {
		t.Fatal(err)
	}
	localRead, stateWrite, progressWrite := parseCostTrace(path, workspaceDir)
	if localRead != 555 || stateWrite != 175 || progressWrite != 115 {
		t.Fatalf("parseCostTrace() = reads %d, state writes %d, progress writes %d; want 555, 175, 115", localRead, stateWrite, progressWrite)
	}
}

func TestWorkspaceCostTraceAnalysis(t *testing.T) {
	directory := os.Getenv("LODE_COST_TRACE_ANALYSIS_DIR")
	if directory == "" {
		return
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".strace") {
			continue
		}
		tracePath := filepath.Join(directory, entry.Name())
		workspaceDir := costWorkspaceFromTrace(tracePath)
		if workspaceDir == "" {
			t.Errorf("cannot find workspace path in %s", entry.Name())
			continue
		}
		reads, stateWrites, progressWrites := parseCostTrace(tracePath, workspaceDir)
		t.Logf("%s local-file-read=%d state-temp-write=%d progress-temp-write=%d combined-temp-write=%d", entry.Name(), reads, stateWrites, progressWrites, stateWrites+progressWrites)
	}
}

func costWorkspaceFromTrace(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	match := regexp.MustCompile(`<([^>]+/workspace)/[^>]+>`).FindSubmatch(data)
	if len(match) != 2 {
		return ""
	}
	return string(match[1])
}

func derivedTransitionStateWriteBytes(initial, final state) uint64 {
	initialBytes, err := json.Marshal(initial)
	if err != nil {
		return 0
	}
	finalBytes, err := json.Marshal(final)
	if err != nil {
		return 0
	}
	return uint64(len(initialBytes) + len(finalBytes) + 2)
}

func derivedTransitionProgressWriteBytes(initial state) uint64 {
	if initial.Pending == nil || initial.Pending.Transition == nil {
		return 0
	}
	t := initial.Pending.Transition
	if t.ProgressVersion != 1 {
		return 0
	}
	plan, err := progressPlan(t)
	if err != nil {
		return 0
	}
	var total uint64
	for index := t.Index; index <= len(t.Actions); index++ {
		progress := transitionProgress{Version: 1, ID: t.ID, Plan: plan, Index: index}
		encoded, err := json.Marshal(progress)
		if err != nil {
			return 0
		}
		total += uint64(len(encoded) + 1)
	}
	return total
}

func cloneCostState(value state) (state, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return state{}, err
	}
	var cloned state
	err = json.Unmarshal(encoded, &cloned)
	return cloned, err
}

func runCostChild(strace, workspaceDir, operation, tracePath, metricsPath string) ([]byte, error) {
	command := exec.Command(strace, "-f", "-qq", "-s", "0", "-yy", "-e", "trace=read,readv,pread64,write,writev,pwrite64", "-o", tracePath, os.Args[0], "-test.run=^TestWorkspaceCostChild$")
	command.Env = append(os.Environ(),
		"LODE_COST_HELPER=1", "LODE_COST_WORKSPACE="+workspaceDir,
		"LODE_COST_OPERATION="+operation, "LODE_COST_METRICS="+metricsPath)
	return command.CombinedOutput()
}

func readCostChild(path string) (costChildMetrics, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return costChildMetrics{}, err
	}
	var metrics costChildMetrics
	if err := json.Unmarshal(data, &metrics); err != nil {
		return costChildMetrics{}, err
	}
	return metrics, nil
}

func costTraceFile(tb testing.TB, prefix, name string) string {
	tb.Helper()
	if directory := os.Getenv("LODE_COST_TRACE_DIR"); directory != "" {
		if err := os.MkdirAll(directory, 0700); err != nil {
			tb.Fatal(err)
		}
		file, err := os.CreateTemp(directory, prefix+"*.strace")
		if err != nil {
			tb.Fatal(err)
		}
		path := file.Name()
		if err := file.Close(); err != nil {
			tb.Fatal(err)
		}
		return path
	}
	return filepath.Join(tb.TempDir(), name)
}

var costSyscallResult = regexp.MustCompile(` = ([0-9]+)(?:$|\s)`)

func parseCostTrace(path, workspaceDir string) (localReadBytes, stateWriteBytes, progressWriteBytes uint64) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, 0
	}
	workspaceDir = filepath.Clean(workspaceDir)
	unfinished := make(map[string]costTraceCall)
	for _, line := range strings.Split(string(data), "\n") {
		pid, callText := costTracePID(line)
		if call, ok := unfinished[pid]; ok && costTraceResumed(callText, call.syscall) {
			delete(unfinished, pid)
			addCostTraceBytes(&localReadBytes, &stateWriteBytes, &progressWriteBytes, call, costTraceReturn(line))
			continue
		}
		call := costTraceCall{}
		switch name := costTraceName(callText); name {
		case "read", "readv", "pread64":
			call.syscall = name
			call.localRead = strings.Contains(line, "<"+workspaceDir+"/") && !strings.Contains(line, "<"+workspaceDir+"/.lode/")
		case "write", "writev", "pwrite64":
			call.syscall = name
			call.stateWrite = strings.Contains(line, "<"+workspaceDir+"/.lode/state-") && strings.Contains(line, ".tmp>")
			call.progressWrite = strings.Contains(line, "<"+workspaceDir+"/.lode/progress-") && strings.Contains(line, ".tmp>")
		default:
			continue
		}
		if strings.Contains(line, "<unfinished ...>") {
			unfinished[pid] = call
			continue
		}
		addCostTraceBytes(&localReadBytes, &stateWriteBytes, &progressWriteBytes, call, costTraceReturn(line))
	}
	return localReadBytes, stateWriteBytes, progressWriteBytes
}

type costTraceCall struct {
	syscall       string
	localRead     bool
	stateWrite    bool
	progressWrite bool
}

func costTracePID(line string) (string, string) {
	if strings.HasPrefix(line, "[pid ") {
		if end := strings.IndexByte(line, ']'); end > 5 {
			return line[5:end], strings.TrimSpace(line[end+1:])
		}
	}
	line = strings.TrimSpace(line)
	if space := strings.IndexByte(line, ' '); space > 0 {
		pid := line[:space]
		if _, err := strconv.ParseUint(pid, 10, 32); err == nil {
			return pid, strings.TrimSpace(line[space+1:])
		}
	}
	return "main", line
}

func costTraceName(line string) string {
	end := strings.IndexByte(line, '(')
	if end < 1 {
		return ""
	}
	return line[:end]
}

func costTraceResumed(line, name string) bool {
	return strings.Contains(line, "<... "+name+" resumed>")
}

func costTraceReturn(line string) uint64 {
	match := costSyscallResult.FindStringSubmatch(line)
	if len(match) != 2 {
		return 0
	}
	count, err := strconv.ParseUint(match[1], 10, 64)
	if err != nil {
		return 0
	}
	return count
}

func addCostTraceBytes(localRead, stateWrite, progressWrite *uint64, call costTraceCall, count uint64) {
	if call.localRead {
		*localRead += count
	}
	if call.stateWrite {
		*stateWrite += count
	}
	if call.progressWrite {
		*progressWrite += count
	}
}

type bodyCounts struct {
	Request  uint64
	Response uint64
}

type bodyCounter struct {
	mu     sync.Mutex
	counts map[string]bodyCounts
}

func (c *bodyCounter) add(path, direction string, count int) {
	if count <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	value := c.counts[path]
	if direction == "request" {
		value.Request += uint64(count)
	} else {
		value.Response += uint64(count)
	}
	c.counts[path] = value
}

func (c *bodyCounter) snapshot() map[string]bodyCounts {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make(map[string]bodyCounts, len(c.counts))
	for path, value := range c.counts {
		result[path] = value
	}
	return result
}

type countingProxy struct {
	counter *bodyCounter
	next    *httputil.ReverseProxy
}

func (p *countingProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if r.Body != nil {
		r.Body = &countedBody{ReadCloser: r.Body, count: func(n int) { p.counter.add(path, "request", n) }}
	}
	p.next.ServeHTTP(&countedResponseWriter{ResponseWriter: w, count: func(n int) { p.counter.add(path, "response", n) }}, r)
}

type countedBody struct {
	io.ReadCloser
	count func(int)
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.count(n)
	return n, err
}

type countedResponseWriter struct {
	http.ResponseWriter
	count func(int)
}

func (w *countedResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.count(n)
	return n, err
}

func (w *countedResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *countedResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func wireDelta(before, after map[string]bodyCounts) map[string]bodyCounts {
	result := make(map[string]bodyCounts)
	for path, value := range after {
		old := before[path]
		result[path] = bodyCounts{Request: value.Request - old.Request, Response: value.Response - old.Response}
	}
	return result
}

func sumWire(values map[string]bodyCounts) uint64 {
	var total uint64
	for _, value := range values {
		total += value.Request + value.Response
	}
	return total
}
