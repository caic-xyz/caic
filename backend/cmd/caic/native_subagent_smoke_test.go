// Native-subagent smoke: delivers each retained recording through a caic-managed md container.

// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

//go:build smoke

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/agent/agenttest"
	"github.com/caic-xyz/caic/backend/internal/agent/claudecode"
	"github.com/caic-xyz/caic/backend/internal/agent/codex"
	"github.com/caic-xyz/caic/backend/internal/agent/harness"
	"github.com/caic-xyz/caic/backend/internal/agent/opencode"
	"github.com/caic-xyz/caic/backend/internal/agent/pi"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/smoketest"
)

const (
	// smokeReplayHold keeps the replay agent paused long enough for caic to
	// restart between the recorded running card and the records that settle it.
	smokeReplayHold = 15 * time.Second
)

// smokeReplayRecordings lists every retained recording the smoke test delivers:
// the standardized delegation for each harness, plus Pi's workflow orchestration,
// which is the only recording that pins the batch scope.
var smokeReplayRecordings = []smokeReplayRecording{
	{harness.Claude, "native-subagent-joke-v3.jsonl"},
	{harness.Codex, "native-subagent-joke-v3.jsonl"},
	{harness.OpenCode, "native-subagent-joke-v3.jsonl"},
	{harness.Pi, "native-subagent-joke-v3.jsonl"},
	{harness.Pi, "native-subagent-workflow-v3.jsonl"},
}

// smokeReplayRecording is one retained recording to deliver through a container.
type smokeReplayRecording struct {
	harness harness.Name
	file    string
}

// name is the subtest name for this recording, for example "pi/workflow".
func (r smokeReplayRecording) name() string {
	return string(r.harness) + "/" + strings.TrimSuffix(strings.TrimPrefix(r.file, "native-subagent-"), "-v3.jsonl")
}

// smokeReplayEvidenceDir maps each harness to the package holding its recordings.
var smokeReplayEvidenceDir = map[harness.Name]string{
	harness.Claude:   "claudecode",
	harness.Codex:    "codex",
	harness.OpenCode: "opencode",
	harness.Pi:       "pi",
}

// smokeReplayCase is one retained recording, the name it is reported under, and
// the canonical contract the replay must reproduce.
type smokeReplayCase struct {
	name      string
	recording smoketest.ReplayRecording
	contract  agenttest.NativeSubagentContract
}

// recordedObservation is one native-subagent observation with the producer time
// its physical task-log record carried.
type recordedObservation struct {
	subagent   agent.NativeSubagent
	producerAt time.Time
}

// TestSmokeNativeSubagents proves the canonical native-subagent lifecycle inside
// a caic-managed md container for every supported harness and recording. Each task
// replays the retained recording through the production relay and that harness's
// wire, and caic restarts while the replay agent is paused between the recorded
// running card and the records that settle it, so the restored card must settle
// from records the container produced after caic stopped, without a duplicate.
//
// It never runs a live harness: the recordings are the evidence, so the test stays
// deterministic and needs no model credentials.
func TestSmokeNativeSubagents(t *testing.T) {
	// nativeSubagent is hand written, so a field added to either side is dropped
	// without a compile error and quietly weakens the contract below.
	if got, want := reflect.TypeFor[v1.EventNativeSubagent]().NumField(), reflect.TypeFor[agent.NativeSubagent]().NumField(); got != want {
		t.Fatalf("v1.EventNativeSubagent has %d fields, agent.NativeSubagent has %d; update nativeSubagent to map the new field", got, want)
	}
	cases := make(map[string]smokeReplayCase, len(smokeReplayRecordings))
	byHarness := map[harness.Name][]smoketest.ReplayRecording{}
	for _, rec := range smokeReplayRecordings {
		c := newSmokeReplayCase(t, rec.harness, rec.file)
		cases[rec.name()] = c
		byHarness[rec.harness] = append(byHarness[rec.harness], c.recording)
	}
	// One fixture for every recording: the container image and the server start
	// are shared, and each task's prompt selects its harness recording.
	backendSet := map[harness.Name]agent.Backend{}
	for h, recordings := range byHarness {
		backend, err := smoketest.NewReplayBackend(func() agent.WireFormat { return smokeReplayWire(h) }, recordings...)
		if err != nil {
			t.Fatalf("%s replay backend: %v", h, err)
		}
		backendSet[h] = backend
	}
	fixture := startServerFixture(t, serverFixture{backends: backendSet})
	for _, rec := range smokeReplayRecordings {
		t.Run(rec.name(), func(t *testing.T) {
			runSmokeReplay(t, fixture, rec.harness, cases[rec.name()])
		})
	}
}

// newSmokeReplayCase loads one retained recording into a replay recording.
func newSmokeReplayCase(t *testing.T, h harness.Name, file string) smokeReplayCase {
	t.Helper()
	newWire := func() agent.WireFormat { return smokeReplayWire(h) }
	path := filepath.Join("..", "..", "internal", "agent", smokeReplayEvidenceDir[h], "testdata", "evidence", file)
	evidence := agenttest.LoadNativeSubagentEvidence(t, path, agent.LogVersionV3, newWire)
	split := evidence.RunningSplit(t)
	if split >= len(evidence.Payloads) {
		t.Fatalf("%s leaves no payload after its running card, so a restart cannot be modeled", path)
	}
	return smokeReplayCase{
		name: file,
		recording: smoketest.ReplayRecording{
			Prompt:    agenttest.NativeSubagentEvidencePrompt(t, path, agent.LogVersionV3),
			Harness:   h,
			Payloads:  evidence.Payloads,
			HoldAfter: split,
			Hold:      smokeReplayHold,
		},
		contract: evidence.Contract(t),
	}
}

// runSmokeReplay creates one task, restarts caic mid-turn, and verifies that the
// replay delivered exactly the recorded lifecycle.
func runSmokeReplay(t *testing.T, fixture *smokeServer, h harness.Name, tc smokeReplayCase) {
	t.Helper()
	var repos []v1.Repo
	getJSON(t, fixture.baseURL, "/api/caic/v1/server/repos", &repos)
	if len(repos) == 0 {
		t.Fatal("no repository available for the replay task")
	}
	var created v1.Task
	postJSON(t, fixture.baseURL, "/api/caic/v1/tasks", v1.CreateTaskReq{
		InitialPrompt: v1.Prompt{Text: tc.recording.Prompt},
		Repos:         []v1.RepoSpec{{Name: repos[0].Path}},
		Harness:       v1.Harness(h),
		RuntimeName:   smoketest.SmokeRuntime(),
	}, &created)
	taskID := created.ID.String()
	if taskID == "" {
		t.Fatal("create response has empty task ID")
	}
	runtimeID := waitForTaskRuntime(t, fixture, taskID)
	if !strings.HasPrefix(string(runtimeID.InstanceID()), "md-") {
		t.Fatalf("task %s runtime = %q, want a caic-managed md container", taskID, runtimeID)
	}
	t.Logf("task %s: replaying %s %s into %s", taskID, h, tc.name, runtimeID)

	// The recording's running card must be visible before caic restarts.
	uiBefore := watchForRunningNativeAgent(t, fixture.baseURL, taskID)
	beforeCards, beforeActive := agenttest.FoldNativeSubagents(uiBefore)
	if len(beforeCards) != len(tc.contract.Cards) || beforeActive != len(beforeCards) {
		t.Fatalf("task %s: pre-restart cards = %#v active = %d, want %d running recorded agents", taskID, beforeCards, beforeActive, len(tc.contract.Cards))
	}
	for i := range beforeCards {
		if beforeCards[i].Status != agent.NativeSubagentStatusRunning || !strings.HasPrefix(beforeCards[i].ID, tc.contract.Cards[i].Identity+":") {
			t.Fatalf("task %s: pre-restart card %d = %#v, want the fixture identity %q still running", taskID, i, beforeCards[i], tc.contract.Cards[i].Identity)
		}
	}

	downtimeStart := time.Now().UTC()
	fixture.stop()
	fixture.start()
	downtimeEnd := time.Now().UTC()
	if !downtimeEnd.After(downtimeStart) {
		t.Fatalf("task %s: caic downtime window is inverted", taskID)
	}

	task := waitForTaskState(t, fixture, taskID, "waiting")
	if task.Error != "" {
		t.Fatalf("task %s: turn failed: %s", taskID, task.Error)
	}
	if task.NumTurns != 1 {
		t.Fatalf("task %s: NumTurns = %d, want 1", taskID, task.NumTurns)
	}

	// The durable recording: replay the task log through the same wire the task
	// used, exactly like restart and settled-history replay do.
	records := replayTaskLog(t, fixture.cfg.Dirs.CacheDir, taskID, smoketest.NewReplayWire(smokeReplayWire(h)))
	recorded := subagentsOf(records)
	if got := agenttest.Contract(recorded); !got.Equal(tc.contract) {
		t.Fatalf("task %s: replayed contract = %s, want the retained recording's %s", taskID, got, tc.contract)
	}
	cards, active := agenttest.FoldNativeSubagents(recorded)
	if len(cards) != len(tc.contract.Cards) || active != tc.contract.Active {
		t.Fatalf("task %s: replayed cards = %#v active = %d, want %d cards with %d active", taskID, cards, active, len(tc.contract.Cards), tc.contract.Active)
	}
	if cards[0].ID != beforeCards[0].ID {
		t.Fatalf("task %s: restored card ID = %q, want the pre-restart identity %q", taskID, cards[0].ID, beforeCards[0].ID)
	}
	verifySmokeSettlement(t, taskID, records, downtimeStart, downtimeEnd)

	// The task UI the frontend consumes must agree with the delivered records.
	uiAfter := readTaskHistory(t, fixture.baseURL, taskID)
	if got := agenttest.Contract(uiAfter); !got.Equal(tc.contract) {
		t.Fatalf("task %s: UI contract = %s, want the retained recording's %s", taskID, got, tc.contract)
	}
	if diff := compareCards(uiAfter, recorded); diff != "" {
		t.Fatalf("task %s: task UI disagrees with the recording: %s", taskID, diff)
	}
	t.Logf("task %s: %s reproduced %s; the restored card settled from a record produced after %s",
		taskID, h, agenttest.Contract(recorded), downtimeStart.Format(time.RFC3339))

	stopAndPurgeTask(t, fixture, taskID)
}

// verifySmokeSettlement checks that the running card came from pre-restart
// history and that the harness records that settled it were produced after caic
// stopped. A settling record may land while caic is still down, in which case
// adoption imports it from the relay tail, or on the live tail after adoption;
// both are records the container produced without caic watching, so the count of
// each is reported rather than asserted.
func verifySmokeSettlement(t *testing.T, taskID string, records []recordedObservation, downtimeStart, downtimeEnd time.Time) {
	t.Helper()
	var runningBefore, settledAfter, duringDowntime int
	for i := range records {
		switch {
		case records[i].subagent.Status == agent.NativeSubagentStatusRunning && records[i].producerAt.Before(downtimeStart):
			runningBefore++
		case records[i].subagent.Status.Terminal() && records[i].producerAt.After(downtimeStart):
			settledAfter++
			if records[i].producerAt.Before(downtimeEnd) {
				duringDowntime++
			}
		}
	}
	if runningBefore == 0 {
		t.Fatalf("task %s: no recorded running card preceded the restart", taskID)
	}
	if settledAfter == 0 {
		t.Fatalf("task %s: no record produced after caic stopped at %s settled the card: %#v", taskID, downtimeStart.Format(time.RFC3339), records)
	}
	t.Logf("task %s: caic was down %s-%s; %d running record(s) before, %d settling record(s) after (%d in the downtime window)",
		taskID, downtimeStart.Format(time.RFC3339), downtimeEnd.Format(time.RFC3339), runningBefore, settledAfter, duringDowntime)
}

// stopAndPurgeTask stops and purges one replayed task so the next harness starts
// from a clean task list.
func stopAndPurgeTask(t *testing.T, fixture *smokeServer, taskID string) {
	t.Helper()
	postJSON(t, fixture.baseURL, "/api/caic/v1/tasks/"+taskID+"/stop", nil, nil)
	waitForTaskState(t, fixture, taskID, "stopped")
	postJSON(t, fixture.baseURL, "/api/caic/v1/tasks/"+taskID+"/purge", nil, nil)
	waitForTaskState(t, fixture, taskID, "purged")
	t.Logf("task %s: stopped and purged", taskID)
}

// watchForRunningNativeAgent waits until the canonical task event stream shows a
// running native card and returns the observations behind that view.
func watchForRunningNativeAgent(t *testing.T, baseURL, taskID string) []agent.NativeSubagent {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), smokeTaskTimeout)
	defer cancel()
	observations, err := readTaskEvents(ctx, baseURL, taskID, func(ready bool, observations []agent.NativeSubagent) bool {
		if !ready {
			// History is still replaying; the view is not yet authoritative.
			return false
		}
		_, active := agenttest.FoldNativeSubagents(observations)
		return active > 0
	})
	if err != nil {
		t.Fatalf("task %s: waiting for a running native card: %v\nobservations: %#v", taskID, err, observations)
	}
	return observations
}

// readTaskHistory returns the observations a task's event stream replays before
// it announces ready, which is the history the task UI paints.
func readTaskHistory(t *testing.T, baseURL, taskID string) []agent.NativeSubagent {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	observations, err := readTaskEvents(ctx, baseURL, taskID, func(ready bool, _ []agent.NativeSubagent) bool {
		return ready
	})
	if err != nil {
		t.Fatalf("task %s: reading replayed history: %v", taskID, err)
	}
	return observations
}

// readTaskEvents reads one task's canonical event stream, collecting the native
// observations it carries, until stop reports it may stop. ready reports that
// the stream finished replaying history, so nothing before it is a live view.
func readTaskEvents(ctx context.Context, baseURL, taskID string, stop func(ready bool, observations []agent.NativeSubagent) bool) ([]agent.NativeSubagent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/caic/v1/tasks/"+taskID+"/events", http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("task events status %d: %s", resp.StatusCode, body)
	}
	var observations []agent.NativeSubagent
	ready := false
	event := ""
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			payload := strings.TrimPrefix(line, "data: ")
			switch event {
			case "ready":
				ready = true
			case "message":
				var ev v1.EventMessage
				if err := json.Unmarshal([]byte(payload), &ev); err != nil {
					return observations, fmt.Errorf("decode task event: %w", err)
				}
				if ev.NativeSubagent != nil {
					observations = append(observations, nativeSubagent(ev.NativeSubagent))
				}
			}
		case line == "":
			event = ""
		}
		if stop(ready, observations) {
			return observations, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return observations, err
	}
	return observations, fmt.Errorf("event stream ended with %d native observations", len(observations))
}

// replayTaskLog replays one task log through the given wire, exactly like
// restart and settled-history replay do, and returns the native-subagent
// observations with the producer times the log recorded.
func replayTaskLog(t *testing.T, cacheDir, taskID string, wire agent.WireFormat) []recordedObservation {
	t.Helper()
	path := findTaskLog(t, cacheDir, taskID)
	reader := openTaskLog(t, path)
	defer func() {
		if err := reader.Close(); err != nil {
			t.Errorf("close %s: %v", path, err)
		}
	}()
	parser, err := agent.NewLogRecordParser(agent.LogVersionV3, wire.ParseMessage)
	if err != nil {
		t.Fatal(err)
	}
	var out []recordedObservation
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 32<<20)
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		record, err := parser.ParseRecord(scanner.Bytes())
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, message := range record.Messages {
			if subagent, ok := message.Message.(*agent.NativeSubagentMessage); ok {
				out = append(out, recordedObservation{subagent: subagent.Subagent, producerAt: message.ProducerTime})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(out) == 0 {
		t.Fatalf("task %s recorded no native subagent activity in %s", taskID, path)
	}
	return out
}

// findTaskLog returns the task log path for taskID, preferring the plain log an
// active task keeps.
func findTaskLog(t *testing.T, cacheDir, taskID string) string {
	t.Helper()
	dir := filepath.Join(cacheDir, "tasks")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read task logs: %v", err)
	}
	plain, compressed := "", ""
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, taskID+"-") {
			continue
		}
		switch {
		case strings.HasSuffix(name, ".jsonl"):
			plain = filepath.Join(dir, name)
		case strings.HasSuffix(name, ".jsonl.zst"):
			compressed = filepath.Join(dir, name)
		}
	}
	if plain != "" {
		return plain
	}
	if compressed != "" {
		return compressed
	}
	t.Fatalf("task %s has no task log in %s", taskID, dir)
	return ""
}

// openTaskLog opens a plain or zstd-compressed task log for reading.
func openTaskLog(t *testing.T, path string) io.ReadCloser {
	t.Helper()
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, ".zst") {
		return file
	}
	decoder, err := zstd.NewReader(file)
	if err != nil {
		_ = file.Close()
		t.Fatalf("open %s: %v", path, err)
	}
	return &zstdTaskLog{decoder: decoder, file: file}
}

// zstdTaskLog reads a compressed task log.
type zstdTaskLog struct {
	decoder *zstd.Decoder
	file    *os.File
}

func (r *zstdTaskLog) Read(p []byte) (int, error) { return r.decoder.Read(p) }

func (r *zstdTaskLog) Close() error {
	r.decoder.Close()
	return r.file.Close()
}

// compareCards returns a description of the first difference between two native
// observation streams folded into cards, or "" when they are identical.
func compareCards(ui, recorded []agent.NativeSubagent) string {
	uiCards, uiActive := agenttest.FoldNativeSubagents(ui)
	recordedCards, recordedActive := agenttest.FoldNativeSubagents(recorded)
	if len(uiCards) != len(recordedCards) || uiActive != recordedActive {
		return fmt.Sprintf("UI has %d cards (%d active), recording has %d cards (%d active)",
			len(uiCards), uiActive, len(recordedCards), recordedActive)
	}
	for i := range uiCards {
		if uiCards[i] != recordedCards[i] {
			return fmt.Sprintf("UI card %d = %#v, recording card %d = %#v", i, uiCards[i], i, recordedCards[i])
		}
	}
	return ""
}

// smokeReplayWire returns a fresh wire format for one harness. These are the
// production wires, so the recorded native records travel through the same
// adapter a live session uses.
func smokeReplayWire(h harness.Name) agent.WireFormat {
	switch h {
	case harness.Claude:
		return claudecode.New().NewWire()
	case harness.Codex:
		return codex.New("", nil).NewWire()
	case harness.OpenCode:
		return opencode.New("", nil).NewWire()
	case harness.Pi:
		return pi.New("", nil).NewWire()
	default:
		panic("no replay wire for harness " + string(h))
	}
}

// nativeSubagent converts the canonical event DTO back to its wire-independent
// form. It maps every DTO field: a dropped field reads as a zero value and
// silently weakens the UI contract this test pins.
func nativeSubagent(e *v1.EventNativeSubagent) agent.NativeSubagent {
	return agent.NativeSubagent{
		ID:         e.ID,
		ToolUseID:  e.ToolUseID,
		Scope:      agent.NativeSubagentScope(e.Scope),
		GroupID:    e.GroupID,
		Label:      e.Label,
		Prompt:     e.Prompt,
		Status:     agent.NativeSubagentStatus(e.Status),
		Result:     e.Result,
		Background: e.Background,
	}
}

// subagentsOf drops the producer times from recorded observations.
func subagentsOf(records []recordedObservation) []agent.NativeSubagent {
	out := make([]agent.NativeSubagent, 0, len(records))
	for i := range records {
		out = append(out, records[i].subagent)
	}
	return out
}
