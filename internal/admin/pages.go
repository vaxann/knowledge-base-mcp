package admin

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/vaxann/knowledge-base-mcp/internal/acl"
	"github.com/vaxann/knowledge-base-mcp/internal/store"
	"github.com/vaxann/knowledge-base-mcp/internal/vault"
	"github.com/vaxann/knowledge-base-mcp/internal/workspace"
)

// ---- accounts ----

type userRow struct {
	ID         string
	Email      string
	Admin      bool
	Disabled   bool
	LastSignIn string
	Sessions   int
	Rules      int
}

func (p *Panel) usersPage(w http.ResponseWriter, _ *http.Request, s *session) {
	st := p.cfg.Store.Snapshot()
	sessions := p.cfg.OAuth.Sessions()
	rules := map[string]int{}
	for _, r := range st.Rules {
		rules[r.AccountID]++
	}
	var rows []userRow
	for _, a := range st.AccountList() {
		last := "never"
		if !a.LastSignIn.IsZero() {
			last = a.LastSignIn.UTC().Format("2006-01-02 15:04 UTC")
		}
		rows = append(rows, userRow{ID: a.ID, Email: a.Email, Admin: a.Admin, Disabled: a.Disabled, LastSignIn: last, Sessions: sessions[a.ID], Rules: rules[a.ID]})
	}
	p.render(w, s, "users", "Accounts", "users", rows)
}

func (p *Panel) userCreate(w http.ResponseWriter, r *http.Request, s *session) {
	email := r.PostForm.Get("email")
	var created *store.Account
	err := p.cfg.Store.Update(func(st *store.State) error {
		a, err := st.AddAccount(email, false, p.cfg.Now())
		created = a
		return err
	})
	if err != nil {
		p.back(w, r, s, "/admin/users", friendly(err), "")
		return
	}
	p.log.Info("admin: account created", "by", p.actor(s), "account", created.Email)
	p.back(w, r, s, "/admin/users", nil, created.Email+" can now sign in. It has no access until you grant some.")
}

// friendly drops the internal prefix of model errors.
func friendly(err error) error {
	msg := err.Error()
	for _, prefix := range []string{"invalid: ", "already exists: ", "not found: "} {
		if rest, ok := strings.CutPrefix(msg, prefix); ok {
			switch prefix {
			case "already exists: ":
				return errors.New(rest + " already exists")
			case "not found: ":
				return errors.New("no such " + rest)
			}
			return errors.New(rest)
		}
	}
	return err
}

func (p *Panel) userAction(w http.ResponseWriter, r *http.Request, s *session) {
	id, action := r.PathValue("id"), r.PathValue("action")
	target := p.cfg.Store.Snapshot().Accounts[id]
	if target == nil {
		p.back(w, r, s, "/admin/users", errors.New("no such account"), "")
		return
	}
	if target.Admin && (action == "disable" || action == "delete") {
		p.back(w, r, s, "/admin/users", errors.New("the administrator account is set in the server configuration and cannot be disabled or deleted here"), "")
		return
	}
	var err error
	msg := ""
	switch action {
	case "disable", "enable":
		disabled := action == "disable"
		err = p.cfg.Store.Update(func(st *store.State) error {
			a := st.Accounts[id]
			if a == nil {
				return errors.New("no such account")
			}
			a.Disabled = disabled
			return nil
		})
		if err == nil && disabled {
			p.cfg.OAuth.RevokeAccount(id)
			msg = target.Email + " is disabled and signed out everywhere."
		} else if err == nil {
			msg = target.Email + " can sign in again."
		}
	case "revoke":
		n := p.cfg.OAuth.RevokeAccount(id)
		msg = fmt.Sprintf("%d session(s) of %s ended.", n, target.Email)
	case "delete":
		if err = p.cfg.Store.Update(func(st *store.State) error { return st.DeleteAccount(id) }); err == nil {
			p.cfg.OAuth.RevokeAccount(id)
			msg = target.Email + " and its access rules were deleted."
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err == nil {
		p.log.Info("admin: account "+action, "by", p.actor(s), "account", target.Email)
	}
	p.back(w, r, s, "/admin/users", err, msg)
}

type ruleRow struct {
	RepoID  string
	Alias   string
	Folder  string // repository-relative, "" is the root
	Shown   string // as displayed: alias/folder
	Level   acl.Level
	Orphan  bool
	Missing bool // the repository itself is gone from the runtime
}

type userData struct {
	ID       string
	Email    string
	Admin    bool
	Disabled bool
	Rules    []ruleRow
	Repos    []string
}

func (p *Panel) userPage(w http.ResponseWriter, r *http.Request, s *session) {
	st := p.cfg.Store.Snapshot()
	a := st.Accounts[r.PathValue("id")]
	if a == nil {
		http.NotFound(w, r)
		return
	}
	d := userData{ID: a.ID, Email: a.Email, Admin: a.Admin, Disabled: a.Disabled}
	for _, rule := range st.Rules {
		if rule.AccountID != a.ID {
			continue
		}
		repo := st.Repos[rule.RepoID]
		if repo == nil {
			continue
		}
		row := ruleRow{RepoID: repo.ID, Alias: repo.Alias, Folder: rule.Path, Shown: shown(repo.Alias, rule.Path), Level: rule.Level}
		row.Orphan = !p.cfg.Workspace.FolderExists(repo.Alias, rule.Path)
		d.Rules = append(d.Rules, row)
	}
	sort.Slice(d.Rules, func(i, j int) bool { return d.Rules[i].Shown < d.Rules[j].Shown })
	for _, repo := range st.RepoList() {
		d.Repos = append(d.Repos, repo.Alias)
	}
	p.render(w, s, "user", a.Email, "users", d)
}

func shown(alias, folder string) string {
	if folder == "" {
		return alias
	}
	return alias + "/" + folder
}

// ---- repositories ----

func (p *Panel) reposPage(w http.ResponseWriter, r *http.Request, s *session) {
	p.render(w, s, "repos", "Repositories", "repos", p.cfg.Workspace.Repos(r.Context()))
}

func repoRequest(r *http.Request) workspace.AddRepoRequest {
	req := workspace.AddRepoRequest{Alias: r.PostForm.Get("alias"), URL: r.PostForm.Get("url"), Branch: r.PostForm.Get("branch")}
	switch r.PostForm.Get("credential") {
	case "generate":
		req.GenerateKey = true
	case "key":
		req.SSHKey = r.PostForm.Get("ssh_key")
	default:
		req.Token = r.PostForm.Get("token")
	}
	return req
}

func (p *Panel) repoCreate(w http.ResponseWriter, r *http.Request, s *session) {
	v, err := p.cfg.Workspace.AddRepo(r.Context(), repoRequest(r))
	if err != nil {
		p.back(w, r, s, "/admin/repos", err, "")
		return
	}
	p.log.Info("admin: repository added", "by", p.actor(s), "repository", v.Alias)
	msg := v.Alias + " was added and is being cloned. Nobody can see it until you grant access."
	if v.Status == workspace.StatusKeyPending {
		msg = v.Alias + " was added. Register its public key with the Git host, then connect."
	}
	p.back(w, r, s, "/admin/repos/"+url.PathEscape(v.Alias), nil, msg)
}

func (p *Panel) repoPage(w http.ResponseWriter, r *http.Request, s *session) {
	v, ok := p.cfg.Workspace.Repo(r.Context(), r.PathValue("alias"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	p.render(w, s, "repo", v.Alias, "repos", v)
}

func (p *Panel) repoAction(w http.ResponseWriter, r *http.Request, s *session) {
	alias, action := r.PathValue("alias"), r.PathValue("action")
	page := "/admin/repos/" + url.PathEscape(alias)
	if _, ok := p.cfg.Workspace.Repo(r.Context(), alias); !ok {
		p.back(w, r, s, "/admin/repos", errors.New("no such repository"), "")
		return
	}
	var err error
	msg := ""
	switch action {
	case "credential":
		if err = p.cfg.Workspace.ReplaceCredential(r.Context(), alias, repoRequest(r)); err == nil {
			msg = "The credential was replaced; the server is trying it now."
		}
	case "connect":
		if err = p.cfg.Workspace.Connect(alias); err == nil {
			msg = "Connecting to the remote."
		}
	case "sync":
		if err = p.cfg.Workspace.SyncRepo(r.Context(), alias); err == nil {
			msg = "Synchronised."
		} else {
			// The detail may quote file names; the status below says enough.
			p.log.Warn("admin: sync failed", "repository", alias, "err", err)
			err = errors.New("the synchronisation did not complete; see the status below")
		}
	case "remove":
		if r.PostForm.Get("confirm") != alias {
			err = errors.New("type the alias exactly to confirm the removal; nothing was removed")
			break
		}
		if err = p.cfg.Workspace.RemoveRepo(r.Context(), alias, r.PostForm.Get("discard") == "yes"); err == nil {
			p.log.Info("admin: repository removed", "by", p.actor(s), "repository", alias)
			p.back(w, r, s, "/admin/repos", nil, alias+" was removed from this server. The remote was not touched.")
			return
		}
		var ue *workspace.UnpushedError
		if errors.As(err, &ue) {
			err = fmt.Errorf("%s. Tick the box to remove it anyway", ue.Error())
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err == nil {
		p.log.Info("admin: repository "+action, "by", p.actor(s), "repository", alias)
	}
	p.back(w, r, s, page, err, msg)
}

// ---- access rules ----

type crumb struct {
	Name   string
	Folder string
}

type childFolder struct {
	Name     string
	Folder   string
	HasRules bool // a rule exists at or below this folder
}

type accessRow struct {
	AccountID string
	Email     string
	Admin     bool
	Disabled  bool
	Explicit  acl.Level // "" when the folder inherits
	Effective acl.Level
	// Source is where the effective level comes from, as displayed; "" when
	// no rule applies at all.
	Source string
}

type accessData struct {
	Alias    string
	Folder   string
	Shown    string
	Crumbs   []crumb
	Children []childFolder
	Rows     []accessRow
	Missing  bool // the folder does not exist (any more)
	Unready  string
}

func under(folder, p string) bool {
	return folder == "" || p == folder || strings.HasPrefix(p, folder+"/")
}

func (p *Panel) accessPage(w http.ResponseWriter, r *http.Request, s *session) {
	alias := r.PathValue("alias")
	st := p.cfg.Store.Snapshot()
	repo := st.RepoByAlias(alias)
	if repo == nil {
		http.NotFound(w, r)
		return
	}
	folder, err := vault.CleanRel(r.URL.Query().Get("folder"))
	if err != nil {
		http.Error(w, "invalid folder", http.StatusBadRequest)
		return
	}
	d := accessData{Alias: alias, Folder: folder, Shown: shown(alias, folder)}
	d.Crumbs = append(d.Crumbs, crumb{Name: alias})
	if folder != "" {
		acc := ""
		for _, seg := range strings.Split(folder, "/") {
			acc = strings.TrimPrefix(acc+"/"+seg, "/")
			d.Crumbs = append(d.Crumbs, crumb{Name: seg, Folder: acc})
		}
	}
	// Folder names come straight from the repository, whatever rules the
	// administrator holds itself; file names and content are never read.
	names, err := p.cfg.Workspace.ChildFolders(alias, folder)
	if err != nil {
		if v, ok := p.cfg.Workspace.Repo(r.Context(), alias); ok && v.Status != workspace.StatusOK && v.Status != workspace.StatusOffline && v.Status != workspace.StatusConflict {
			d.Unready = v.Status
		} else {
			d.Missing = true
		}
	}
	for _, name := range names {
		child := strings.TrimPrefix(folder+"/"+name, "/")
		c := childFolder{Name: name, Folder: child}
		for _, rule := range st.Rules {
			if rule.RepoID == repo.ID && under(child, rule.Path) {
				c.HasRules = true
			}
		}
		d.Children = append(d.Children, c)
	}
	for _, a := range st.AccountList() {
		rules := st.RulesFor(a.ID, repo.ID)
		row := accessRow{AccountID: a.ID, Email: a.Email, Admin: a.Admin, Disabled: a.Disabled, Effective: rules.Level(folder)}
		if m, ok := rules.Match(folder); ok {
			if m.Path == folder {
				row.Explicit = m.Level
			} else {
				row.Source = shown(alias, m.Path)
			}
		}
		d.Rows = append(d.Rows, row)
	}
	p.render(w, s, "access", "Access · "+d.Shown, "repos", d)
}

func (p *Panel) accessPost(w http.ResponseWriter, r *http.Request, s *session) {
	alias := r.PathValue("alias")
	folder, err := vault.CleanRel(r.PostForm.Get("folder"))
	if err != nil {
		http.Error(w, "invalid folder", http.StatusBadRequest)
		return
	}
	back := "/admin/access/" + url.PathEscape(alias) + "?folder=" + url.QueryEscape(folder)
	// Only a plain path inside the panel is accepted as a place to return to.
	if to := r.PostForm.Get("return"); strings.HasPrefix(to, "/admin/") && !strings.ContainsAny(to, "\\\r\n") && !strings.Contains(to, "//") {
		back = to
	}
	level := r.PostForm.Get("level")
	accountID := r.PostForm.Get("account")
	st := p.cfg.Store.Snapshot()
	repo, account := st.RepoByAlias(alias), st.Accounts[accountID]
	if repo == nil || account == nil {
		p.back(w, r, s, back, errors.New("no such repository or account"), "")
		return
	}
	if level != "inherit" && !p.cfg.Workspace.FolderExists(alias, folder) {
		p.back(w, r, s, back, fmt.Errorf("%s is not a folder of this repository", shown(alias, folder)), "")
		return
	}
	err = p.cfg.Store.Update(func(st *store.State) error {
		if level == "inherit" {
			st.ClearRule(accountID, repo.ID, folder)
			return nil
		}
		return st.SetRule(accountID, repo.ID, folder, acl.Level(level))
	})
	if err != nil {
		p.back(w, r, s, back, friendly(err), "")
		return
	}
	// Rules decide who reads what, so every change is on record.
	p.log.Info("admin: access rule changed", "by", p.actor(s), "account", account.Email, "repository", alias, "folder", folder, "level", level)
	msg := fmt.Sprintf("%s now has %s on %s.", account.Email, level, shown(alias, folder))
	if level == "inherit" {
		msg = fmt.Sprintf("%s inherits its level on %s again.", account.Email, shown(alias, folder))
	}
	p.back(w, r, s, back, nil, msg)
}
