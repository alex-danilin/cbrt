package projects

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Visibility verdicts stored in the project list.
const (
	Public  = "public"
	Private = "private"
)

// probeTimeout bounds one visibility request, probeTotal all of them
// (tests shorten them).
var (
	probeTimeout = 5 * time.Second
	probeTotal   = 15 * time.Second
)

// userAgent identifies the visibility requests (GitHub requires one).
const userAgent = "cbrt-projects (visibility check)"

// Markers around the block connect adds to .git/info/exclude; the start
// line names the project's folder relative to the repository top level.
const (
	excludeStart = "# cbrt:exclude "
	excludeEnd   = "# /cbrt:exclude"
)

// remoteRef is a remote URL taken apart: a lower-case host without port or
// user, and the repository path without a leading or trailing slash or
// ".git". Local is set for a remote that is a folder on this machine.
type remoteRef struct {
	Host, Path string
	Local      bool
}

// parseRemote takes apart the URL forms git accepts: scheme URLs (https,
// http, ssh, git), scp-like user@host:path, and local paths (POSIX,
// Windows drive and UNC paths, file://). ok is false for an empty URL.
func parseRemote(raw string) (remoteRef, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return remoteRef{}, false
	}
	if isLocalPath(s) {
		return remoteRef{Local: true, Path: s}, true
	}
	var host, path string
	if i := strings.Index(s, "://"); i >= 0 {
		u, err := url.Parse(s)
		if err != nil {
			return remoteRef{}, false
		}
		switch strings.ToLower(u.Scheme) {
		case "file":
			return remoteRef{Local: true, Path: u.Path}, true
		}
		host, path = u.Hostname(), u.Path
	} else {
		// scp-like: [user@]host:path, the colon before any slash
		colon := strings.Index(s, ":")
		slash := strings.IndexAny(s, `/\`)
		if colon < 0 || (slash >= 0 && slash < colon) {
			return remoteRef{Local: true, Path: s}, true
		}
		host, path = s[:colon], s[colon+1:]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		host = strings.Trim(host, "[]")
	}
	if host == "" {
		return remoteRef{}, false
	}
	parts := strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' })
	if n := len(parts); n > 0 {
		parts[n-1] = strings.TrimSuffix(parts[n-1], ".git")
		if parts[n-1] == "" {
			parts = parts[:n-1]
		}
	}
	return remoteRef{Host: strings.ToLower(host), Path: strings.Join(parts, "/")}, true
}

// isLocalPath: an absolute or relative path, a Windows drive path (C:\x,
// C:/x) or a UNC path; these have no host to ask.
func isLocalPath(s string) bool {
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, `\`) || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "~") {
		return true
	}
	if len(s) >= 2 && s[1] == ':' && ((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z')) &&
		(len(s) == 2 || s[2] == '/' || s[2] == '\\') {
		return true
	}
	return false
}

// remotes is the fetch and push URLs of the repository at dir (`git remote
// -v`), in order, without duplicates: a private fetch mirror with a public
// pushurl is public.
func remotes(dir string) ([]string, error) {
	out, err := git(dir, "remote", "-v")
	if err != nil {
		return nil, err
	}
	var urls []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		rest, ok := strings.CutSuffix(l, "(fetch)")
		if !ok {
			rest, ok = strings.CutSuffix(l, "(push)")
		}
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 2 {
			continue
		}
		u := strings.Join(f[1:], " ")
		if !slices.Contains(urls, u) {
			urls = append(urls, u)
		}
	}
	return urls, nil
}

// verdict is a visibility decision and how it was reached.
type verdict struct {
	Visibility string // Public or Private
	How        string // short, for projects.json and the printed line
}

// noRemoteHow starts the verdict of a repository without a remote; the
// page flags such a project once a remote appears.
const noRemoteHow = "no git remote"

// decideVisibility applies the flag, else asks the hosts of urls. Every
// remote is asked until one is public; a host cbrt cannot ask, a failure
// or an unexpected answer counts as public (privacy first).
func decideVisibility(ctx context.Context, flag string, urls []string, client *http.Client) verdict {
	switch flag {
	case Public, Private:
		return verdict{flag, "set with --" + flag}
	}
	if len(urls) == 0 {
		return verdict{Private, noRemoteHow + ": the repository is local only"}
	}
	if client == nil {
		client = &http.Client{Timeout: probeTimeout}
	}
	ctx, cancel := context.WithTimeout(ctx, probeTotal)
	defer cancel()
	var reasons []string
	for _, raw := range urls {
		v, how := probeRemote(ctx, raw, client)
		if v == Public {
			return verdict{Public, how}
		}
		reasons = append(reasons, how)
	}
	return verdict{Private, strings.Join(reasons, "; ")}
}

// probeRemote asks one remote's host whether the repository is public.
func probeRemote(ctx context.Context, raw string, client *http.Client) (string, string) {
	r, ok := parseRemote(raw)
	switch {
	case !ok:
		return Public, fmt.Sprintf("remote %q cannot be read; treated as public (privacy first)", redactURL(raw))
	case r.Local:
		return Private, "remote " + redactURL(raw) + " is a folder on this machine"
	}
	name := r.Host + "/" + r.Path
	var api string
	switch r.Host {
	case "github.com", "www.github.com", "ssh.github.com":
		parts := strings.Split(r.Path, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return Public, name + " is not an owner/repository path; treated as public (privacy first)"
		}
		name = "github.com/" + r.Path
		api = "https://api.github.com/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
	case "gitlab.com", "www.gitlab.com", "altssh.gitlab.com":
		if !strings.Contains(r.Path, "/") {
			return Public, name + " is not a group/project path; treated as public (privacy first)"
		}
		name = "gitlab.com/" + r.Path
		api = "https://gitlab.com/api/v4/projects/" + url.PathEscape(r.Path)
	default:
		return Public, r.Host + " is not a host cbrt can ask; treated as public (privacy first)"
	}
	status, err := probe(ctx, client, api)
	switch {
	case err != nil:
		return Public, fmt.Sprintf("%s could not be checked (%s); treated as public (privacy first)", name, Clean(err.Error()))
	case status == http.StatusOK:
		return Public, name + " is public"
	case status == http.StatusNotFound:
		return Private, name + " is private (not visible without a login)"
	}
	return Public, fmt.Sprintf("%s answered %d; treated as public (privacy first)", name, status)
}

// probe sends an anonymous GET: no credentials, no cookies, a User-Agent.
func probe(ctx context.Context, client *http.Client, api string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, errors.New("timed out")
		}
		return 0, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return resp.StatusCode, nil
}

// redactURL drops a user name and password from a remote URL before it is
// printed or stored.
// A URL url.Parse rejects (a bad escape, a space in the password) still
// loses everything between "://" and the last "@" before the path.
func redactURL(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		if u.User != nil {
			u.User = nil
			return Clean(u.String())
		}
		return Clean(raw)
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		auth := rest
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			auth = rest[:j]
		}
		if at := strings.LastIndexByte(auth, '@'); at >= 0 {
			raw = raw[:i+3] + rest[at+1:]
		}
	}
	return Clean(raw)
}

// excludeTarget is where a project's exclude block goes: the exclude file
// of its repository and the project's folder relative to the top level
// ("" or "packages/app/", slash-separated).
type excludeTarget struct {
	File, Prefix string
}

// findExclude asks git for the exclude file of dir's repository (this also
// resolves worktrees and nested repositories) and dir's prefix in it.
func findExclude(dir string) (excludeTarget, error) {
	p, err := git(dir, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return excludeTarget{}, err
	}
	p = filepath.FromSlash(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	prefix, err := git(dir, "rev-parse", "--show-prefix")
	if err != nil {
		return excludeTarget{}, err
	}
	return excludeTarget{File: p, Prefix: prefix}, nil
}

// excludeFiles is the project-relative files that stay local in a public
// repository; a trailing slash marks a folder.
func excludeFiles(agents bool) []string {
	fs := []string{"PLAN.md", ".cbrt/"}
	if agents {
		fs = append(fs, "AGENTS.md")
	}
	return fs
}

// excludePattern anchors a repository-relative path as a gitignore pattern,
// escaping the characters gitignore treats specially (a leading ! or # is
// not special after the leading slash).
func excludePattern(rel string) string {
	var b strings.Builder
	b.WriteByte('/')
	for _, r := range rel {
		switch r {
		case '*', '?', '[', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	s := b.String()
	if strings.HasSuffix(s, " ") {
		s = strings.TrimSuffix(s, " ") + `\ `
	}
	return s
}

// excludeBlock is the marker-delimited block for a project at prefix.
func excludeBlock(prefix string, files []string) string {
	var b strings.Builder
	b.WriteString(excludeStart + excludePattern(prefix) + "\n")
	b.WriteString("# the cbrt project plan stays local in this public repository\n")
	for _, f := range files {
		b.WriteString(excludePattern(prefix+f) + "\n")
	}
	b.WriteString(excludeEnd + "\n")
	return b.String()
}

// findExcludeBlock returns the byte range of the block for prefix in b,
// including its line end; ok is false when there is none.
func findExcludeBlock(b []byte, prefix string) (int, int, bool) {
	s := string(b)
	start := excludeStart + excludePattern(prefix)
	off := 0
	for {
		i := strings.Index(s[off:], start)
		if i < 0 {
			return 0, 0, false
		}
		i += off
		lineEnd := i + len(start)
		if (i == 0 || s[i-1] == '\n') && (lineEnd == len(s) || s[lineEnd] == '\n' || s[lineEnd] == '\r') {
			j := strings.Index(s[lineEnd:], excludeEnd)
			if j < 0 {
				return 0, 0, false
			}
			end := lineEnd + j + len(excludeEnd)
			if strings.HasPrefix(s[end:], "\r\n") {
				end += 2
			} else if strings.HasPrefix(s[end:], "\n") {
				end++
			}
			return i, end, true
		}
		off = lineEnd
	}
}

// withExcludeBlock is the exclude file b with the project's block added or
// replaced in place; changed is false when it is already there as is.
func withExcludeBlock(b []byte, block, prefix string) ([]byte, bool) {
	nl := lineEnding(b)
	block = strings.ReplaceAll(block, "\n", nl)
	if i, j, ok := findExcludeBlock(b, prefix); ok {
		if string(b[i:j]) == block {
			return b, false
		}
		out := slices.Concat(b[:i], []byte(block), b[j:])
		return out, true
	}
	out := slices.Clone(b)
	if len(out) > 0 && !bytes.HasSuffix(out, []byte("\n")) {
		out = append(out, nl...)
	}
	return append(out, block...), true
}

// withoutExcludeBlock removes the project's block; ok is false when b has
// none.
func withoutExcludeBlock(b []byte, prefix string) ([]byte, bool) {
	i, j, ok := findExcludeBlock(b, prefix)
	if !ok {
		return b, false
	}
	return slices.Concat(b[:i], b[j:]), true
}

// writeExclude adds or updates the project's block in the exclude file,
// creating the file (and its folder) when missing. changed reports a write
// (or, in a dry run, one that would happen).
func writeExclude(t excludeTarget, files []string, dry bool) (bool, error) {
	b, err := os.ReadFile(t.File)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	missing := err != nil
	out, changed := withExcludeBlock(b, excludeBlock(t.Prefix, files), t.Prefix)
	if !changed || dry {
		return changed, nil
	}
	if missing {
		if err := os.MkdirAll(filepath.Dir(t.File), 0o755); err != nil {
			return false, err
		}
		return true, createFile(t.File, out)
	}
	return true, replaceFile(t.File, out)
}

// keepLocal writes the exclude block for a public repository and prints
// what git still tracks: the block cannot hide a tracked file, and connect
// never rewrites history, so it prints the command that stops tracking it.
// Without a repository yet (a dry run before git init) there is nothing
// tracked to check.
func keepLocal(dir string, t excludeTarget, files []string, haveRepo bool, a *actor) error {
	changed, err := writeExclude(t, files, a.dry)
	if err != nil {
		return err
	}
	pats := make([]string, len(files))
	for i, f := range files {
		pats[i] = excludePattern(t.Prefix + f)
	}
	if changed {
		a.act("kept %s out of git locally in %s", "keep %s out of git locally in %s", strings.Join(pats, " "), Clean(t.File))
	}
	if !haveRepo {
		return nil
	}
	for _, f := range files {
		name := strings.TrimSuffix(f, "/")
		tracked, err := gitStatus(dir, "ls-files", "--error-unmatch", "--", name)
		if err != nil {
			return err
		}
		if !tracked {
			continue
		}
		r := ""
		if name != f {
			r = " -r"
		}
		a.note("%s is already tracked by git, so the exclude block does not hide it; history is not changed. To stop tracking it, run:", f)
		a.note(`  git -C "%s" rm --cached%s -- %s`, Clean(dir), r, name)
	}
	if tracked, err := gitStatus(dir, "ls-files", "--error-unmatch", "--", "AGENTS.md"); err != nil {
		return err
	} else if tracked {
		a.note("AGENTS.md is tracked: its plan rule block (generic, no plan content) will be public")
	}
	return nil
}

// removeExclude removes the project's exclude block from its repository
// (disconnect --remove-files); a folder that is no repository has none.
func removeExclude(dir string, a *actor) error {
	t, err := findExclude(dir)
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(t.File)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	out, ok := withoutExcludeBlock(b, t.Prefix)
	if !ok {
		return nil
	}
	if err := replaceFile(t.File, out); err != nil {
		return err
	}
	a.act("removed the local exclude block from %s", "", Clean(t.File))
	return nil
}

// hasExcludeBlock reports the project's block in the exclude file.
func hasExcludeBlock(t excludeTarget) bool {
	b, err := os.ReadFile(t.File)
	if err != nil {
		return false
	}
	_, _, ok := findExcludeBlock(b, t.Prefix)
	return ok
}
