package workspace

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Remote kinds.
const (
	remoteHTTPS = "https"
	remoteSSH   = "ssh"
	remoteLocal = "local" // tests only
)

// scpLike matches user@host:path, the short SSH form Git accepts.
var scpLike = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[A-Za-z0-9._~/-]+$`)

// parseRemote validates a repository URL entered by the administrator and
// returns its kind and host. The value ends up on a Git command line, so
// anything that is not plainly an HTTPS or SSH address is refused: other
// transports, embedded credentials, and values Git could read as an option.
func parseRemote(raw string, allowLocal bool) (kind, host string, err error) {
	bad := func(why string) (string, string, error) {
		return "", "", fmt.Errorf("repository URL %s: use https://host/owner/repo.git with a token, or ssh://git@host/owner/repo.git or git@host:owner/repo.git with an SSH key", why)
	}
	if raw == "" {
		return bad("is empty")
	}
	if len(raw) > 2048 || strings.ContainsAny(raw, " \t\r\n\x00") {
		return bad("contains spaces or is too long")
	}
	if strings.HasPrefix(raw, "-") {
		return bad("must not start with a dash")
	}
	if allowLocal && strings.HasPrefix(raw, "/") {
		return remoteLocal, "", nil
	}
	if strings.Contains(raw, "::") {
		return bad("uses an unsupported transport")
	}
	if strings.Contains(raw, "://") {
		u, perr := url.Parse(raw)
		if perr != nil || u.Host == "" {
			return bad("is not a valid URL")
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return bad("must not carry a query or fragment")
		}
		switch u.Scheme {
		case "https":
			if u.User != nil {
				return bad("must not contain credentials; enter the token separately")
			}
			return remoteHTTPS, u.Hostname(), nil
		case "ssh":
			if u.User != nil {
				if _, hasPassword := u.User.Password(); hasPassword {
					return bad("must not contain a password")
				}
			}
			if strings.Trim(u.Path, "/") == "" {
				return bad("has no repository path")
			}
			return remoteSSH, u.Hostname(), nil
		}
		return bad("uses the unsupported scheme " + u.Scheme)
	}
	if scpLike.MatchString(raw) {
		at, colon := strings.Index(raw, "@"), strings.Index(raw, ":")
		return remoteSSH, raw[at+1 : colon], nil
	}
	return bad("is not recognised")
}
