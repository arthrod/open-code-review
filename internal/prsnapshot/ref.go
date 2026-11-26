// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package prsnapshot

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var (
	repoNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	hostPattern     = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*(:[0-9]+)?$`)
	decimalPattern  = regexp.MustCompile(`^[0-9]+$`)
)

// DefaultHost is the host assumed when a reference names no host.
const DefaultHost = "github.com"

// RepoRef names a repository on a GitHub-compatible host. Every field has
// passed validation, so each one is safe to use as a single path component.
type RepoRef struct {
	Host  string
	Owner string
	Repo  string
}

// PullRef names one pull request.
type PullRef struct {
	RepoRef
	Number int
}

// ParseRepoRef accepts "OWNER/REPO", "HOST/OWNER/REPO" and
// "https://HOST/OWNER/REPO" (an optional ".git" suffix is dropped). A missing
// host means DefaultHost.
func ParseRepoRef(s string) (RepoRef, error) {
	if strings.Contains(s, "://") {
		u, err := parseRefURL(s)
		if err != nil {
			return RepoRef{}, err
		}
		if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return RepoRef{}, fmt.Errorf("repository reference must not contain a query or fragment")
		}
		parts := refURLParts(u)
		if len(parts) > 0 && parts[len(parts)-1] == "" {
			parts = parts[:len(parts)-1]
		}
		if len(parts) != 2 {
			return RepoRef{}, fmt.Errorf("repository URL must name an owner and repository")
		}
		return validatedRepoRef(u.Host, parts[0], parts[1])
	}
	parts := strings.Split(s, "/")
	switch len(parts) {
	case 2:
		return validatedRepoRef(DefaultHost, parts[0], parts[1])
	case 3:
		return validatedRepoRef(parts[0], parts[1], parts[2])
	default:
		return RepoRef{}, fmt.Errorf("repository reference must be OWNER/REPO or HOST/OWNER/REPO")
	}
}

// ParsePullRef accepts "OWNER/REPO#N", "HOST/OWNER/REPO#N" and
// "https://HOST/OWNER/REPO/pull/N" with any trailing path, query or fragment
// (for example ".../pull/7/files").
func ParsePullRef(s string) (PullRef, error) {
	var ref RepoRef
	var number string
	var err error
	if strings.Contains(s, "://") {
		u, parseErr := parseRefURL(s)
		if parseErr != nil {
			return PullRef{}, parseErr
		}
		parts := refURLParts(u)
		if len(parts) < 4 || parts[2] != "pull" {
			return PullRef{}, fmt.Errorf("pull request URL must name OWNER/REPO/pull/N")
		}
		ref, err = validatedRepoRef(u.Host, parts[0], parts[1])
		number = parts[3]
	} else {
		repo, n, ok := strings.Cut(s, "#")
		if !ok {
			return PullRef{}, fmt.Errorf("pull request reference must end with #N")
		}
		ref, err = ParseRepoRef(repo)
		number = n
	}
	if err != nil {
		return PullRef{}, fmt.Errorf("parse pull request repository: %w", err)
	}
	if !decimalPattern.MatchString(number) {
		return PullRef{}, fmt.Errorf("pull request number must be a positive decimal integer")
	}
	n, err := strconv.Atoi(number)
	if err != nil {
		return PullRef{}, fmt.Errorf("parse pull request number: %w", err)
	}
	if n <= 0 {
		return PullRef{}, fmt.Errorf("pull request number must be positive")
	}
	return PullRef{RepoRef: ref, Number: n}, nil
}

// String renders "OWNER/REPO" for DefaultHost and "HOST/OWNER/REPO" otherwise,
// which ParseRepoRef reads back unchanged.
func (r RepoRef) String() string {
	name := r.Owner + "/" + r.Repo
	if r.Host == DefaultHost {
		return name
	}
	return r.Host + "/" + name
}

// String renders the RepoRef form followed by "#N".
func (p PullRef) String() string {
	return p.RepoRef.String() + "#" + strconv.Itoa(p.Number)
}

// PathComponents returns the host, owner and repo as directory names. The host
// may carry a port; its ':' is replaced so the result is valid on every
// platform. Owner and repo are lowercased because GitHub ignores their case, so
// one repository never gets two directories.
func (r RepoRef) PathComponents() []string {
	return []string{safePathComponent(strings.ReplaceAll(r.Host, ":", "_")), safePathComponent(strings.ToLower(r.Owner)), safePathComponent(strings.ToLower(r.Repo))}
}

func parseRefURL(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("parse reference URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Opaque != "" {
		return nil, fmt.Errorf("reference URL must use HTTP or HTTPS without credentials")
	}
	return u, nil
}

func refURLParts(u *url.URL) []string {
	parts := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	for i := range parts {
		// Split before decoding so encoded separators cannot change boundaries.
		parts[i], _ = url.PathUnescape(parts[i])
	}
	return parts
}

func validatedRepoRef(host, owner, repo string) (RepoRef, error) {
	if !hostPattern.MatchString(host) {
		return RepoRef{}, fmt.Errorf("invalid repository host %q", host)
	}
	if hostname, port, ok := strings.Cut(host, ":"); ok {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return RepoRef{}, fmt.Errorf("invalid repository host port %q", port)
		}
		// Every URL built from the host is https, so 443 only splits one host
		// into two for token selection, the API root and state paths.
		if n == 443 {
			host = hostname
		}
	}
	repo = strings.TrimSuffix(repo, ".git")
	if !validRepoName(owner) || !validRepoName(repo) || strings.HasSuffix(repo, ".git") {
		return RepoRef{}, fmt.Errorf("invalid repository owner or name")
	}
	return RepoRef{Host: strings.ToLower(host), Owner: owner, Repo: repo}, nil
}

func validRepoName(name string) bool {
	return name != "." && name != ".." && repoNamePattern.MatchString(name)
}

func safePathComponent(name string) string {
	stem, _, _ := strings.Cut(strings.ToUpper(name), ".")
	reserved := stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL"
	if len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9' {
		reserved = true
	}
	// Windows reserves the stem whatever extension follows it, so the escape
	// has to change the stem itself. Escaping names that already start or end
	// like an escaped one keeps the mapping one to one: "CON" and "_CON", or
	// "x." and "x._", must not share a directory.
	if reserved || strings.HasPrefix(name, "_") {
		name = "_" + name
	}
	if strings.HasSuffix(strings.TrimRight(name, "_"), ".") {
		name += "_"
	}
	return name
}
