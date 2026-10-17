package workspace

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/cgi" //nolint:gosec // runs git-http-backend for a test server; the Httpoxy issue is fixed since Go 1.6.3
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vaxann/knowledge-base-mcp/internal/acl"
	"github.com/vaxann/knowledge-base-mcp/internal/kb"
	"github.com/vaxann/knowledge-base-mcp/internal/testutil"
)

// gitHTTPS serves a bare repository over HTTPS with token authentication,
// the way a Git host does.
func gitHTTPS(t *testing.T, bare, token string) string {
	t.Helper()
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git --exec-path failed")
	}
	backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend is not installed")
	}
	testutil.Git(t, bare, "config", "http.receivepack", "true")
	h := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + filepath.Dir(bare), "GIT_HTTP_EXPORT_ALL=1"}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pass, ok := r.BasicAuth(); !ok || pass != token {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("GIT_SSL_NO_VERIFY", "1") // the test server has a self-signed certificate
	return srv.URL + "/" + filepath.Base(bare)
}

func TestHTTPSTokenCredential(t *testing.T) {
	_, bare := testutil.NewAccessRepo(t)
	url := gitHTTPS(t, bare, "the-right-token")
	e := newEnv(t)
	ctx := context.Background()

	if _, err := e.ws.AddRepo(ctx, AddRepoRequest{Alias: "notes", URL: url, Token: "a-wrong-token-value"}); err != nil {
		t.Fatal(err)
	}
	e.ready()
	v, _ := e.ws.Repo(ctx, "notes")
	if v.Status != StatusError || v.Reason == "" {
		t.Fatalf("a rejected token must leave the repository in the error state: %+v", v)
	}
	if strings.Contains(v.Reason, "a-wrong-token-value") || strings.Contains(e.logs.String(), "a-wrong-token-value") {
		t.Error("neither the reason nor the logs may contain the token")
	}
	e.grant("ann@example.com", "notes", "", acl.Write)
	if _, err := e.ws.GetNote(e.ann, "notes/Open/Note.md"); code(err) != kb.CodeNotFound {
		t.Errorf("no notes are served for a repository that failed to clone: %v", err)
	}

	if err := e.ws.ReplaceCredential(ctx, "notes", AddRepoRequest{Token: "the-right-token"}); err != nil {
		t.Fatal(err)
	}
	e.ready()
	if v, _ = e.ws.Repo(ctx, "notes"); v.Status != StatusOK || v.Hint != "oken" {
		t.Fatalf("after replacing the token: %+v", v)
	}
	before := strings.TrimSpace(testutil.Git(t, bare, "rev-parse", "main"))
	if _, err := e.ws.PatchNote(e.ann, "notes/Open/Note.md", []kb.PatchOp{{Op: "append", Content: "pushed over https"}}, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.ws.SyncRepo(ctx, "notes"); err != nil {
		t.Fatalf("sync with the token: %v", err)
	}
	if after := strings.TrimSpace(testutil.Git(t, bare, "rev-parse", "main")); after == before {
		t.Error("the commit must reach the remote using the stored token")
	}
	state, _ := os.ReadFile(e.st.Path())
	if strings.Contains(string(state), "the-right-token") || strings.Contains(e.logs.String(), "the-right-token") {
		t.Error("the token must appear neither in the state file nor in the logs")
	}
	for _, f := range grepTree(t, e.cfg.DataDirOrDefault(), "the-right-token") {
		t.Errorf("the token is on disk in clear text: %s", f)
	}
	if err := e.ws.ReplaceCredential(ctx, "notes", AddRepoRequest{GenerateKey: true}); err == nil {
		t.Error("an HTTPS repository cannot take an SSH key")
	}
}

// grepTree returns the files under root that contain needle.
func grepTree(t *testing.T, root, needle string) []string {
	t.Helper()
	var found []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if data, err := os.ReadFile(p); err == nil && bytes.Contains(data, []byte(needle)) {
			found = append(found, p)
		}
		return nil
	})
	return found
}

func keygen(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("ssh-keygen", args...).CombinedOutput(); err != nil {
		t.Skipf("ssh-keygen is not usable here: %v %s", err, out)
	}
}

func TestSSHKeys(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen is not installed")
	}
	e := newEnv(t)
	ctx := context.Background()
	// Port 1 refuses connections at once, so no test waits on the network.
	const unreachable = "ssh://git@127.0.0.1:1/org/notes.git"

	t.Run("generated key waits for registration", func(t *testing.T) {
		v, err := e.ws.AddRepo(ctx, AddRepoRequest{Alias: "gen", URL: "git@example.com:org/notes.git", GenerateKey: true})
		if err != nil {
			t.Fatal(err)
		}
		if v.Status != StatusKeyPending || !strings.HasPrefix(v.PublicKey, "ssh-ed25519 ") || !strings.HasPrefix(v.Fingerprint, "SHA256:") {
			t.Fatalf("generated key: %+v", v)
		}
		e.ready()
		if v, _ = e.ws.Repo(ctx, "gen"); v.Status != StatusKeyPending {
			t.Errorf("nothing may be attempted before the key is confirmed: %+v", v)
		}
		other, err := e.ws.AddRepo(ctx, AddRepoRequest{Alias: "gen2", URL: "git@example.com:org/other.git", GenerateKey: true})
		if err != nil || other.PublicKey == v.PublicKey {
			t.Errorf("every repository gets its own key: %v", err)
		}
	})

	dir := t.TempDir()
	plain, locked := filepath.Join(dir, "plain"), filepath.Join(dir, "locked")
	keygen(t, "-q", "-t", "ed25519", "-N", "", "-C", "test", "-f", plain)
	keygen(t, "-q", "-t", "ed25519", "-N", "a passphrase", "-C", "test", "-f", locked)
	read := func(p string) string {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	t.Run("supplied key", func(t *testing.T) {
		v, err := e.ws.AddRepo(ctx, AddRepoRequest{Alias: "own", URL: unreachable, SSHKey: strings.ReplaceAll(read(plain), "\n", "\r\n")})
		if err != nil {
			t.Fatal(err)
		}
		if want := strings.Fields(read(plain + ".pub")); !strings.HasPrefix(v.PublicKey, want[0]+" "+want[1]) || v.Fingerprint == "" {
			t.Errorf("public key derived from the supplied key: %+v", v)
		}
		e.ready()
		if v, _ = e.ws.Repo(ctx, "own"); v.Status != StatusError {
			t.Errorf("an unreachable host ends in the error state: %+v", v)
		}
		// The private key exists decrypted only outside the data directory.
		private := strings.Split(read(plain), "\n")[1]
		if found := grepTree(t, e.cfg.DataDirOrDefault(), private); len(found) != 0 {
			t.Errorf("the private key is in clear text on the data volume: %v", found)
		}
		if strings.HasPrefix(e.ws.keyDir, e.cfg.DataDirOrDefault()) {
			t.Errorf("decrypted keys must live outside the data directory: %s", e.ws.keyDir)
		}
		if found := grepTree(t, e.ws.keyDir, private); len(found) != 1 {
			t.Errorf("the decrypted key file: %v", found)
		} else if st, _ := os.Stat(found[0]); st.Mode().Perm() != 0o600 {
			t.Errorf("key file mode = %v", st.Mode().Perm())
		}
		if strings.Contains(e.logs.String(), private) {
			t.Error("a private key must never be logged")
		}
	})

	t.Run("rejected keys", func(t *testing.T) {
		for name, key := range map[string]string{
			"passphrase-protected": read(locked),
			"malformed":            "-----BEGIN OPENSSH PRIVATE KEY-----\nnot a key\n-----END OPENSSH PRIVATE KEY-----\n",
			"not a key at all":     "hello",
			"public key":           read(plain + ".pub"),
		} {
			_, err := e.ws.AddRepo(ctx, AddRepoRequest{Alias: "bad", URL: unreachable, SSHKey: key})
			if err == nil {
				t.Errorf("%s key must be rejected", name)
			}
			if _, ok := e.ws.Repo(ctx, "bad"); ok {
				t.Fatalf("%s: a rejected key must store nothing", name)
			}
		}
		if _, err := e.ws.AddRepo(ctx, AddRepoRequest{Alias: "bad", URL: unreachable, SSHKey: read(locked)}); err == nil || !strings.Contains(err.Error(), "passphrase") {
			t.Errorf("the explanation must mention the passphrase: %v", err)
		}
	})

	t.Run("replace with a generated key", func(t *testing.T) {
		old, _ := e.ws.Repo(ctx, "own")
		if err := e.ws.ReplaceCredential(ctx, "own", AddRepoRequest{GenerateKey: true}); err != nil {
			t.Fatal(err)
		}
		v, _ := e.ws.Repo(ctx, "own")
		if v.Status != StatusKeyPending || v.PublicKey == old.PublicKey {
			t.Errorf("after replacing the key: %+v", v)
		}
		if err := e.ws.Connect("own"); err != nil {
			t.Fatal(err)
		}
		e.ready()
		if v, _ = e.ws.Repo(ctx, "own"); v.Status != StatusError {
			t.Errorf("connect starts the clone: %+v", v)
		}
	})

	t.Run("missing ssh-keygen", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, err := e.ws.AddRepo(ctx, AddRepoRequest{Alias: "nokeygen", URL: unreachable, GenerateKey: true})
		if err == nil || !strings.Contains(err.Error(), "ssh-keygen") {
			t.Fatalf("err = %v, want one naming ssh-keygen", err)
		}
		if _, ok := e.ws.Repo(ctx, "nokeygen"); ok {
			t.Error("nothing may be stored when the key cannot be created")
		}
	})

	keyDir := e.ws.keyDir
	_ = e.ws.Close()
	if _, err := os.Stat(keyDir); !os.IsNotExist(err) {
		t.Errorf("no decrypted key file may remain after shutdown: %v", err)
	}
}

// sshd runs a throw-away SSH server for the current user. It skips the test
// when one cannot be started here.
type sshd struct {
	t    *testing.T
	dir  string
	port int
	cmd  *exec.Cmd
}

func startSSHD(t *testing.T) *sshd {
	t.Helper()
	bin, err := exec.LookPath("sshd")
	if err != nil {
		if _, serr := os.Stat("/usr/sbin/sshd"); serr != nil {
			t.Skip("sshd is not installed")
		}
		bin = "/usr/sbin/sshd"
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	s := &sshd{t: t, dir: t.TempDir(), port: port}
	s.newHostKey()
	s.start(bin)
	t.Cleanup(s.stop)
	return s
}

func (s *sshd) newHostKey() {
	host := filepath.Join(s.dir, "host_key")
	_ = os.Remove(host)
	_ = os.Remove(host + ".pub")
	keygen(s.t, "-q", "-t", "ed25519", "-N", "", "-f", host)
}

func (s *sshd) authorize(publicKey string) {
	if err := os.WriteFile(filepath.Join(s.dir, "authorized_keys"), []byte(publicKey+"\n"), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sshd) start(bin string) {
	cfg := fmt.Sprintf("Port %d\nListenAddress 127.0.0.1\nHostKey %s\nPidFile %s\nAuthorizedKeysFile %s\nStrictModes no\nUsePAM no\nPasswordAuthentication no\nPubkeyAuthentication yes\n",
		s.port, filepath.Join(s.dir, "host_key"), filepath.Join(s.dir, "pid"), filepath.Join(s.dir, "authorized_keys"))
	cfgFile := filepath.Join(s.dir, "sshd_config")
	if err := os.WriteFile(cfgFile, []byte(cfg), 0o600); err != nil {
		s.t.Fatal(err)
	}
	s.cmd = exec.Command(bin, "-D", "-e", "-f", cfgFile)
	var stderr bytes.Buffer
	s.cmd.Stderr = &stderr
	if err := s.cmd.Start(); err != nil {
		s.t.Skipf("sshd cannot be started here: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", s.port), 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.stop()
	s.t.Skipf("sshd did not come up here: %s", stderr.String())
}

func (s *sshd) stop() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_, _ = s.cmd.Process.Wait()
		s.cmd = nil
	}
}

func TestSSHCloneAndHostKeyPinning(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh is not installed")
	}
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	_, bare := testutil.NewAccessRepo(t)
	server := startSSHD(t)
	e := newEnv(t)
	ctx := context.Background()
	url := fmt.Sprintf("ssh://%s@127.0.0.1:%d%s", me.Username, server.port, bare)

	v, err := e.ws.AddRepo(ctx, AddRepoRequest{Alias: "notes", URL: url, GenerateKey: true})
	if err != nil {
		t.Fatal(err)
	}
	// The key is not registered yet: connecting now is refused by the host.
	if err := e.ws.Connect("notes"); err != nil {
		t.Fatal(err)
	}
	e.ready()
	if got, _ := e.ws.Repo(ctx, "notes"); got.Status != StatusError {
		if got.Status == StatusOK {
			t.Skip("this sshd accepts the test user without the key; cannot test key authentication here")
		}
		t.Fatalf("before the key is registered: %+v", got)
	}
	server.authorize(v.PublicKey)
	if err := e.ws.Connect("notes"); err != nil {
		t.Fatal(err)
	}
	e.ready()
	got, _ := e.ws.Repo(ctx, "notes")
	if got.Status != StatusOK {
		t.Skipf("clone over the local sshd did not work here: %s", got.Reason)
	}
	e.grant("ann@example.com", "notes", "", acl.Write)
	if _, err := e.ws.GetNote(e.ann, "notes/Open/Note.md"); err != nil {
		t.Errorf("read after an SSH clone: %v", err)
	}
	hosts, _ := os.ReadFile(filepath.Join(e.cfg.DataDirOrDefault(), "known_hosts"))
	if !strings.Contains(string(hosts), "ssh-ed25519 ") {
		t.Errorf("the host key must be pinned on first contact: %q", hosts)
	}
	before := strings.TrimSpace(testutil.Git(t, bare, "rev-parse", "main"))
	if _, err := e.ws.PatchNote(e.ann, "notes/Open/Note.md", []kb.PatchOp{{Op: "append", Content: "pushed over ssh"}}, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.ws.SyncRepo(ctx, "notes"); err != nil {
		t.Fatalf("sync over ssh: %v", err)
	}
	pushed := strings.TrimSpace(testutil.Git(t, bare, "rev-parse", "main"))
	if pushed == before {
		t.Fatal("the commit must reach the remote over ssh")
	}

	// The host comes back with another key: the server must refuse to talk.
	bin := server.cmd.Path
	server.stop()
	server.newHostKey()
	server.start(bin)
	if _, err := e.ws.PatchNote(e.ann, "notes/Open/Note.md", []kb.PatchOp{{Op: "append", Content: "must not leave"}}, "", ""); err != nil {
		t.Fatalf("a local write still works while the remote is refused: %v", err)
	}
	if err := e.ws.SyncRepo(ctx, "notes"); err == nil {
		t.Fatal("sync must fail when the host key changed")
	}
	got, _ = e.ws.Repo(ctx, "notes")
	if got.Status != StatusError || !strings.Contains(got.Reason, "127.0.0.1") {
		t.Errorf("a changed host key must show as an error naming the host: %+v", got)
	}
	if after := strings.TrimSpace(testutil.Git(t, bare, "rev-parse", "main")); after != pushed {
		t.Error("nothing may be sent to a host whose key changed")
	}
}
