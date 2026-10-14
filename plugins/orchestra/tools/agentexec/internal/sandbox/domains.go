package sandbox

import (
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"regexp"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

// Domains the user approved (`sandbox allow-domain`) live in the learned
// store under their own `domains` key, never mixed with the path grants,
// and so under the same tamper protection.

var domainRe = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)*` +
	`[a-z0-9]([a-z0-9-]*[a-z0-9])?(:[0-9]{1,5})?$`)

// normDomain trims and ASCII-folds a domain for comparison.
func normDomain(s string) string { return config.FoldHost(strings.TrimSpace(s)) }

func isIP(text string) bool {
	host := text
	if strings.HasPrefix(host, "[") && strings.Contains(host, "]") {
		host = host[1:strings.Index(host, "]")]
	} else if strings.Count(host, ":") == 1 {
		host, _, _ = strings.Cut(host, ":")
	}
	_, err := netip.ParseAddr(host)
	return err == nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// DomainRefusal is why a domain grant is refused ("" = allowed): empty,
// the bare wildcard, IP literals, localhost, TLD wildcards, junk.
func DomainRefusal(domain string) string {
	if strings.TrimSpace(domain) == "" {
		return "refusing an empty domain"
	}
	text := normDomain(domain)
	if text == "*" || strings.HasPrefix(text, "*:") {
		return "refusing the wildcard *"
	}
	if isIP(text) {
		return "refusing an IP literal: " + text
	}
	if !domainRe.MatchString(text) {
		return "not a host name or *.wildcard: " + text
	}
	host := text
	if i := strings.LastIndex(text, ":"); i >= 0 {
		host = text[:i]
	}
	bare := strings.TrimPrefix(host, "*.")
	if bare == "localhost" || strings.HasSuffix(bare, ".localhost") {
		return "refusing localhost"
	}
	if strings.HasPrefix(host, "*.") && !strings.Contains(bare, ".") {
		return "refusing a top-level-domain wildcard: " + host
	}
	if allDigits(strings.ReplaceAll(host, ".", "")) {
		return "refusing an IP literal: " + text
	}
	return ""
}

func loadDomains(home string) ([]DomainGrant, string) {
	raw := rawStore(home)
	v, ok := raw["domains"]
	if !ok || v == nil {
		return []DomainGrant{}, ""
	}
	list, isList := v.([]any)
	if !isList {
		return []DomainGrant{}, fmt.Sprintf("learned domain grants corrupt (%s)", learnedPath(home))
	}
	good := []DomainGrant{}
	for _, e := range list {
		o, ok := e.(map[string]any)
		if !ok || o == nil {
			continue
		}
		d, ok := mustGet(o, "domain").(string)
		if ok && DomainRefusal(d) == "" {
			good = append(good, domainOf(maps.Clone(o)))
		}
	}
	warning := ""
	if n := len(list) - len(good); n != 0 {
		warning = fmt.Sprintf("learned domain grants: ignored %d invalid entr%s", n, plural(n))
	}
	return good, warning
}

// LearnedDomains are the approved domains, normalized and de-duplicated.
func LearnedDomains() []string { return learnedDomains("") }

func learnedDomains(home string) []string {
	ds, _ := loadDomains(home)
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = normDomain(d.Domain)
	}
	return dedupe(out)
}

// AddDomain persists one user domain grant (idempotent).
func AddDomain(d string) error {
	_, err := AddDomainEntry(d, "user", "")
	return err
}

// AddDomainEntry is AddDomain with its source (user, or who decided an
// approval) and reason; it returns the stored entry.
func AddDomainEntry(domain, source, reason string) (DomainGrant, error) {
	return addDomain(domain, source, reason, "")
}

func addDomain(domain, source, reason, home string) (DomainGrant, error) {
	if r := DomainRefusal(domain); r != "" {
		return DomainGrant{}, errors.New(r)
	}
	if strings.TrimSpace(source) == "" {
		return DomainGrant{}, errors.New("a domain grant needs a source")
	}
	text := normDomain(domain)
	unlock, err := lockStore(home)
	if err != nil {
		return DomainGrant{}, err
	}
	defer unlock()
	domains, _ := loadDomains(home)
	for _, e := range domains {
		if normDomain(e.Domain) == text {
			return e, nil
		}
	}
	entry := map[string]any{
		"domain": text,
		"added":  nowISOZ(),
		"source": strings.TrimSpace(source),
	}
	if r := strings.TrimSpace(reason); r != "" {
		entry["reason"] = r
	}
	list := make([]any, 0, len(domains)+1)
	for _, e := range domains {
		list = append(list, e.raw)
	}
	list = append(list, entry)
	if err := saveLearned(home, nil, list); err != nil {
		return DomainGrant{}, err
	}
	return domainOf(entry), nil
}

// ForgetDomain removes a domain grant; ErrNotFound if there was none.
func ForgetDomain(d string) error { return forgetDomain(d, "") }

func forgetDomain(domain, home string) error {
	text := normDomain(domain)
	unlock, err := lockStore(home)
	if err != nil {
		return err
	}
	defer unlock()
	domains, _ := loadDomains(home)
	kept := []any{}
	for _, e := range domains {
		if normDomain(e.Domain) != text {
			kept = append(kept, e.raw)
		}
	}
	if len(kept) == len(domains) {
		return ErrNotFound
	}
	return saveLearned(home, nil, kept)
}

// DomainMatches: host equals a pattern, or a *.suffix pattern matches a
// subdomain.
func DomainMatches(host string, patterns []string) bool {
	host = strings.TrimRight(normDomain(host), ".")
	for _, p := range patterns {
		if strings.HasPrefix(p, "*.") {
			if strings.HasSuffix(host, p[1:]) {
				return true
			}
		} else if host == p {
			return true
		}
	}
	return false
}

var srtBlockedRe = regexp.MustCompile(`Connection blocked to (\S+)`)

// ParseSrtDenial is the (host, port) of a `Connection blocked to
// <host>:<port>` srt debug line; port is -1 when absent. ok is false for
// any other line.
func ParseSrtDenial(line string) (host string, port int, ok bool) {
	m := srtBlockedRe.FindStringSubmatch(line)
	if m == nil {
		return "", 0, false
	}
	target := strings.TrimRight(m[1], ".,;")
	var portText string
	if strings.HasPrefix(target, "[") {
		h, rest, _ := strings.Cut(target[1:], "]")
		host = h
		if strings.HasPrefix(rest, ":") {
			portText = rest[1:]
		}
	} else {
		i := strings.LastIndex(target, ":")
		if i < 0 || !allDigits(target[i+1:]) {
			host, portText = target, ""
		} else {
			host, portText = target[:i], target[i+1:]
		}
	}
	if host == "" {
		return "", 0, false
	}
	port = -1
	if allDigits(portText) {
		fmt.Sscanf(portText, "%d", &port)
	}
	return host, port, true
}
