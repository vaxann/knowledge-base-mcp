package kb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/gitrepo"
)

// SyncStatus is returned by kb_sync_status.
type SyncStatus struct {
	Branch    string    `json:"branch"`
	Remote    string    `json:"remote,omitempty"`
	State     string    `json:"state"`
	Ahead     int       `json:"ahead"`
	Behind    int       `json:"behind"`
	LastPull  time.Time `json:"last_pull,omitempty"`
	LastPush  time.Time `json:"last_push,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	Conflicts []string  `json:"conflicts"`
}

// pull fetches and merges the remote branch. It must be called with s.mu
// held. It returns true when the merge stopped on conflicts.
func (s *Service) pull(ctx context.Context) (conflicted bool, err error) {
	if s.remote == "" {
		return false, nil
	}
	if s.repo.MergeInProgress(ctx) {
		s.setState("conflict", "")
		return true, nil
	}
	if err := s.repo.Fetch(ctx, s.remote); err != nil {
		s.setState("offline", err.Error())
		return false, err
	}
	oldHead, _ := s.repo.Head(ctx)
	conflicted, err = s.repo.Merge(ctx, s.remote+"/"+s.cfg.Git.Branch)
	if err != nil {
		s.setState("offline", err.Error())
		return false, err
	}
	if conflicted {
		s.setState("conflict", "")
		s.log.Warn("merge conflict: writes are blocked until a client resolves it")
		return true, nil
	}
	newHead, _ := s.repo.Head(ctx)
	if oldHead != newHead {
		changes, err := s.repo.DiffNameStatus(ctx, oldHead, newHead)
		if err != nil {
			return false, err
		}
		if err := s.applyChanges(ctx, changes); err != nil {
			return false, err
		}
		s.log.Info("pulled", "changes", len(changes))
	}
	s.stateMu.Lock()
	s.lastPull = time.Now()
	s.stateMu.Unlock()
	s.setState("ok", "")
	return false, nil
}

// pullIfStale pulls unless a pull succeeded within the freshness window.
func (s *Service) pullIfStale(ctx context.Context) (bool, error) {
	s.stateMu.RLock()
	fresh := time.Since(s.lastPull) < s.cfg.Git.PullFreshness
	s.stateMu.RUnlock()
	if fresh && !s.repo.MergeInProgress(ctx) {
		return false, nil
	}
	return s.pull(ctx)
}

// push sends local commits. Must be called with s.mu held.
func (s *Service) push(ctx context.Context) error {
	if s.remote == "" || !s.cfg.Git.AutoPush {
		return nil
	}
	if s.repo.MergeInProgress(ctx) {
		return nil
	}
	ahead, _, err := s.repo.AheadBehind(ctx, s.remote, s.cfg.Git.Branch)
	if err == nil && ahead == 0 {
		return nil
	}
	err = s.repo.Push(ctx, s.remote, s.cfg.Git.Branch)
	if errors.Is(err, gitrepo.ErrPushRejected) {
		conflicted, perr := s.pull(ctx)
		if perr != nil {
			return perr
		}
		if conflicted {
			return nil
		}
		err = s.repo.Push(ctx, s.remote, s.cfg.Git.Branch)
	}
	if err != nil {
		s.setState("offline", err.Error())
		return err
	}
	s.stateMu.Lock()
	s.lastPush = time.Now()
	s.stateMu.Unlock()
	if s.State() == "offline" {
		s.setState("ok", "")
	}
	return nil
}

func (s *Service) requestPush() {
	select {
	case s.pushRequested <- struct{}{}:
	default:
	}
}

func (s *Service) pushLoop(ctx context.Context) {
	defer s.wg.Done()
	backoff := time.Second
	for {
		select {
		case <-s.stop:
			return
		case <-ctx.Done():
			return
		case <-s.pushRequested:
		}
		if d := s.cfg.Git.PushDebounce; d > 0 {
			select {
			case <-time.After(d):
			case <-s.stop:
				return
			case <-ctx.Done():
				return
			}
		}
		s.mu.Lock()
		err := s.push(ctx)
		s.mu.Unlock()
		if err != nil {
			s.log.Warn("push failed, will retry", "err", err, "in", backoff)
			go func(d time.Duration) {
				select {
				case <-time.After(d):
					s.requestPush()
				case <-s.stop:
				}
			}(backoff)
			if backoff < 5*time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}

func (s *Service) pullLoop(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(s.cfg.Git.PullInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			s.mu.Lock()
			if _, err := s.pull(ctx); err != nil {
				s.log.Warn("periodic pull failed", "err", err)
			}
			s.mu.Unlock()
			s.requestPush()
		}
	}
}

// Status reports synchronisation state.
func (s *Service) Status(ctx context.Context) (SyncStatus, error) {
	st := SyncStatus{Branch: s.cfg.Git.Branch, Remote: s.remote, Conflicts: []string{}}
	s.stateMu.RLock()
	st.State, st.LastError, st.LastPull, st.LastPush = s.state, s.lastErr, s.lastPull, s.lastPush
	s.stateMu.RUnlock()
	if s.remote != "" {
		if a, b, err := s.repo.AheadBehind(ctx, s.remote, s.cfg.Git.Branch); err == nil {
			st.Ahead, st.Behind = a, b
		}
	}
	if s.repo.MergeInProgress(ctx) {
		st.State = "conflict"
		un, _ := s.repo.UnmergedFiles(ctx)
		for _, u := range un {
			st.Conflicts = append(st.Conflicts, u.Path)
		}
	}
	return st, nil
}

// SyncNow pulls and pushes immediately.
func (s *Service) SyncNow(ctx context.Context) (SyncStatus, error) {
	if s.remote == "" {
		return SyncStatus{}, E(CodeNoRemote, "the vault has no 'origin' remote")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.repo.MergeInProgress(ctx) {
		return SyncStatus{}, s.mergeConflictError(ctx)
	}
	conflicted, err := s.pull(ctx)
	if err != nil {
		return SyncStatus{}, E(CodeInternal, "pull failed: %s", err)
	}
	if conflicted {
		return SyncStatus{}, s.mergeConflictError(ctx)
	}
	if err := s.push(ctx); err != nil {
		return SyncStatus{}, E(CodeInternal, "push failed: %s", err)
	}
	return s.Status(ctx)
}

// ---- conflicts ----

// Conflict describes one conflicted path.
type Conflict struct {
	Path    string  `json:"path"`
	Kind    string  `json:"kind"`
	Content *string `json:"content"` // work tree with markers; nil if absent
	Base    *string `json:"base"`
	Ours    *string `json:"ours"`
	Theirs  *string `json:"theirs"`
}

// ConflictReport is returned by kb_conflicts and inside merge_conflict errors.
type ConflictReport struct {
	Conflicts     []Conflict       `json:"conflicts"`
	RemoteCommits []gitrepo.Commit `json:"remote_commits,omitempty"`
}

// Conflicts lists the frozen merge, if any.
func (s *Service) Conflicts(ctx context.Context) (ConflictReport, error) {
	if !s.repo.MergeInProgress(ctx) {
		return ConflictReport{Conflicts: []Conflict{}}, nil
	}
	return s.conflictReport(ctx)
}

func (s *Service) conflictReport(ctx context.Context) (ConflictReport, error) {
	un, err := s.repo.UnmergedFiles(ctx)
	if err != nil {
		return ConflictReport{}, err
	}
	rep := ConflictReport{Conflicts: []Conflict{}}
	for _, u := range un {
		c := Conflict{Path: u.Path, Kind: u.Kind()}
		if data, _, err := s.v.Read(u.Path); err == nil {
			str := string(data)
			c.Content = &str
		}
		for stage, dst := range map[int]**string{1: &c.Base, 2: &c.Ours, 3: &c.Theirs} {
			if data, ok, _ := s.repo.StageContent(ctx, stage, u.Path); ok {
				str := string(data)
				*dst = &str
			}
		}
		rep.Conflicts = append(rep.Conflicts, c)
	}
	if commits, err := s.repo.Log(ctx, gitrepo.LogOptions{Rev: "HEAD..MERGE_HEAD", Limit: 50}); err == nil {
		rep.RemoteCommits = commits
	}
	return rep, nil
}

func (s *Service) mergeConflictError(ctx context.Context) *Error {
	rep, err := s.conflictReport(ctx)
	e := E(CodeMergeConflict, "a merge with the remote has conflicts; resolve them with kb_resolve_conflict before writing")
	if err == nil {
		e.With("conflicts", rep.Conflicts).With("remote_commits", rep.RemoteCommits)
	}
	return e
}

// Resolution is one entry of kb_resolve_conflict.
type Resolution struct {
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
	Take    string `json:"take,omitempty"` // ours | theirs
	Delete  bool   `json:"delete,omitempty"`
}

// ResolveResult reports what remains after a resolution call.
type ResolveResult struct {
	Resolved  []string `json:"resolved"`
	Remaining []string `json:"remaining"`
	Commit    string   `json:"commit,omitempty"`
	Complete  bool     `json:"complete"`
}

// HasConflictMarkers reports Git-style markers at line starts.
func HasConflictMarkers(content string) bool {
	for _, l := range strings.Split(content, "\n") {
		if strings.HasPrefix(l, "<<<<<<< ") || l == "=======" || strings.HasPrefix(l, ">>>>>>> ") {
			return true
		}
	}
	return false
}

// ResolveConflict applies client resolutions and completes the merge once
// every conflicted path is resolved.
func (s *Service) ResolveConflict(ctx context.Context, summary string, res []Resolution) (ResolveResult, error) {
	if s.ReadOnly() {
		return ResolveResult{}, E(CodeReadOnly, "the server runs in read-only mode")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.repo.MergeInProgress(ctx) {
		return ResolveResult{}, E(CodeNotInConflict, "no merge is in progress")
	}
	un, err := s.repo.UnmergedFiles(ctx)
	if err != nil {
		return ResolveResult{}, wrap(err)
	}
	unmerged := map[string]gitrepo.Unmerged{}
	for _, u := range un {
		unmerged[u.Path] = u
	}
	// Validate everything before touching anything.
	for _, r := range res {
		u, ok := unmerged[r.Path]
		if !ok {
			return ResolveResult{}, E(CodeNotFound, "%s is not a conflicted path", r.Path).With("remaining", keys(unmerged))
		}
		switch {
		case r.Delete:
		case r.Take == "ours" || r.Take == "theirs":
			if (r.Take == "ours" && !u.Ours) || (r.Take == "theirs" && !u.Theirs) {
				// that side deleted the file: taking it means deleting
			}
		case r.Take != "":
			return ResolveResult{}, E(CodeInvalidArgument, "take must be 'ours' or 'theirs' for %s", r.Path)
		case r.Content != "":
			if HasConflictMarkers(r.Content) {
				return ResolveResult{}, E(CodeMarkersPresent, "resolution for %s still contains conflict markers", r.Path)
			}
		default:
			return ResolveResult{}, E(CodeInvalidArgument, "resolution for %s needs content, take or delete", r.Path)
		}
	}
	out := ResolveResult{Resolved: []string{}, Remaining: []string{}}
	for _, r := range res {
		u := unmerged[r.Path]
		switch {
		case r.Delete, (r.Take == "ours" && !u.Ours), (r.Take == "theirs" && !u.Theirs):
			if err := s.repo.RemoveFromIndexAndTree(ctx, r.Path); err != nil {
				return out, wrap(err)
			}
		case r.Take != "":
			if err := s.repo.CheckoutSide(ctx, r.Take, r.Path); err != nil {
				return out, wrap(err)
			}
			if err := s.repo.Stage(ctx, r.Path); err != nil {
				return out, wrap(err)
			}
		default:
			if err := s.v.WriteAtomic(r.Path, []byte(r.Content)); err != nil {
				return out, wrap(err)
			}
			if err := s.repo.Stage(ctx, r.Path); err != nil {
				return out, wrap(err)
			}
		}
		out.Resolved = append(out.Resolved, r.Path)
		delete(unmerged, r.Path)
	}
	out.Remaining = keys(unmerged)
	if len(out.Remaining) > 0 {
		return out, nil
	}
	oldHead, _ := s.repo.Head(ctx)
	paths := make([]string, 0, len(un))
	for _, u := range un {
		paths = append(paths, u.Path)
	}
	msg := s.commitMessage(ctx, fmt.Sprintf("merge: %s/%s (resolved: %s)", s.remote, s.cfg.Git.Branch, strings.Join(paths, ", ")), summary)
	hash, err := s.repo.Commit(ctx, msg)
	if err != nil {
		return out, wrap(err)
	}
	out.Commit, out.Complete = hash, true
	changes, _ := s.repo.DiffNameStatus(ctx, oldHead, hash)
	if err := s.applyChanges(ctx, changes); err != nil {
		return out, wrap(err)
	}
	s.stateMu.Lock()
	s.lastPull = time.Now()
	s.stateMu.Unlock()
	s.setState("ok", "")
	s.requestPush()
	return out, nil
}

func keys(m map[string]gitrepo.Unmerged) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
