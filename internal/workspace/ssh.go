package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// errNoSSHKeygen explains a missing ssh-keygen binary.
var errNoSSHKeygen = errors.New("SSH keys need the ssh-keygen program, which is not installed on this server; use an HTTPS URL with a token instead")

func sshKeygen(ctx context.Context, args ...string) (string, error) {
	bin, err := exec.LookPath("ssh-keygen")
	if err != nil {
		return "", errNoSSHKeygen
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// stderr talks about the key file only by path, never its content.
		return "", fmt.Errorf("ssh-keygen: %s", strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// sshKey is a private key with its public half.
type sshKey struct {
	Private     []byte
	Public      string // one authorized_keys line
	Fingerprint string // SHA256:...
}

// scratch runs fn with a private temporary directory that is removed after.
func scratch(fn func(dir string) error) error {
	dir, err := os.MkdirTemp("", "kb-keygen-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	return fn(dir)
}

func fingerprintOf(ctx context.Context, pubFile string) (string, error) {
	out, err := sshKeygen(ctx, "-l", "-E", "sha256", "-f", pubFile)
	if err != nil {
		return "", err
	}
	f := strings.Fields(out)
	if len(f) < 2 {
		return "", errors.New("ssh-keygen: unexpected fingerprint output")
	}
	return f[1], nil
}

// generateSSHKey creates an ed25519 key pair for one repository.
func generateSSHKey(ctx context.Context, comment string) (*sshKey, error) {
	var k sshKey
	err := scratch(func(dir string) error {
		file := filepath.Join(dir, "key")
		if _, err := sshKeygen(ctx, "-q", "-t", "ed25519", "-N", "", "-C", comment, "-f", file); err != nil {
			return err
		}
		priv, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		pub, err := os.ReadFile(file + ".pub")
		if err != nil {
			return err
		}
		k.Private, k.Public = priv, strings.TrimSpace(string(pub))
		k.Fingerprint, err = fingerprintOf(ctx, file+".pub")
		return err
	})
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// inspectSSHKey validates a private key supplied by the administrator and
// derives its public half. Keys protected by a passphrase are refused: the
// server has to use the key unattended.
func inspectSSHKey(ctx context.Context, private string) (*sshKey, error) {
	private = strings.ReplaceAll(strings.TrimSpace(private), "\r\n", "\n") + "\n"
	if !strings.Contains(private, "PRIVATE KEY") {
		return nil, errors.New("this is not a private key: paste the whole file including its BEGIN and END lines")
	}
	k := sshKey{Private: []byte(private)}
	err := scratch(func(dir string) error {
		file := filepath.Join(dir, "key")
		if err := os.WriteFile(file, k.Private, 0o600); err != nil {
			return err
		}
		pub, err := sshKeygen(ctx, "-y", "-P", "", "-f", file)
		if err != nil {
			if errors.Is(err, errNoSSHKeygen) {
				return err
			}
			return errors.New("the private key cannot be used: it is malformed or protected by a passphrase (passphrase-protected keys are not supported)")
		}
		k.Public = strings.TrimSpace(pub)
		pubFile := file + ".pub"
		if err := os.WriteFile(pubFile, []byte(k.Public+"\n"), 0o600); err != nil {
			return err
		}
		k.Fingerprint, err = fingerprintOf(ctx, pubFile)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// hostKeyProblem recognises SSH's refusal to talk to a host whose key is
// unknown or has changed.
func hostKeyProblem(text string) bool {
	return strings.Contains(text, "HOST IDENTIFICATION HAS CHANGED") ||
		strings.Contains(text, "Host key verification failed") ||
		strings.Contains(text, "No ED25519 host key is known") ||
		strings.Contains(text, "host key is known for")
}
