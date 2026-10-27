// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package prsnapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// maxResponseBytes bounds a successful API response, far above a page of 100
// pull requests, so a misbehaving API root cannot exhaust memory.
var maxResponseBytes int64 = 64 << 20

// Pull is the subset of a GitHub pull request the review needs.
type Pull struct {
	Number    int
	Title     string
	Body      string
	HTMLURL   string
	State     string
	Draft     bool
	UpdatedAt time.Time
	HeadSHA   string
	BaseSHA   string
	BaseRef   string
	// CloneURL is the base repository's clone URL. A fork's head commit is
	// fetchable from it too, because GitHub keeps refs/pull/N/head there.
	CloneURL string
}

// Client is a minimal GitHub REST client.
type Client struct {
	// BaseURL is the API root without a trailing slash, for example
	// "https://api.github.com" or "https://ghe.example.com/api/v3".
	BaseURL string
	// Token is sent as a bearer token when non-empty.
	Token string
	HTTP  *http.Client
}

// APIError is returned for any non-2xx response. Message carries GitHub's
// "message" field when the body has one; the token never appears in it.
type APIError struct {
	StatusCode int
	Method     string
	URL        string
	Message    string
}

func (e *APIError) Error() string {
	message := fmt.Sprintf("github: %s %s: %d %s", e.Method, e.URL, e.StatusCode, http.StatusText(e.StatusCode))
	if e.Message != "" {
		message += ": " + e.Message
	}
	return message
}

// APIBaseForHost returns the REST root for host: https://api.github.com for
// github.com, https://api.HOST for GHE.com (data residency serves the API
// only from that subdomain), and https://HOST/api/v3 for GitHub Enterprise
// Server.
func APIBaseForHost(host string) string {
	switch lower := strings.ToLower(host); {
	case lower == DefaultHost:
		return "https://api.github.com"
	case strings.HasSuffix(lower, ".ghe.com"):
		return "https://api." + host
	}
	return "https://" + host + "/api/v3"
}

// NewClient returns a Client with a bounded default HTTP timeout.
func NewClient(baseURL, token string) *Client {
	c := &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token}
	c.HTTP = &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if err := c.checkOrigin(req.URL); err != nil {
				return err
			}
			if len(via) >= 10 {
				return fmt.Errorf("github: stopped after 10 redirects")
			}
			return nil
		},
	}
	return c
}

// GetPull fetches one pull request.
func (c *Client) GetPull(ctx context.Context, owner, repo string, number int) (*Pull, error) {
	var response pullResponse
	endpoint := c.repoURL(owner, repo) + "/pulls/" + strconv.Itoa(number)
	if _, err := c.get(ctx, endpoint, &response); err != nil {
		return nil, fmt.Errorf("get pull request: %w", err)
	}
	pull := response.pull()
	return &pull, nil
}

// MergeBase returns the merge-base commit of base and head as GitHub computes
// it for the pull request "Files changed" view. base and head may be commit
// ids or ref names; a '/' inside a ref name stays a path separator.
func (c *Client) MergeBase(ctx context.Context, owner, repo, base, head string) (string, error) {
	var response struct {
		MergeBaseCommit struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	endpoint := c.repoURL(owner, repo) + "/compare/" + escapeRef(base) + "..." + escapeRef(head) + "?per_page=1"
	if _, err := c.get(ctx, endpoint, &response); err != nil {
		return "", fmt.Errorf("get merge base: %w", err)
	}
	if response.MergeBaseCommit.SHA == "" {
		return "", fmt.Errorf("github: compare response has no merge-base SHA")
	}
	return response.MergeBaseCommit.SHA, nil
}

// ListOpenPulls lists every open pull request, most recently updated first,
// following the Link rel="next" pagination to the end.
func (c *Client) ListOpenPulls(ctx context.Context, owner, repo string) ([]Pull, error) {
	endpoint := c.repoURL(owner, repo) + "/pulls?state=open&sort=updated&direction=desc&per_page=100"
	var pulls []Pull
	for page := 0; page < 100; page++ {
		var response []pullResponse
		headers, err := c.get(ctx, endpoint, &response)
		if err != nil {
			return nil, fmt.Errorf("list open pull requests: %w", err)
		}
		for _, pull := range response {
			pulls = append(pulls, pull.pull())
		}
		endpoint = nextLink(headers.Values("Link"))
		if endpoint == "" {
			return pulls, nil
		}
	}
	return nil, fmt.Errorf("github: pull request pagination exceeded 100 pages")
}

// TokenEnvVars returns the token environment variables for host in precedence order.
func TokenEnvVars(host string) []string {
	if host == DefaultHost || strings.HasSuffix(host, ".ghe.com") {
		return []string{"GH_TOKEN", "GITHUB_TOKEN"}
	}
	return []string{"GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}
}

// ResolveToken returns the token for host the way gh picks it: $GH_TOKEN or
// $GITHUB_TOKEN for github.com and *.ghe.com, $GH_ENTERPRISE_TOKEN or
// $GITHUB_ENTERPRISE_TOKEN for any other host, then the output of
// `gh auth token --hostname HOST`; "" means anonymous access. An environment
// token is never sent to a host it was not issued for.
// lookupEnv and ghToken are injectable for tests; nil selects the real ones.
func ResolveToken(ctx context.Context, host string, lookupEnv func(string) string, ghToken func(context.Context, string) (string, error)) string {
	if lookupEnv == nil {
		lookupEnv = os.Getenv
	}
	for _, key := range TokenEnvVars(host) {
		if token := strings.TrimSpace(lookupEnv(key)); token != "" {
			return token
		}
	}
	if ghToken == nil {
		ghToken = readGHToken
	}
	token, err := ghToken(ctx, host)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(token)
}

type pullResponse struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	HTMLURL   string    `json:"html_url"`
	State     string    `json:"state"`
	Draft     bool      `json:"draft"`
	UpdatedAt time.Time `json:"updated_at"`
	Head      struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo struct {
			CloneURL string `json:"clone_url"`
		} `json:"repo"`
	} `json:"base"`
}

func (p pullResponse) pull() Pull {
	return Pull{
		Number: p.Number, Title: p.Title, Body: p.Body, HTMLURL: p.HTMLURL,
		State: p.State, Draft: p.Draft, UpdatedAt: p.UpdatedAt,
		HeadSHA: p.Head.SHA, BaseSHA: p.Base.SHA, BaseRef: p.Base.Ref,
		CloneURL: p.Base.Repo.CloneURL,
	}
}

func (c *Client) repoURL(owner, repo string) string {
	return c.BaseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func (c *Client) checkOrigin(target *url.URL) error {
	base, err := url.Parse(c.BaseURL)
	if err != nil {
		return fmt.Errorf("parse GitHub API root: %w", err)
	}
	if !target.IsAbs() || target.Host == "" || target.User != nil ||
		!strings.EqualFold(target.Scheme, base.Scheme) || !strings.EqualFold(target.Host, base.Host) {
		return fmt.Errorf("github: refusing request outside API origin")
	}
	return nil
}

func loopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
}

func (c *Client) get(ctx context.Context, endpoint string, result any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create GitHub request: %w", err)
	}
	if err := c.checkOrigin(req.URL); err != nil {
		return nil, err
	}
	if c.Token != "" && strings.EqualFold(req.URL.Scheme, "http") && !loopbackHost(req.URL.Hostname()) {
		return nil, fmt.Errorf("github: refusing to send a token over plain HTTP to %s", req.URL.Host)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "open-code-review")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	response, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send GitHub request: %w", err)
	}
	defer response.Body.Close()
	requestURL := *req.URL
	requestURL.RawQuery = ""
	requestURL.ForceQuery = false
	requestURL.Fragment = ""
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var body struct {
			Message string `json:"message"`
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body); err != nil {
			body.Message = ""
		}
		if c.Token != "" {
			body.Message = strings.ReplaceAll(body.Message, c.Token, "[redacted]")
		}
		return nil, &APIError{StatusCode: response.StatusCode, Method: req.Method, URL: requestURL.String(), Message: body.Message}
	}
	body := &io.LimitedReader{R: response.Body, N: maxResponseBytes + 1}
	decodeErr := json.NewDecoder(body).Decode(result)
	_, readErr := io.Copy(io.Discard, body)
	if body.N == 0 {
		return nil, fmt.Errorf("github: response from %s exceeds %d bytes", requestURL.String(), maxResponseBytes)
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("decode GitHub response: %w", decodeErr)
	}
	if readErr != nil {
		return nil, fmt.Errorf("read GitHub response: %w", readErr)
	}
	return response.Header, nil
}

func nextLink(headers []string) string {
	for _, header := range headers {
		for _, link := range strings.Split(header, ",") {
			parts := strings.Split(strings.TrimSpace(link), ";")
			if len(parts) < 2 || !strings.HasPrefix(parts[0], "<") || !strings.HasSuffix(parts[0], ">") {
				continue
			}
			for _, param := range parts[1:] {
				key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
				if !ok || key != "rel" {
					continue
				}
				for _, rel := range strings.Fields(strings.Trim(value, `"`)) {
					if rel == "next" {
						return strings.TrimSuffix(strings.TrimPrefix(parts[0], "<"), ">")
					}
				}
			}
		}
	}
	return ""
}

func readGHToken(ctx context.Context, host string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", host).Output()
	if err != nil {
		return "", fmt.Errorf("read gh authentication token: %w", err)
	}
	return string(output), nil
}
