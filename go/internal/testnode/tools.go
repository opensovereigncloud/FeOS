// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	toolv1 "github.com/ironcore-dev/feos/go/gen/feos/tool/v1"
)

// The guest's tool limits (tool-service tools.rs).
const (
	readMaxBytes  = 16 << 20
	readDefLimit  = 2000
	imageMaxBytes = 3840 << 10
	imageMaxSide  = 8000
	lsDefLimit    = 500
	rgDefLimit    = 100
	rgMaxBytes    = 1 << 20
	fdDefLimit    = 1000
	fetchMaxBody  = 2 << 20
	fetchTimeout  = 30 * time.Second
	fetchAgent    = "feos-webfetch/1.0 (+readable extraction; trafilatura-rs)"
)

// toolService is the guest's ToolService on the pod's directory, which
// stands in for every container's root filesystem: paths are
// container-rooted and confined to it (os.Root, as openat2 with
// RESOLVE_BENEATH confines the guest's). Tool failures come in the
// responses' error fields, as the guest's do. Unlike the guest's, rg and
// fd read no ignore files; they skip hidden entries as the guest's do.
type toolService struct {
	toolv1.UnimplementedToolServiceServer
	n *Node
}

// toRel makes a container-view path rootfs-relative (container_fs.rs
// to_rel): a leading "/" is dropped, "." collapses, ".." pops a component,
// and a ".." that would leave the root is an error. "" is the root.
func toRel(p string) (string, error) {
	if p == "" {
		p = "/"
	}
	if strings.ContainsRune(p, 0) {
		return "", errors.New("NUL byte in path")
	}
	var parts []string
	for _, c := range strings.Split(p, "/") {
		switch c {
		case "", ".":
		case "..":
			if len(parts) == 0 {
				return "", fmt.Errorf("path %q escapes the container root", p)
			}
			parts = parts[:len(parts)-1]
		default:
			parts = append(parts, c)
		}
	}
	return strings.Join(parts, "/"), nil
}

// osRel is a rootfs-relative path as os.Root names it.
func osRel(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}

// openRoot opens the container's root: the pod's directory, for "main"
// and every container of the pod's spec.
func openRoot(g *guest, container string) (*os.Root, error) {
	name := container
	if name == "" {
		name = "main"
	}
	if len(name) > 64 || !validContainerName(name) {
		return nil, fmt.Errorf("bad container name %q", name)
	}
	if !g.containers[name] {
		return nil, fmt.Errorf("container %s has no rootfs (not created?)", name)
	}
	r, err := os.OpenRoot(g.dir)
	if err != nil {
		return nil, fmt.Errorf("open rootfs: %v", err)
	}
	return r, nil
}

// scripted answers a call from the script, when it has an answer.
func scripted[T proto.Message](ctx context.Context, t *toolService, method string, req proto.Message) (T, bool, error) {
	var zero T
	f := t.n.script.toolFunc()
	if f == nil {
		return zero, false, nil
	}
	g, err := guestFrom(ctx)
	if err != nil {
		return zero, false, err
	}
	resp, ok, err := f(ctx, g.uid, method, req)
	if err != nil || !ok {
		return zero, ok, err
	}
	typed, isT := resp.(T)
	if !isT {
		return zero, false, status.Errorf(codes.Internal, "the script answered %s with %T", method, resp)
	}
	return typed, true, nil
}

func (t *toolService) Read(ctx context.Context, req *toolv1.ReadRequest) (*toolv1.ReadResponse, error) {
	if resp, ok, err := scripted[*toolv1.ReadResponse](ctx, t, toolv1.ToolService_Read_FullMethodName, req); ok || err != nil {
		return resp, err
	}
	g, err := guestFrom(ctx)
	if err != nil {
		return nil, err
	}
	return readTool(g, req), nil
}

func readTool(g *guest, req *toolv1.ReadRequest) *toolv1.ReadResponse {
	fail := func(e string) *toolv1.ReadResponse { return &toolv1.ReadResponse{Error: e} }
	root, err := openRoot(g, req.Container)
	if err != nil {
		return fail(err.Error())
	}
	defer root.Close()
	rel, err := toRel(req.Path)
	if err != nil {
		return fail(err.Error())
	}
	f, err := root.Open(osRel(rel))
	if err != nil {
		return fail(fmt.Sprintf("Path not found: %s (%v)", req.Path, err))
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, readMaxBytes+1))
	if err != nil {
		return fail(fmt.Sprintf("read %s: %v", req.Path, err))
	}
	if len(buf) > readMaxBytes {
		return fail(fmt.Sprintf("file exceeds %d MiB — use offset/limit is not implemented for >cap; file is too large", readMaxBytes>>20))
	}
	if pic, ok, err := sniffPicture(buf); ok {
		if err != nil {
			return fail(fmt.Sprintf("%s: %v", req.Path, err))
		}
		if len(buf) > imageMaxBytes {
			return fail(fmt.Sprintf("%s: %s is %d KiB, images are read up to %d KiB — take a smaller one (a viewport instead of a full-page screenshot, or JPEG)",
				req.Path, pic.mediaType, len(buf)/1024, imageMaxBytes/1024))
		}
		if pic.width > imageMaxSide || pic.height > imageMaxSide {
			return fail(fmt.Sprintf("%s: %s is %dx%d px, images are read up to %d px on a side — take a smaller one (a viewport instead of a full-page screenshot)",
				req.Path, pic.mediaType, pic.width, pic.height, imageMaxSide))
		}
		return &toolv1.ReadResponse{Image: buf, MediaType: pic.mediaType, Width: pic.width, Height: pic.height}
	}
	text := strings.ToValidUTF8(string(buf), "�")
	lines := splitLines(text)
	total := len(lines)
	off := max(int(req.Offset), 1)
	limit := int(req.Limit)
	if limit == 0 {
		limit = readDefLimit
	}
	if off > total {
		return &toolv1.ReadResponse{}
	}
	end := min(off-1+limit, total)
	out := strings.Join(lines[off-1:end], "\n")
	if out != "" && (end < total || strings.HasSuffix(text, "\n")) {
		out += "\n"
	}
	return &toolv1.ReadResponse{Text: out, Truncated: end < total}
}

// splitLines splits as Rust's str::lines: at "\n" or "\r\n", without an
// empty last line after a final line ending.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

// picture is an image that read hands back whole.
type picture struct {
	mediaType     string
	width, height uint32
}

// sniffPicture recognises PNG, JPEG, GIF and WebP by their leading bytes
// and reads their pixel size. ok false: not one of them. An error: it
// starts like one but its header does not parse.
func sniffPicture(b []byte) (picture, bool, error) {
	be16 := func(i int) (uint32, bool) {
		if i+2 > len(b) {
			return 0, false
		}
		return uint32(binary.BigEndian.Uint16(b[i:])), true
	}
	le16 := func(i int) (uint32, bool) {
		if i+2 > len(b) {
			return 0, false
		}
		return uint32(binary.LittleEndian.Uint16(b[i:])), true
	}
	le24 := func(i int) (uint32, bool) {
		if i+3 > len(b) {
			return 0, false
		}
		return uint32(b[i]) | uint32(b[i+1])<<8 | uint32(b[i+2])<<16, true
	}
	pic := func(mt string, w, h uint32, ok bool) (picture, bool, error) {
		if !ok || w == 0 || h == 0 {
			return picture{}, true, fmt.Errorf("%s: the header does not parse", mt)
		}
		return picture{mediaType: mt, width: w, height: h}, true, nil
	}
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		if len(b) < 24 || string(b[12:16]) != "IHDR" {
			return pic("image/png", 0, 0, false)
		}
		return pic("image/png", binary.BigEndian.Uint32(b[16:]), binary.BigEndian.Uint32(b[20:]), true)
	case bytes.HasPrefix(b, []byte("GIF87a")) || bytes.HasPrefix(b, []byte("GIF89a")):
		w, ok1 := le16(6)
		h, ok2 := le16(8)
		return pic("image/gif", w, h, ok1 && ok2)
	case bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}):
		i := 2
		for {
			if i >= len(b) || b[i] != 0xff {
				return pic("image/jpeg", 0, 0, false)
			}
			for i < len(b) && b[i] == 0xff {
				i++
			}
			if i >= len(b) {
				return pic("image/jpeg", 0, 0, false)
			}
			m := b[i]
			i++
			switch {
			case m == 0xd8 || m == 0x01 || m >= 0xd0 && m <= 0xd7:
				continue
			case m == 0xd9:
				return pic("image/jpeg", 0, 0, false)
			case m >= 0xc0 && m <= 0xcf && m != 0xc4 && m != 0xc8 && m != 0xcc:
				h, ok1 := be16(i + 3)
				w, ok2 := be16(i + 5)
				return pic("image/jpeg", w, h, ok1 && ok2)
			default:
				l, ok := be16(i)
				if !ok || l < 2 {
					return pic("image/jpeg", 0, 0, false)
				}
				i += int(l)
			}
		}
	case bytes.HasPrefix(b, []byte("RIFF")) && len(b) >= 12 && string(b[8:12]) == "WEBP":
		if len(b) < 16 {
			return pic("image/webp", 0, 0, false)
		}
		switch string(b[12:16]) {
		case "VP8 ":
			if len(b) >= 26 && bytes.Equal(b[23:26], []byte{0x9d, 0x01, 0x2a}) {
				w, ok1 := le16(26)
				h, ok2 := le16(28)
				return pic("image/webp", w&0x3fff, h&0x3fff, ok1 && ok2)
			}
		case "VP8L":
			if len(b) >= 25 && b[20] == 0x2f {
				v := binary.LittleEndian.Uint32(b[21:])
				return pic("image/webp", v&0x3fff+1, (v>>14)&0x3fff+1, true)
			}
		case "VP8X":
			w, ok1 := le24(24)
			h, ok2 := le24(27)
			return pic("image/webp", w+1, h+1, ok1 && ok2)
		}
		return pic("image/webp", 0, 0, false)
	}
	return picture{}, false, nil
}

func (t *toolService) Write(ctx context.Context, req *toolv1.WriteRequest) (*toolv1.WriteResponse, error) {
	if resp, ok, err := scripted[*toolv1.WriteResponse](ctx, t, toolv1.ToolService_Write_FullMethodName, req); ok || err != nil {
		return resp, err
	}
	g, err := guestFrom(ctx)
	if err != nil {
		return nil, err
	}
	return &toolv1.WriteResponse{Error: writeTool(g, req)}, nil
}

func writeTool(g *guest, req *toolv1.WriteRequest) string {
	root, err := openRoot(g, req.Container)
	if err != nil {
		return err.Error()
	}
	defer root.Close()
	rel, err := toRel(req.Path)
	if err != nil {
		return err.Error()
	}
	if rel == "" {
		return "write target is the container root"
	}
	if dir := path.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return fmt.Sprintf("mkdir %s: %v", dir, err)
		}
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Sprintf("%s: %v", rel, err)
	}
	defer f.Close()
	if _, err := f.Write(req.Content); err != nil {
		return fmt.Sprintf("write %s: %v", req.Path, err)
	}
	return ""
}

func (t *toolService) Edit(ctx context.Context, req *toolv1.EditRequest) (*toolv1.EditResponse, error) {
	if resp, ok, err := scripted[*toolv1.EditResponse](ctx, t, toolv1.ToolService_Edit_FullMethodName, req); ok || err != nil {
		return resp, err
	}
	g, err := guestFrom(ctx)
	if err != nil {
		return nil, err
	}
	return editTool(g, req), nil
}

// editTool replaces every edit's old text, which must occur exactly once
// in the file as the edits before left it.
func editTool(g *guest, req *toolv1.EditRequest) *toolv1.EditResponse {
	fail := func(e string) *toolv1.EditResponse { return &toolv1.EditResponse{Error: e} }
	if len(req.Edits) == 0 {
		return fail("edits must not be empty")
	}
	root, err := openRoot(g, req.Container)
	if err != nil {
		return fail(err.Error())
	}
	defer root.Close()
	rel, err := toRel(req.Path)
	if err != nil {
		return fail(err.Error())
	}
	raw, err := root.ReadFile(osRel(rel))
	if err != nil {
		return fail(fmt.Sprintf("%s: %v", osRel(rel), err))
	}
	if !utf8.Valid(raw) {
		return fail(fmt.Sprintf("%s is not valid utf-8", req.Path))
	}
	src := string(raw)
	for i, ed := range req.Edits {
		if n := strings.Count(src, ed.Old); n != 1 {
			return fail(fmt.Sprintf("edit %d: oldText not unique (occurrences: %d)", i, n))
		}
		src = strings.Replace(src, ed.Old, ed.New, 1)
	}
	f, err := root.OpenFile(osRel(rel), os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return fail(fmt.Sprintf("truncate %s: %v", req.Path, err))
	}
	defer f.Close()
	if _, err := f.WriteString(src); err != nil {
		return fail(fmt.Sprintf("write %s: %v", req.Path, err))
	}
	return &toolv1.EditResponse{Replacements: uint32(len(req.Edits))}
}

func (t *toolService) Ls(ctx context.Context, req *toolv1.LsRequest) (*toolv1.LsResponse, error) {
	if resp, ok, err := scripted[*toolv1.LsResponse](ctx, t, toolv1.ToolService_Ls_FullMethodName, req); ok || err != nil {
		return resp, err
	}
	g, err := guestFrom(ctx)
	if err != nil {
		return nil, err
	}
	return lsTool(g, req), nil
}

// lsTool lists a directory: entries sorted case-insensitively, "/" after
// directories, dotfiles included.
func lsTool(g *guest, req *toolv1.LsRequest) *toolv1.LsResponse {
	fail := func(e string) *toolv1.LsResponse { return &toolv1.LsResponse{Error: e} }
	root, err := openRoot(g, req.Container)
	if err != nil {
		return fail(err.Error())
	}
	defer root.Close()
	rel, err := toRel(req.Path)
	if err != nil {
		return fail(err.Error())
	}
	st, err := root.Stat(osRel(rel))
	if err != nil {
		return fail(fmt.Sprintf("Path not found: %s (%v)", req.Path, err))
	}
	if !st.IsDir() {
		return fail(fmt.Sprintf("Not a directory: %s", req.Path))
	}
	ents, err := fs.ReadDir(root.FS(), osRel(rel))
	if err != nil {
		return fail(fmt.Sprintf("list %s: %v", req.Path, err))
	}
	limit := int(req.Limit)
	if limit == 0 {
		limit = lsDefLimit
	}
	entries := make([]string, 0, len(ents))
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		entries = append(entries, name)
	}
	slices.SortStableFunc(entries, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	truncated := len(entries) > limit
	if truncated {
		entries = entries[:limit]
	}
	return &toolv1.LsResponse{Entries: entries, Truncated: truncated}
}

// labelPrefix is what rg and fd put before a found path: nothing for the
// root, the searched path and "/" otherwise.
func labelPrefix(p string) string {
	if p == "" || p == "." || p == "/" {
		return ""
	}
	t := strings.TrimRight(p, "/")
	if t == "" {
		return ""
	}
	return t + "/"
}

func (t *toolService) Rg(ctx context.Context, req *toolv1.RgRequest) (*toolv1.RgResponse, error) {
	if resp, ok, err := scripted[*toolv1.RgResponse](ctx, t, toolv1.ToolService_Rg_FullMethodName, req); ok || err != nil {
		return resp, err
	}
	g, err := guestFrom(ctx)
	if err != nil {
		return nil, err
	}
	return rgTool(ctx, g, req), nil
}

// rgSink collects rg's output lines: "label:n:text" for matches,
// "label-n-text" for context and "--" between context groups, until the
// line limit or the byte cap.
type rgSink struct {
	limit     int
	out       strings.Builder
	lines     int
	matches   uint32
	truncated bool
	stop      bool
}

func (s *rgSink) push(line string) {
	if s.stop {
		return
	}
	if s.out.Len()+len(line)+1 > rgMaxBytes {
		s.truncated, s.stop = true, true
		return
	}
	s.out.WriteString(line)
	s.out.WriteByte('\n')
	s.lines++
	if s.lines >= s.limit {
		s.truncated, s.stop = true, true
	}
}

// search searches one file's content. Its lines are split at "\n" and keep
// a "\r", as the guest's searcher reports them.
func (s *rgSink) search(re *regexp.Regexp, label string, content []byte, context int) {
	if len(content) == 0 || bytes.IndexByte(content, 0) >= 0 {
		return // empty, or binary: the search quits at a NUL byte
	}
	lines := strings.Split(strings.TrimSuffix(strings.ToValidUTF8(string(content), "\uFFFD"), "\n"), "\n")
	var hits []int
	for i, l := range lines {
		if re.MatchString(l) {
			hits = append(hits, i)
		}
	}
	if len(hits) == 0 {
		return
	}
	// The context groups: a match with its context, merged where they
	// touch.
	type group struct{ from, to int }
	var groups []group
	isHit := map[int]bool{}
	for _, h := range hits {
		isHit[h] = true
		from, to := max(0, h-context), min(len(lines)-1, h+context)
		if n := len(groups); n > 0 && from <= groups[n-1].to+1 {
			groups[n-1].to = max(groups[n-1].to, to)
			continue
		}
		groups = append(groups, group{from, to})
	}
	for gi, gr := range groups {
		for i := gr.from; i <= gr.to; i++ {
			if s.stop {
				return
			}
			// As the guest's sink: a match that follows a separator which
			// reached the limit is counted, but not written.
			if context > 0 && gi > 0 && i == gr.from && s.out.Len() > 0 {
				s.push("--")
			}
			if isHit[i] {
				s.push(fmt.Sprintf("%s:%d:%s", label, i+1, lines[i]))
				s.matches++
			} else if context > 0 {
				s.push(fmt.Sprintf("%s-%d-%s", label, i+1, lines[i]))
			}
		}
	}
}

func rgTool(ctx context.Context, g *guest, req *toolv1.RgRequest) *toolv1.RgResponse {
	fail := func(e string) *toolv1.RgResponse { return &toolv1.RgResponse{Error: e} }
	if req.Pattern == "" {
		return fail("pattern is required")
	}
	root, err := openRoot(g, req.Container)
	if err != nil {
		return fail(err.Error())
	}
	defer root.Close()
	rel, err := toRel(req.Path)
	if err != nil {
		return fail(err.Error())
	}
	pattern := req.Pattern
	if req.Literal {
		pattern = regexp.QuoteMeta(pattern)
	}
	if req.IgnoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fail("rg: " + err.Error())
	}
	var glob *regexp.Regexp
	if req.Glob != "" {
		if glob, err = globRegexp(req.Glob, false); err != nil {
			return fail("rg -g: " + err.Error())
		}
	}
	limit := int(req.Limit)
	if limit == 0 {
		limit = rgDefLimit
	}
	sink := &rgSink{limit: limit}
	prefix := labelPrefix(req.Path)
	st, err := root.Stat(osRel(rel))
	if err != nil {
		return fail(fmt.Sprintf("%s: %v", req.Path, err))
	}
	if !st.IsDir() {
		content, err := root.ReadFile(osRel(rel))
		if err != nil {
			return fail(fmt.Sprintf("%s: %v", req.Path, err))
		}
		label := strings.TrimSuffix(prefix, "/")
		if name := path.Base(rel); rel != "" && name != "" {
			label = prefix + name
		}
		sink.search(re, label, content, int(req.Context))
	} else {
		base := osRel(rel)
		_ = fs.WalkDir(root.FS(), base, func(p string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil || sink.stop {
				return fs.SkipAll
			}
			if err != nil || p == base {
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			relFile := strings.TrimPrefix(strings.TrimPrefix(p, base), "/")
			if base == "." {
				relFile = p
			}
			if glob != nil && !glob.MatchString(relFile) {
				return nil
			}
			content, err := root.ReadFile(p)
			if err != nil {
				return nil // unreadable: skipped, as rg skips it
			}
			sink.search(re, prefix+relFile, content, int(req.Context))
			return nil
		})
	}
	return &toolv1.RgResponse{Text: sink.out.String(), Matches: sink.matches, Truncated: sink.truncated}
}

func (t *toolService) Fd(ctx context.Context, req *toolv1.FdRequest) (*toolv1.FdResponse, error) {
	if resp, ok, err := scripted[*toolv1.FdResponse](ctx, t, toolv1.ToolService_Fd_FullMethodName, req); ok || err != nil {
		return resp, err
	}
	g, err := guestFrom(ctx)
	if err != nil {
		return nil, err
	}
	return fdTool(ctx, g, req), nil
}

// fdTool finds entries by name: a regular expression (or a glob) matched
// against the name, case-insensitive unless the pattern has an upper-case
// letter, hidden entries skipped.
func fdTool(ctx context.Context, g *guest, req *toolv1.FdRequest) *toolv1.FdResponse {
	fail := func(e string) *toolv1.FdResponse { return &toolv1.FdResponse{Error: e} }
	if req.Pattern == "" {
		return fail("pattern is required")
	}
	root, err := openRoot(g, req.Container)
	if err != nil {
		return fail(err.Error())
	}
	defer root.Close()
	rel, err := toRel(req.Path)
	if err != nil {
		return fail(err.Error())
	}
	st, err := root.Stat(osRel(rel))
	if err != nil || !st.IsDir() {
		if err == nil {
			err = errors.New("not a directory")
		}
		return fail(fmt.Sprintf("%s: not a directory (%v)", req.Path, err))
	}
	sensitive := strings.IndexFunc(req.Pattern, unicode.IsUpper) >= 0
	var re *regexp.Regexp
	if req.Glob {
		if re, err = globRegexp(req.Pattern, !sensitive); err != nil {
			return fail("fd --glob: " + err.Error())
		}
	} else {
		p := req.Pattern
		if !sensitive {
			p = "(?i)" + p
		}
		if re, err = regexp.Compile(p); err != nil {
			return fail("fd: " + err.Error())
		}
	}
	limit := int(req.Limit)
	if limit == 0 {
		limit = fdDefLimit
	}
	prefix := labelPrefix(req.Path)
	var entries []string
	truncated := false
	base := osRel(rel)
	_ = fs.WalkDir(root.FS(), base, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil || truncated {
			return fs.SkipAll
		}
		if err != nil || p == base {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !re.MatchString(d.Name()) {
			return nil
		}
		entries = append(entries, prefix+d.Name())
		if len(entries) >= limit {
			truncated = true
			return fs.SkipAll
		}
		return nil
	})
	return &toolv1.FdResponse{Entries: entries, Truncated: truncated}
}

func (t *toolService) Webfetch(ctx context.Context, req *toolv1.WebfetchRequest) (*toolv1.WebfetchResponse, error) {
	if resp, ok, err := scripted[*toolv1.WebfetchResponse](ctx, t, toolv1.ToolService_Webfetch_FullMethodName, req); ok || err != nil {
		return resp, err
	}
	u, err := url.Parse(req.Url)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid url %q", req.Url)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, status.Errorf(codes.InvalidArgument, "invalid url %q (http/https only)", req.Url)
	}
	return webfetch(ctx, req.Url), nil
}

// webfetch fetches a page and extracts its text: the title and the text
// of an HTML page without its markup, scripts and styles, or the body of
// anything else.
func webfetch(ctx context.Context, rawURL string) *toolv1.WebfetchResponse {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return &toolv1.WebfetchResponse{Error: "webfetch: fetch: " + err.Error()}
	}
	req.Header.Set("User-Agent", fetchAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &toolv1.WebfetchResponse{Error: "webfetch: fetch: " + err.Error()}
	}
	defer resp.Body.Close()
	effective := resp.Request.URL.String()
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBody+1))
	if err != nil {
		return &toolv1.WebfetchResponse{Error: "webfetch: read body: " + err.Error()}
	}
	if len(body) > fetchMaxBody {
		return &toolv1.WebfetchResponse{Error: fmt.Sprintf("webfetch: read body: larger than %d bytes", fetchMaxBody)}
	}
	code := uint32(resp.StatusCode)
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return &toolv1.WebfetchResponse{Status: code, EffectiveUrl: effective, Error: fmt.Sprintf("webfetch: HTTP %d", resp.StatusCode)}
	}
	page := strings.ToValidUTF8(string(body), "�")
	title, text := "", page
	if strings.Contains(resp.Header.Get("Content-Type"), "html") || looksLikeHTML(page) {
		title, text = extractHTML(page)
	}
	if strings.TrimSpace(text) == "" {
		return &toolv1.WebfetchResponse{Status: code, EffectiveUrl: effective, Error: "webfetch: no readable content extracted"}
	}
	return &toolv1.WebfetchResponse{Status: code, EffectiveUrl: effective, Title: title, Text: text}
}

var (
	reTitle   = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reDrop    = regexp.MustCompile(`(?is)<(script|style|head|noscript)[^>]*>.*?</(script|style|head|noscript)>`)
	reBreak   = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/h[1-6]|/tr)[^>]*>`)
	reTag     = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpaces  = regexp.MustCompile(`[ \t\f\v]+`)
	reNewline = regexp.MustCompile(`\n\s*\n+`)
)

func looksLikeHTML(s string) bool {
	head := strings.ToLower(s[:min(len(s), 512)])
	return strings.Contains(head, "<html") || strings.Contains(head, "<!doctype html")
}

// extractHTML is a plain readable-text extraction: the title, and the
// text of the page without markup, one block per line.
func extractHTML(page string) (title, text string) {
	if m := reTitle.FindStringSubmatch(page); m != nil {
		title = strings.TrimSpace(html.UnescapeString(reTag.ReplaceAllString(m[1], "")))
	}
	body := reDrop.ReplaceAllString(page, "")
	body = reBreak.ReplaceAllString(body, "\n")
	body = html.UnescapeString(reTag.ReplaceAllString(body, ""))
	var lines []string
	for _, l := range strings.Split(body, "\n") {
		if l = strings.TrimSpace(reSpaces.ReplaceAllString(l, " ")); l != "" {
			lines = append(lines, l)
		}
	}
	text = reNewline.ReplaceAllString(strings.Join(lines, "\n"), "\n")
	return title, text
}
