// Package gitrepo wraps the git command line for one work tree. It never
// rewrites history: no force push, no reset --hard, no amend.
package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is returned for unknown revisions or paths.
var ErrNotFound = errors.New("not_found")

// ErrPushRejected is returned when the remote has newer commits.
var ErrPushRejected = errors.New("push rejected: remote has newer commits")

// Options configure identity and credentials.
type Options struct {
	AuthorName  string
	AuthorEmail string
	// Token, when set, is handed to Git through an in-memory credential
	// helper for HTTPS remotes. It is never written to disk.
	Token    string
	Username string
	Logger   *slog.Logger
}

// Repo is a Git work tree.
type Repo struct {
	Dir  string
	env  []string
	log  *slog.Logger
	opts Options
}

// New prepares a Repo handle without checking the directory.
func New(dir string, opts Options) *Repo {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Username == "" {
		opts.Username = "x-access-token"
	}
	r := &Repo{Dir: dir, log: opts.Logger, opts: opts}
	r.env = buildEnv(opts)
	return r
}

func buildEnv(o Options) []string {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	if o.AuthorName != "" {
		env = append(env, "GIT_AUTHOR_NAME="+o.AuthorName, "GIT_COMMITTER_NAME="+o.AuthorName)
	}
	if o.AuthorEmail != "" {
		env = append(env, "GIT_AUTHOR_EMAIL="+o.AuthorEmail, "GIT_COMMITTER_EMAIL="+o.AuthorEmail)
	}
	configs := [][2]string{{"core.quotepath", "off"}, {"merge.conflictStyle", "merge"}}
	if o.Token != "" {
		// The helper reads the token from its own environment; the token never
		// appears on a command line or in a file.
		helper := `!f() { if [ "$1" = get ]; then echo "username=$KB_GIT_HELPER_USER"; echo "password=$KB_GIT_HELPER_TOKEN"; fi; }; f`
		configs = append(configs, [2]string{"credential.helper", helper})
		env = append(env, "KB_GIT_HELPER_TOKEN="+o.Token, "KB_GIT_HELPER_USER="+o.Username)
	}
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(configs)))
	for i, kv := range configs {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return env
}

// Error carries git's stderr.
type Error struct {
	Args   []string
	Stderr string
	Code   int
	Err    error
}

func (e *Error) Error() string {
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), strings.TrimSpace(redact(e.Stderr)))
}

func (e *Error) Unwrap() error { return e.Err }

func redact(s string) string {
	// Defensive: never echo anything that looks like a URL credential.
	if i := strings.Index(s, "://"); i >= 0 {
		if at := strings.Index(s[i:], "@"); at >= 0 {
			return s[:i+3] + "***" + s[i+at:]
		}
	}
	return s
}

func (r *Repo) run(ctx context.Context, args ...string) (string, error) {
	return r.runIn(ctx, r.Dir, args...)
}

func (r *Repo) runIn(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = r.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	r.log.Debug("git", "args", args, "ms", time.Since(start).Milliseconds(), "ok", err == nil)
	if err != nil {
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		return stdout.String(), &Error{Args: args, Stderr: stderr.String(), Code: code, Err: err}
	}
	return stdout.String(), nil
}

// Clone clones remote into dir on branch.
func Clone(ctx context.Context, remote, dir, branch string, opts Options) error {
	r := New(dir, opts)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	_, err := r.runIn(ctx, filepath.Dir(dir), "clone", "-q", "-b", branch, "--", remote, dir)
	return err
}

// IsWorkTree reports whether Dir is inside a Git work tree whose top level is Dir.
func (r *Repo) IsWorkTree(ctx context.Context) bool {
	out, err := r.run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return false
	}
	top, _ := filepath.EvalSymlinks(strings.TrimSpace(out))
	dir, _ := filepath.EvalSymlinks(r.Dir)
	return top == dir
}

// CurrentBranch returns the checked-out branch name.
func (r *Repo) CurrentBranch(ctx context.Context) (string, error) {
	out, err := r.run(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	return strings.TrimSpace(out), err
}

// Head returns the current commit hash ("" for an empty repository).
func (r *Repo) Head(ctx context.Context) (string, error) {
	out, err := r.run(ctx, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil {
		var ge *Error
		if errors.As(err, &ge) && ge.Code == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// RevParse resolves a revision to a full hash.
func (r *Repo) RevParse(ctx context.Context, rev string) (string, error) {
	if rev == "" || strings.HasPrefix(rev, "-") {
		return "", fmt.Errorf("%w: revision %q", ErrNotFound, rev)
	}
	out, err := r.run(ctx, "rev-parse", "--verify", "-q", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%w: revision %q", ErrNotFound, rev)
	}
	return strings.TrimSpace(out), nil
}

// HasRemote reports whether the named remote exists.
func (r *Repo) HasRemote(ctx context.Context, name string) bool {
	_, err := r.run(ctx, "remote", "get-url", name)
	return err == nil
}

// Stage stages the given paths (additions, modifications and deletions).
func (r *Repo) Stage(ctx context.Context, paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	args := append([]string{"add", "-A", "--"}, paths...)
	_, err := r.run(ctx, args...)
	return err
}

// StageAll stages everything.
func (r *Repo) StageAll(ctx context.Context) error {
	_, err := r.run(ctx, "add", "-A")
	return err
}

// Commit creates a commit from the index and returns its hash. It fails when
// there is nothing to commit unless allowEmpty is set.
func (r *Repo) Commit(ctx context.Context, message string) (string, error) {
	if _, err := r.run(ctx, "commit", "-q", "--no-verify", "-m", message); err != nil {
		return "", err
	}
	return r.Head(ctx)
}

// IsDirty reports whether the work tree or index has changes.
func (r *Repo) IsDirty(ctx context.Context) (bool, error) {
	out, err := r.run(ctx, "status", "--porcelain")
	return strings.TrimSpace(out) != "", err
}

// Fetch updates remote-tracking refs.
func (r *Repo) Fetch(ctx context.Context, remote string) error {
	_, err := r.run(ctx, "fetch", "-q", "--prune", remote)
	return err
}

// Merge merges ref into the current branch. conflicted is true when the merge
// stopped on conflicts and is now in progress.
func (r *Repo) Merge(ctx context.Context, ref string) (conflicted bool, err error) {
	_, err = r.run(ctx, "merge", "-q", "--no-edit", ref)
	if err == nil {
		return false, nil
	}
	if r.MergeInProgress(ctx) {
		return true, nil
	}
	return false, err
}

// MergeInProgress reports whether MERGE_HEAD exists.
func (r *Repo) MergeInProgress(ctx context.Context) bool {
	_, err := r.run(ctx, "rev-parse", "-q", "--verify", "MERGE_HEAD")
	return err == nil
}

// MergeHead returns the commit being merged, if any.
func (r *Repo) MergeHead(ctx context.Context) (string, error) {
	out, err := r.run(ctx, "rev-parse", "-q", "--verify", "MERGE_HEAD")
	return strings.TrimSpace(out), err
}

// Unmerged describes one conflicted path and which index stages exist.
type Unmerged struct {
	Path               string
	Base, Ours, Theirs bool // stage 1, 2, 3 present
}

// Kind classifies the conflict.
func (u Unmerged) Kind() string {
	switch {
	case u.Ours && u.Theirs && !u.Base:
		return "add/add"
	case u.Ours && !u.Theirs:
		return "modify/delete"
	case !u.Ours && u.Theirs:
		return "delete/modify"
	default:
		return "content"
	}
}

// UnmergedFiles lists conflicted paths from the index.
func (r *Repo) UnmergedFiles(ctx context.Context) ([]Unmerged, error) {
	out, err := r.run(ctx, "ls-files", "-u", "-z")
	if err != nil {
		return nil, err
	}
	byPath := map[string]*Unmerged{}
	var order []string
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		// "<mode> <hash> <stage>\t<path>"
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		fields := strings.Fields(rec[:tab])
		p := rec[tab+1:]
		u := byPath[p]
		if u == nil {
			u = &Unmerged{Path: p}
			byPath[p] = u
			order = append(order, p)
		}
		switch fields[2] {
		case "1":
			u.Base = true
		case "2":
			u.Ours = true
		case "3":
			u.Theirs = true
		}
	}
	res := make([]Unmerged, 0, len(order))
	for _, p := range order {
		res = append(res, *byPath[p])
	}
	return res, nil
}

// StageContent returns the content of path at index stage (1 base, 2 ours,
// 3 theirs). ok is false when that stage is absent.
func (r *Repo) StageContent(ctx context.Context, stage int, path string) (data []byte, ok bool, err error) {
	out, err := r.run(ctx, "show", fmt.Sprintf(":%d:%s", stage, path))
	if err != nil {
		return nil, false, nil
	}
	return []byte(out), true, nil
}

// CheckoutSide restores path from the given merge side ("ours"/"theirs").
func (r *Repo) CheckoutSide(ctx context.Context, side, path string) error {
	_, err := r.run(ctx, "checkout", "--"+side, "--", path)
	return err
}

// RemoveFromIndexAndTree deletes path from the index and work tree.
func (r *Repo) RemoveFromIndexAndTree(ctx context.Context, path string) error {
	_, err := r.run(ctx, "rm", "-q", "-f", "--", path)
	return err
}

// Push pushes branch to remote. Non-fast-forward rejections are reported as
// ErrPushRejected.
func (r *Repo) Push(ctx context.Context, remote, branch string) error {
	_, err := r.run(ctx, "push", "-q", remote, branch)
	if err == nil {
		return nil
	}
	var ge *Error
	if errors.As(err, &ge) {
		s := ge.Stderr
		if strings.Contains(s, "rejected") || strings.Contains(s, "fetch first") || strings.Contains(s, "non-fast-forward") {
			return fmt.Errorf("%w: %s", ErrPushRejected, strings.TrimSpace(redact(s)))
		}
	}
	return err
}

// AheadBehind counts commits ahead of and behind remote/branch.
func (r *Repo) AheadBehind(ctx context.Context, remote, branch string) (ahead, behind int, err error) {
	out, err := r.run(ctx, "rev-list", "--left-right", "--count", "HEAD..."+remote+"/"+branch)
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("unexpected rev-list output %q", out)
	}
	ahead, _ = strconv.Atoi(f[0])
	behind, _ = strconv.Atoi(f[1])
	return ahead, behind, nil
}

// Commit describes a history entry.
type Commit struct {
	Hash    string    `json:"hash"`
	Date    time.Time `json:"date"`
	Author  string    `json:"author"`
	Email   string    `json:"email,omitempty"`
	Message string    `json:"message"`
	Changes []Change  `json:"changes,omitempty"`
}

// Change is a path touched by a commit.
type Change struct {
	Status  string `json:"status"` // A M D R
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"`
}

// LogOptions filter history.
type LogOptions struct {
	Path   string // single path; enables --follow
	Folder string // restrict to a folder
	Limit  int
	Skip   int
	Since  string
	Rev    string // default HEAD
}

// Log lists commits.
func (r *Repo) Log(ctx context.Context, o LogOptions) ([]Commit, error) {
	args := []string{"log", "--format=%x1e%H%x1f%aI%x1f%an%x1f%ae%x1f%B%x1f", "--name-status"}
	if o.Limit > 0 {
		args = append(args, "-n", strconv.Itoa(o.Limit))
	}
	if o.Skip > 0 {
		args = append(args, "--skip", strconv.Itoa(o.Skip))
	}
	if o.Since != "" {
		args = append(args, "--since="+o.Since)
	}
	if o.Path != "" {
		args = append(args, "--follow")
	}
	rev := o.Rev
	if rev == "" {
		rev = "HEAD"
	}
	args = append(args, rev, "--")
	switch {
	case o.Path != "":
		args = append(args, o.Path)
	case o.Folder != "":
		args = append(args, o.Folder)
	}
	out, err := r.run(ctx, args...)
	if err != nil {
		var ge *Error
		if errors.As(err, &ge) && strings.Contains(ge.Stderr, "unknown revision") {
			return nil, fmt.Errorf("%w: revision %q", ErrNotFound, rev)
		}
		return nil, err
	}
	return parseLog(out), nil
}

func parseLog(out string) []Commit {
	var commits []Commit
	for _, rec := range strings.Split(out, "\x1e") {
		if strings.TrimSpace(rec) == "" {
			continue
		}
		f := strings.SplitN(rec, "\x1f", 6)
		if len(f) < 6 {
			continue
		}
		c := Commit{Hash: f[0], Author: f[2], Email: f[3], Message: strings.TrimSpace(f[4])}
		c.Date, _ = time.Parse(time.RFC3339, f[1])
		for _, line := range strings.Split(strings.TrimSpace(f[5]), "\n") {
			parts := strings.Split(line, "\t")
			if len(parts) < 2 || parts[0] == "" {
				continue
			}
			ch := Change{Status: parts[0][:1], Path: parts[1]}
			if (ch.Status == "R" || ch.Status == "C") && len(parts) >= 3 {
				ch.OldPath, ch.Path = parts[1], parts[2]
			}
			c.Changes = append(c.Changes, ch)
		}
		commits = append(commits, c)
	}
	return commits
}

// TreeEntry is a file or folder at a revision.
type TreeEntry struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // note | attachment | folder
	Size int64  `json:"size,omitempty"`
}

// LsTree lists the direct children of folder at rev.
func (r *Repo) LsTree(ctx context.Context, rev, folder string) ([]TreeEntry, error) {
	if _, err := r.RevParse(ctx, rev); err != nil {
		return nil, err
	}
	args := []string{"ls-tree", "-l", "-z", rev}
	if folder != "" {
		args = append(args, "--", strings.TrimSuffix(folder, "/")+"/")
	}
	out, err := r.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var entries []TreeEntry
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		meta := strings.Fields(rec[:tab])
		if len(meta) < 4 {
			continue
		}
		e := TreeEntry{Path: rec[tab+1:]}
		switch meta[1] {
		case "tree":
			e.Kind = "folder"
		default:
			e.Kind = "attachment"
			if strings.HasSuffix(strings.ToLower(e.Path), ".md") {
				e.Kind = "note"
			}
			e.Size, _ = strconv.ParseInt(meta[3], 10, 64)
		}
		entries = append(entries, e)
	}
	if folder != "" && len(entries) == 0 {
		return nil, fmt.Errorf("%w: %s at %s", ErrNotFound, folder, rev)
	}
	return entries, nil
}

// Show returns the content of path at rev.
func (r *Repo) Show(ctx context.Context, rev, path string) ([]byte, error) {
	hash, err := r.RevParse(ctx, rev)
	if err != nil {
		return nil, err
	}
	out, err := r.run(ctx, "show", hash+":"+path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s at %s", ErrNotFound, path, rev)
	}
	return []byte(out), nil
}

// Diff returns a unified diff between from and to (to == "" means the work
// tree), optionally limited to path.
func (r *Repo) Diff(ctx context.Context, from, to, path string) (string, error) {
	if _, err := r.RevParse(ctx, from); err != nil {
		return "", err
	}
	args := []string{"diff", "--no-color", from}
	if to != "" {
		if _, err := r.RevParse(ctx, to); err != nil {
			return "", err
		}
		args = append(args, to)
	}
	args = append(args, "--")
	if path != "" {
		args = append(args, path)
	}
	return r.run(ctx, args...)
}

// DiffNameStatus lists paths changed between two revisions.
func (r *Repo) DiffNameStatus(ctx context.Context, from, to string) ([]Change, error) {
	if from == "" {
		return nil, nil
	}
	out, err := r.run(ctx, "diff", "--name-status", "-z", "-M", from, to)
	if err != nil {
		return nil, err
	}
	var changes []Change
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		st := fields[i]
		if st == "" {
			continue
		}
		ch := Change{Status: st[:1]}
		if i+1 >= len(fields) {
			break
		}
		ch.Path = fields[i+1]
		i++
		if (ch.Status == "R" || ch.Status == "C") && i+1 < len(fields) {
			ch.OldPath, ch.Path = ch.Path, fields[i+1]
			i++
		}
		changes = append(changes, ch)
	}
	return changes, nil
}
