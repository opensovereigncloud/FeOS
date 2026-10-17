// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// goldenSpecs is the baseline as the clients know it: names in order,
// descriptions and schemas byte for byte.
var goldenSpecs = []struct{ name, description, schema string }{
	{"bash", "Execute a bash command and return its output with exit code: for building, testing, git, package managers and running programs. Do not read, search or change files with it — read, rg, fd, edit and write do that, and their answers are made for you. Output is truncated to 2000 lines / 50KB.",
		`{"properties":{"command":{"description":"Shell command to execute","type":"string"},"timeout":{"description":"Timeout in seconds (default 120); give more for a long build or test run","type":"integer"}},"required":["command"],"type":"object"}`},
	{"read", "Read a file. Use this instead of cat, head, tail or sed -n in bash. Text comes back numbered like cat -n: each line starts with its number and a tab, which are not part of the file — leave them out of an edit. Up to 2000 lines or 50KB per call; when the file goes on, the answer ends with the offset to continue from. Use offset (1-based)/limit to read a part of a large file. Images (png, jpg, gif, webp — e.g. a screenshot) are returned as an image you can look at, up to 3.75 MB and 8000 px per side.",
		`{"properties":{"limit":{"description":"Maximum number of lines to read","type":"integer"},"offset":{"description":"Line number to start reading from (1-indexed)","type":"integer"},"path":{"description":"Path to the file to read (relative or absolute)","type":"string"}},"required":["path"],"type":"object"}`},
	{"write", "Write a whole file: creates it and its parent directories, or replaces it if it exists. Use this instead of cat <<EOF, echo > or tee in bash. To change part of an existing file, use edit.",
		`{"properties":{"content":{"description":"Content to write to the file","type":"string"},"path":{"description":"Path to the file to write (relative or absolute)","type":"string"}},"required":["path","content"],"type":"object"}`},
	{"edit", "Change an existing file by exact text replacement. Use this for every change to a file, never sed -i, perl -pi, python or a heredoc in bash. Each entry of edits replaces old with new; old must match the file exactly, whitespace and indentation included, and occur exactly once — take in a line or two around it to make it unique. List several places of one file in one call: they apply to the file as it was and must not overlap. For several files, call edit once per file; several calls in one answer are fine.",
		`{"properties":{"edits":{"description":"Targeted exact-match replacements","items":{"properties":{"new":{"description":"Replacement text for this targeted edit","type":"string"},"old":{"description":"Exact text to replace, as it is in the file (without read's line numbers); must occur exactly once","type":"string"}},"required":["old","new"],"type":"object"},"type":"array"},"path":{"description":"Path to the file to edit (relative or absolute)","type":"string"}},"required":["path","edits"],"type":"object"}`},
	{"ls", "List directory contents. Entries sorted alphabetically, '/' suffix for directories, includes dotfiles. Default limit 500.",
		`{"properties":{"limit":{"description":"Maximum number of entries to return (default: 500)","type":"integer"},"path":{"description":"Directory to list (default: current directory)","type":"string"}},"type":"object"}`},
	{"rg", "Search file contents with ripgrep syntax. Use this instead of grep or rg in bash. Pattern is a regex (Rust regex, like rg). Returns matching lines with file paths and line numbers. Respects .gitignore, skips hidden and binary files. Truncated to 100 match lines (default) or 50KB.",
		`{"properties":{"context":{"description":"Lines of context around each match (rg -C)","type":"integer"},"glob":{"description":"Filter files, e.g. '*.py' (rg -g)","type":"string"},"ignoreCase":{"description":"Case-insensitive search (rg -i)","type":"boolean"},"limit":{"description":"Maximum number of match lines (default: 100)","type":"integer"},"literal":{"description":"Treat pattern as literal string (rg -F)","type":"boolean"},"path":{"description":"Directory or file to search (default: current directory)","type":"string"},"pattern":{"description":"Search pattern (regex, ripgrep syntax)","type":"string"}},"required":["pattern"],"type":"object"}`},
	{"fd", "Find files by name with fd syntax. Use this instead of find or ls -R in bash. Pattern is a regex matched against file names, smart-case (all-lowercase = insensitive); use glob=true for glob patterns like '*.go'. Returns matching file paths. Respects .gitignore, skips hidden files. Truncated to 1000 results (default).",
		`{"properties":{"glob":{"description":"Treat pattern as glob, e.g. '*.go' (fd --glob)","type":"boolean"},"limit":{"description":"Maximum number of results (default: 1000)","type":"integer"},"path":{"description":"Directory to search in (default: current directory)","type":"string"},"pattern":{"description":"Pattern to match file names (fd regex, smart case)","type":"string"}},"required":["pattern"],"type":"object"}`},
	{"process_start", "Start a long-running process (a dev server, a watcher, a queue worker) that keeps running after this call returns, and get a handle for it. Use this instead of backgrounding a command inside bash: only a process started here can be listed, read and stopped later. Output goes to a log file, not to this answer. timeout bounds how long the process may run — the sandbox stays awake for that long, so pick what the work needs; 0 means no limit and a sandbox that never falls asleep on its own.",
		`{"properties":{"command":{"description":"Shell command to run in the background","type":"string"},"cwd":{"description":"Working directory (default: current directory)","type":"string"},"name":{"description":"Short name shown in process_list, e.g. 'dev server'","type":"string"},"timeout":{"description":"Seconds the process may run; 0 = no limit","type":"integer"}},"required":["command","timeout"],"type":"object"}`},
	{"process_list", "List the background processes of this sandbox: id, name, whether they are still running, how long they may still run, and the command behind each.",
		`{"properties":{},"type":"object"}`},
	{"process_logs", "Read the output a background process has produced so far (stdout and stderr together, newest lines last).",
		`{"properties":{"id":{"description":"Process id from process_start or process_list","type":"string"},"lines":{"description":"How many lines from the end (default: 100)","type":"integer"}},"required":["id"],"type":"object"}`},
	{"process_stop", "Stop a background process: SIGTERM to its process group, SIGKILL if it is still there a moment later.",
		`{"properties":{"id":{"description":"Process id from process_start or process_list","type":"string"}},"required":["id"],"type":"object"}`},
	{"webfetch", "Fetch a web page (http/https) and return its main readable content as text. Boilerplate (navigation, ads, footers) is stripped; links are kept. Use for reading documentation, articles and pages the agent needs to research.",
		`{"properties":{"url":{"description":"URL to fetch (http/https)","type":"string"}},"required":["url"],"type":"object"}`},
}

func TestSpecsGolden(t *testing.T) {
	specs := Specs()
	if len(specs) != len(goldenSpecs) {
		t.Fatalf("%d tools, want %d", len(specs), len(goldenSpecs))
	}
	for i, g := range goldenSpecs {
		s := specs[i]
		if s.Name != g.name {
			t.Errorf("tool %d is %q, want %q", i, s.Name, g.name)
		}
		if s.Description != g.description {
			t.Errorf("%s: description\n got %q\nwant %q", g.name, s.Description, g.description)
		}
		if string(s.InputSchema) != g.schema {
			t.Errorf("%s: schema\n got %s\nwant %s", g.name, s.InputSchema, g.schema)
		}
		// The schema is what an encoding of its map gives: valid JSON,
		// keys sorted, of type object.
		var m map[string]any
		if err := json.Unmarshal(s.InputSchema, &m); err != nil {
			t.Fatalf("%s: %v", g.name, err)
		}
		if m["type"] != "object" {
			t.Errorf("%s: type %v", g.name, m["type"])
		}
		again, _ := json.Marshal(m)
		if !bytes.Equal(again, s.InputSchema) {
			t.Errorf("%s: schema is not in the canonical encoding:\n%s\n%s", g.name, s.InputSchema, again)
		}
	}
	// Specs hands out copies.
	specs[0].InputSchema[0] = 'X'
	if Specs()[0].InputSchema[0] != '{' {
		t.Error("Specs shares its schemas with the caller")
	}
	if MaxToolOutput != 50000 || DefaultTimeout != 120*time.Second {
		t.Errorf("MaxToolOutput %d, DefaultTimeout %v", MaxToolOutput, DefaultTimeout)
	}
}

// The process tools take their arguments from argv, where python puts "-c"
// in argv[0]: a placeholder as bash -c needs would shift every argument by
// one, and the timeout would arrive as the command.
func TestProcessArgv(t *testing.T) {
	c, err := prepare(ToolProcessStart, json.RawMessage(`{"command":"npm run dev","timeout":1800,"name":"dev","cwd":"/workspace"}`), DefaultTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.argv) != 7 || c.argv[0] != "python3" || c.argv[1] != "-c" || c.argv[2] != processStartScript {
		t.Fatalf("argv %q", c.argv)
	}
	if got := c.argv[3:]; got[0] != "npm run dev" || got[1] != "1800" || got[2] != "dev" || got[3] != "/workspace" {
		t.Errorf("arguments %q", got)
	}
	if c.startTimeout != 1800*time.Second || c.timeout != DefaultTimeout {
		t.Errorf("start timeout %v, call timeout %v", c.startTimeout, c.timeout)
	}
	for args, want := range map[string]string{
		`{"command":"x","timeout":-1}`:  "timeout must be 0 (no limit) or a number of seconds",
		`{"command":"  ","timeout":0}`:  "command is required",
		`{"command":"x","timeout":"1"}`: "json: cannot unmarshal string into Go struct field ProcessStartParams.timeout of type int",
	} {
		if _, err := prepare(ToolProcessStart, json.RawMessage(args), DefaultTimeout); err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %q", args, err, want)
		}
	}
	c, err = prepare(ToolProcessLogs, json.RawMessage(`{"id":"120001-abc"}`), DefaultTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if c.argv[2] != processLogsScript || c.argv[3] != "120001-abc" || c.argv[4] != "100" {
		t.Errorf("process_logs argv %q (the default is 100 lines)", c.argv[3:])
	}
	c, _ = prepare(ToolProcessStop, json.RawMessage(`{"id":"120001-abc"}`), DefaultTimeout)
	if len(c.argv) != 4 || c.argv[2] != processStopScript || c.argv[3] != "120001-abc" {
		t.Errorf("process_stop argv %q", c.argv)
	}
	c, _ = prepare(ToolProcessList, json.RawMessage(`{}`), DefaultTimeout)
	if len(c.argv) != 3 || c.argv[2] != processListScript {
		t.Errorf("process_list argv %q", c.argv)
	}
	c, _ = prepare(ToolBash, json.RawMessage(`{"command":"ls","timeout":7}`), DefaultTimeout)
	if strings.Join(c.argv, "|") != "bash|-c|ls" || c.timeout != 7*time.Second {
		t.Errorf("bash argv %q timeout %v", c.argv, c.timeout)
	}
	c, _ = prepare(ToolBash, json.RawMessage(`{"command":"ls","timeout":-3}`), DefaultTimeout)
	if c.timeout != DefaultTimeout {
		t.Errorf("bash with a negative timeout: %v", c.timeout)
	}

	// A timeout too large for a time.Duration is cut to the largest one
	// the pods' protocol carries, never wrapped around into the past; the
	// script still gets the number as written.
	limit := time.Duration(math.MaxUint32) * time.Second
	c, _ = prepare(ToolBash, json.RawMessage(`{"command":"ls","timeout":10000000000}`), DefaultTimeout)
	if c.timeout != limit {
		t.Errorf("bash with a huge timeout: %v", c.timeout)
	}
	c, _ = prepare(ToolProcessStart, json.RawMessage(`{"command":"x","timeout":10000000000}`), DefaultTimeout)
	if c.startTimeout != limit || c.argv[4] != "10000000000" {
		t.Errorf("process_start with a huge timeout: %v %q", c.startTimeout, c.argv[4])
	}
}

// The arguments are checked before anything runs, with the texts the
// clients have seen.
func TestPrepareErrors(t *testing.T) {
	for _, tc := range []struct{ tool, args, want string }{
		{ToolBash, `{"command":5}`, "json: cannot unmarshal number into Go struct field BashParams.command of type string"},
		{ToolEdit, `{"path":"/a"}`, "edits must not be empty"},
		{ToolEdit, `{"path":"/a","edits":[]}`, "edits must not be empty"},
		{ToolRg, `{"path":"/"}`, "pattern is required"},
		{ToolFd, `{"pattern":""}`, "pattern is required"},
		{ToolWebfetch, `{}`, "webfetch: bad params"},
		{ToolWebfetch, `{"url":5}`, "webfetch: bad params"},
		{ToolRead, `{"path":"/a","offset":-1}`, "offset must not be negative, got -1"},
		{ToolRead, `{"path":"/a","limit":-2}`, "limit must not be negative, got -2"},
		{ToolLs, `{"limit":-1}`, "limit must not be negative, got -1"},
		{ToolRg, `{"pattern":"x","context":-1}`, "context must not be negative, got -1"},
		{ToolFd, `{"pattern":"x","limit":-5}`, "limit must not be negative, got -5"},
		{ToolRead, `[1]`, "json: cannot unmarshal array into Go value of type tools.ReadParams"},
		{"nope", `{}`, `unknown tool "nope"`},
	} {
		_, err := prepare(tc.tool, json.RawMessage(tc.args), DefaultTimeout)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s %s: %v, want %q", tc.tool, tc.args, err, tc.want)
		}
	}
	// An empty bash command is a command: bash -c "" exits 0.
	if _, err := prepare(ToolBash, json.RawMessage(`{}`), DefaultTimeout); err != nil {
		t.Errorf("bash without a command: %v", err)
	}
}

// The scripts are Go strings, where a typo is invisible until a sandbox
// runs them: compile them.
func TestProcessScriptsCompile(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed")
	}
	for name, src := range map[string]string{
		"processStartScript": processStartScript,
		"processListScript":  processListScript,
		"processLogsScript":  processLogsScript,
		"processStopScript":  processStopScript,
	} {
		cmd := exec.Command("python3", "-c", `import sys; compile(sys.stdin.read(), sys.argv[1], "exec")`, name)
		cmd.Stdin = strings.NewReader(src)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s does not compile: %v\n%s", name, err, out)
		}
	}
}

// runScript runs a process script with its records under root instead of
// the container's directory.
func runScript(t *testing.T, root, script string, args ...string) (string, int) {
	t.Helper()
	src := strings.ReplaceAll(script, processRoot, root)
	cmd := exec.Command("python3", append([]string{"-c", src}, args...)...)
	out, err := cmd.Output()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

// The scripts' answers are what the clients parse: process_start's JSON
// handle, process_list's JSON array or its "no background processes", the
// texts of process_logs and process_stop. A zombie is not a running
// process.
func TestProcessScriptsRun(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed")
	}
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("timeout is not installed")
	}
	root := filepath.Join(t.TempDir(), "processes")

	out, code := runScript(t, root, processListScript)
	if out != "no background processes\n" || code != 0 {
		t.Fatalf("empty list %q, %d", out, code)
	}
	if rows, ok := parseProcessList(out); !ok || len(rows) != 0 {
		t.Errorf("parse of the empty list: %v %v", rows, ok)
	}

	out, code = runScript(t, root, processStartScript, "echo hello; echo oops >&2; sleep 30", "60", "dev server", "")
	if code != 0 {
		t.Fatalf("start: %d %q", code, out)
	}
	var handle struct {
		ID      string `json:"id"`
		PGID    int    `json:"pgid"`
		Log     string `json:"log"`
		Timeout int    `json:"timeout"`
		Name    string `json:"name"`
	}
	if err := json.Unmarshal([]byte(out), &handle); err != nil {
		t.Fatalf("handle %q: %v", out, err)
	}
	if handle.Timeout != 60 || handle.Name != "dev server" || handle.Log != filepath.Join(root, handle.ID, "log") || handle.PGID == 0 {
		t.Errorf("handle %+v", handle)
	}
	if id, ok := parseHandle(out); !ok || id != handle.ID {
		t.Errorf("parseHandle %q %v", id, ok)
	}
	quick, _ := runScript(t, root, processStartScript, "sh -c 'exit 7'", "0", "", "")
	quickID, _ := parseHandle(quick)

	waitFor(t, "the log", func() bool {
		b, _ := os.ReadFile(handle.Log)
		return strings.Contains(string(b), "oops")
	})
	waitFor(t, "the quick process's exit", func() bool {
		_, err := os.Stat(filepath.Join(root, quickID, "exit"))
		return err == nil
	})

	out, _ = runScript(t, root, processListScript)
	if !strings.HasPrefix(out, "[\n  {\n") {
		t.Errorf("list is not an indented JSON array: %q", out)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list) != 2 {
		t.Fatalf("list %q: %v", out, err)
	}
	byID := map[string]map[string]any{}
	for _, row := range list {
		byID[row["id"].(string)] = row
	}
	run, done := byID[handle.ID], byID[quickID]
	if run["running"] != true || run["name"] != "dev server" || run["timeout"] != float64(60) || run["seconds_left"] == nil || run["exit_code"] != nil {
		t.Errorf("running row %v", run)
	}
	if done["running"] != false || done["exit_code"] != float64(7) || done["seconds_left"] != nil {
		t.Errorf("ended row %v", done)
	}
	rows, ok := parseProcessList(out)
	if !ok || len(rows) != 2 {
		t.Fatalf("parse %v %v", rows, ok)
	}

	out, code = runScript(t, root, processLogsScript, handle.ID, "100")
	if code != 0 || !strings.Contains(out, "hello\n") || !strings.Contains(out, "oops\n") {
		t.Errorf("logs %d %q", code, out)
	}
	out, code = runScript(t, root, processLogsScript, quickID, "100")
	if code != 0 || out != "(no output yet)\n" {
		t.Errorf("logs of a silent process %d %q", code, out)
	}
	out, code = runScript(t, root, processLogsScript, "000000-nope", "100")
	if code != 1 || out != "no such process: 000000-nope\n" {
		t.Errorf("logs of an unknown process %d %q", code, out)
	}

	out, code = runScript(t, root, processStopScript, handle.ID)
	if code != 0 || out != "process "+handle.ID+" stopped\n" {
		t.Errorf("stop %d %q", code, out)
	}
	out, code = runScript(t, root, processStopScript, handle.ID)
	if code != 0 || out != "process "+handle.ID+" was not running any more\n" {
		t.Errorf("second stop %d %q", code, out)
	}
	out, code = runScript(t, root, processStopScript, "000000-nope")
	if code != 1 || out != "no such process: 000000-nope\n" {
		t.Errorf("stop of an unknown process %d %q", code, out)
	}
	out, _ = runScript(t, root, processListScript)
	rows, _ = parseProcessList(out)
	for _, r := range rows {
		if r.Running {
			t.Errorf("%s still runs after the stop: %q", r.ID, out)
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
