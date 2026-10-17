// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"encoding/json"
	"time"
)

// The tool names of the baseline.
const (
	ToolBash         = "bash"
	ToolRead         = "read"
	ToolWrite        = "write"
	ToolEdit         = "edit"
	ToolLs           = "ls"
	ToolRg           = "rg"
	ToolFd           = "fd"
	ToolProcessStart = "process_start"
	ToolProcessList  = "process_list"
	ToolProcessLogs  = "process_logs"
	ToolProcessStop  = "process_stop"
	ToolWebfetch     = "webfetch"
)

// MaxToolOutput is where a tool's text output is cut, in bytes. It belongs
// to the tool contract rather than to one door: MCP and the REST routes cut
// a result at the same place, so that a client sees the same answer however
// it asks.
const MaxToolOutput = 50_000

// DefaultTimeout bounds a tool call into the guest when the call names no
// timeout of its own (bash's timeout parameter is the only one).
const DefaultTimeout = 120 * time.Second

// Spec is one tool of the baseline: its name, its description and the JSON
// schema of its input, served as they are by tools/list and GET
// /api/v1/tools.
type Spec struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// baseline is the frozen tool surface, in the order every listing serves
// it. The descriptions and schemas are byte for byte those the clients
// were built against; the schemas' keys are sorted, as an encoding of a
// map gives them. Extending it is a deliberate change, not configuration.
var baseline = []Spec{
	{
		Name:        ToolBash,
		Description: "Execute a bash command and return its output with exit code: for building, testing, git, package managers and running programs. Do not read, search or change files with it — read, rg, fd, edit and write do that, and their answers are made for you. Output is truncated to 2000 lines / 50KB.",
		InputSchema: json.RawMessage(`{"properties":{"command":{"description":"Shell command to execute","type":"string"},"timeout":{"description":"Timeout in seconds (default 120); give more for a long build or test run","type":"integer"}},"required":["command"],"type":"object"}`),
	},
	{
		Name:        ToolRead,
		Description: "Read a file. Use this instead of cat, head, tail or sed -n in bash. Text comes back numbered like cat -n: each line starts with its number and a tab, which are not part of the file — leave them out of an edit. Up to 2000 lines or 50KB per call; when the file goes on, the answer ends with the offset to continue from. Use offset (1-based)/limit to read a part of a large file. Images (png, jpg, gif, webp — e.g. a screenshot) are returned as an image you can look at, up to 3.75 MB and 8000 px per side.",
		InputSchema: json.RawMessage(`{"properties":{"limit":{"description":"Maximum number of lines to read","type":"integer"},"offset":{"description":"Line number to start reading from (1-indexed)","type":"integer"},"path":{"description":"Path to the file to read (relative or absolute)","type":"string"}},"required":["path"],"type":"object"}`),
	},
	{
		Name:        ToolWrite,
		Description: "Write a whole file: creates it and its parent directories, or replaces it if it exists. Use this instead of cat <<EOF, echo > or tee in bash. To change part of an existing file, use edit.",
		InputSchema: json.RawMessage(`{"properties":{"content":{"description":"Content to write to the file","type":"string"},"path":{"description":"Path to the file to write (relative or absolute)","type":"string"}},"required":["path","content"],"type":"object"}`),
	},
	{
		Name:        ToolEdit,
		Description: "Change an existing file by exact text replacement. Use this for every change to a file, never sed -i, perl -pi, python or a heredoc in bash. Each entry of edits replaces old with new; old must match the file exactly, whitespace and indentation included, and occur exactly once — take in a line or two around it to make it unique. List several places of one file in one call: they apply to the file as it was and must not overlap. For several files, call edit once per file; several calls in one answer are fine.",
		InputSchema: json.RawMessage(`{"properties":{"edits":{"description":"Targeted exact-match replacements","items":{"properties":{"new":{"description":"Replacement text for this targeted edit","type":"string"},"old":{"description":"Exact text to replace, as it is in the file (without read's line numbers); must occur exactly once","type":"string"}},"required":["old","new"],"type":"object"},"type":"array"},"path":{"description":"Path to the file to edit (relative or absolute)","type":"string"}},"required":["path","edits"],"type":"object"}`),
	},
	{
		Name:        ToolLs,
		Description: "List directory contents. Entries sorted alphabetically, '/' suffix for directories, includes dotfiles. Default limit 500.",
		InputSchema: json.RawMessage(`{"properties":{"limit":{"description":"Maximum number of entries to return (default: 500)","type":"integer"},"path":{"description":"Directory to list (default: current directory)","type":"string"}},"type":"object"}`),
	},
	{
		Name:        ToolRg,
		Description: "Search file contents with ripgrep syntax. Use this instead of grep or rg in bash. Pattern is a regex (Rust regex, like rg). Returns matching lines with file paths and line numbers. Respects .gitignore, skips hidden and binary files. Truncated to 100 match lines (default) or 50KB.",
		InputSchema: json.RawMessage(`{"properties":{"context":{"description":"Lines of context around each match (rg -C)","type":"integer"},"glob":{"description":"Filter files, e.g. '*.py' (rg -g)","type":"string"},"ignoreCase":{"description":"Case-insensitive search (rg -i)","type":"boolean"},"limit":{"description":"Maximum number of match lines (default: 100)","type":"integer"},"literal":{"description":"Treat pattern as literal string (rg -F)","type":"boolean"},"path":{"description":"Directory or file to search (default: current directory)","type":"string"},"pattern":{"description":"Search pattern (regex, ripgrep syntax)","type":"string"}},"required":["pattern"],"type":"object"}`),
	},
	{
		Name:        ToolFd,
		Description: "Find files by name with fd syntax. Use this instead of find or ls -R in bash. Pattern is a regex matched against file names, smart-case (all-lowercase = insensitive); use glob=true for glob patterns like '*.go'. Returns matching file paths. Respects .gitignore, skips hidden files. Truncated to 1000 results (default).",
		InputSchema: json.RawMessage(`{"properties":{"glob":{"description":"Treat pattern as glob, e.g. '*.go' (fd --glob)","type":"boolean"},"limit":{"description":"Maximum number of results (default: 1000)","type":"integer"},"path":{"description":"Directory to search in (default: current directory)","type":"string"},"pattern":{"description":"Pattern to match file names (fd regex, smart case)","type":"string"}},"required":["pattern"],"type":"object"}`),
	},
	{
		Name:        ToolProcessStart,
		Description: "Start a long-running process (a dev server, a watcher, a queue worker) that keeps running after this call returns, and get a handle for it. Use this instead of backgrounding a command inside bash: only a process started here can be listed, read and stopped later. Output goes to a log file, not to this answer. timeout bounds how long the process may run — the sandbox stays awake for that long, so pick what the work needs; 0 means no limit and a sandbox that never falls asleep on its own.",
		InputSchema: json.RawMessage(`{"properties":{"command":{"description":"Shell command to run in the background","type":"string"},"cwd":{"description":"Working directory (default: current directory)","type":"string"},"name":{"description":"Short name shown in process_list, e.g. 'dev server'","type":"string"},"timeout":{"description":"Seconds the process may run; 0 = no limit","type":"integer"}},"required":["command","timeout"],"type":"object"}`),
	},
	{
		Name:        ToolProcessList,
		Description: "List the background processes of this sandbox: id, name, whether they are still running, how long they may still run, and the command behind each.",
		InputSchema: json.RawMessage(`{"properties":{},"type":"object"}`),
	},
	{
		Name:        ToolProcessLogs,
		Description: "Read the output a background process has produced so far (stdout and stderr together, newest lines last).",
		InputSchema: json.RawMessage(`{"properties":{"id":{"description":"Process id from process_start or process_list","type":"string"},"lines":{"description":"How many lines from the end (default: 100)","type":"integer"}},"required":["id"],"type":"object"}`),
	},
	{
		Name:        ToolProcessStop,
		Description: "Stop a background process: SIGTERM to its process group, SIGKILL if it is still there a moment later.",
		InputSchema: json.RawMessage(`{"properties":{"id":{"description":"Process id from process_start or process_list","type":"string"}},"required":["id"],"type":"object"}`),
	},
	{
		Name:        ToolWebfetch,
		Description: "Fetch a web page (http/https) and return its main readable content as text. Boilerplate (navigation, ads, footers) is stripped; links are kept. Use for reading documentation, articles and pages the agent needs to research.",
		InputSchema: json.RawMessage(`{"properties":{"url":{"description":"URL to fetch (http/https)","type":"string"}},"required":["url"],"type":"object"}`),
	},
}

// Specs returns the tool baseline, in order. The caller may keep the slice;
// it is a copy.
func Specs() []Spec {
	out := make([]Spec, len(baseline))
	for i, s := range baseline {
		out[i] = Spec{Name: s.Name, Description: s.Description, InputSchema: append(json.RawMessage(nil), s.InputSchema...)}
	}
	return out
}

// known reports whether name is a tool of the baseline.
func known(name string) bool {
	for _, s := range baseline {
		if s.Name == name {
			return true
		}
	}
	return false
}

// The parameters of the tools: the execution contract, decoded from a
// call's arguments. Their JSON names are those of the input schemas; their
// type names appear in decoding errors ("Go struct field
// BashParams.timeout"), which the clients have seen in this form.

type BashParams struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type ReadParams struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

type WriteParams struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type EditOp struct {
	Old string `json:"old"`
	New string `json:"new"`
}

type EditParams struct {
	Path  string   `json:"path"`
	Edits []EditOp `json:"edits"`
}

type LsParams struct {
	Path  string `json:"path"`
	Limit int    `json:"limit"`
}

type RgParams struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	IgnoreCase bool   `json:"ignoreCase"`
	Literal    bool   `json:"literal"`
	Context    int    `json:"context"`
	Limit      int    `json:"limit"`
}

type FdParams struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
	Glob    bool   `json:"glob"`
	Limit   int    `json:"limit"`
}

type ProcessStartParams struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout"` // seconds the process may run; 0: no limit
	Name    string `json:"name"`
	Cwd     string `json:"cwd"`
}

type ProcessLogsParams struct {
	ID    string `json:"id"`
	Lines int    `json:"lines"`
}

type ProcessStopParams struct {
	ID string `json:"id"`
}

type WebFetchParams struct {
	URL string `json:"url"`
}
