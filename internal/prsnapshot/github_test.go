// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package prsnapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

const ghPullJSON = `{"number":7,"title":"Fix parser","body":"Details\nSecond line","html_url":"https://github.com/o/r/pull/7","state":"open","draft":true,"updated_at":"2026-10-20T12:34:56Z","head":{"sha":"head-sha"},"base":{"sha":"base-sha","ref":"release/1.0","repo":{"clone_url":"https://github.com/o/r.git"}}}`

func ghExpectedPull() Pull {
	return Pull{
		Number: 7, Title: "Fix parser", Body: "Details\nSecond line",
		HTMLURL: "https://github.com/o/r/pull/7", State: "open", Draft: true,
		UpdatedAt: time.Date(2026, time.October, 20, 12, 34, 56, 0, time.UTC),
		HeadSHA:   "head-sha", BaseSHA: "base-sha", BaseRef: "release/1.0",
		CloneURL: "https://github.com/o/r.git",
	}
}

func ghNewClient(t *testing.T, baseURL, token string) *Client {
	t.Helper()
	client := NewClient(baseURL, token)
	if client == nil {
		t.Fatal("NewClient returned nil")
	}
	if client.HTTP == nil {
		t.Fatal("NewClient returned nil HTTP client")
	}
	return client
}

func ghNewServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	// Client construction must work before allocating the HTTP fixture.
	ghNewClient(t, "http://127.0.0.1", "")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for HTTP fixture: %v", err)
	}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler}}
	server.Start()
	t.Cleanup(server.Close)
	return server
}

func ghAssertHeaders(t *testing.T, r *http.Request, token string) {
	t.Helper()
	for key, want := range map[string]string{
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
		"User-Agent":           "open-code-review",
	} {
		if got := r.Header.Get(key); got != want {
			t.Errorf("header %s = %q, want %q", key, got, want)
		}
	}
	if token == "" {
		if values := r.Header.Values("Authorization"); len(values) != 0 {
			t.Errorf("anonymous request has Authorization: %q", values)
		}
	} else if got := r.Header.Get("Authorization"); got != "Bearer "+token {
		t.Errorf("Authorization = %q, want bearer token", got)
	}
}

func ghAssertQuery(t *testing.T, r *http.Request, want url.Values) {
	t.Helper()
	if got := r.URL.Query(); !reflect.DeepEqual(got, want) {
		t.Errorf("query = %v, want %v", got, want)
	}
}

func TestGetPullDecodesEveryField(t *testing.T) {
	var requests atomic.Int32
	server := ghNewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/api/v3/repos/o/r/pulls/7" {
			t.Errorf("request = %s %s, want GET /api/v3/repos/o/r/pulls/7", r.Method, r.URL.EscapedPath())
		}
		ghAssertQuery(t, r, url.Values{})
		ghAssertHeaders(t, r, "tok")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, ghPullJSON)
	}))
	client := ghNewClient(t, server.URL+"/api/v3/", "tok")
	pull, err := client.GetPull(t.Context(), "o", "r", 7)
	if err != nil {
		t.Fatalf("GetPull: %v", err)
	}
	if pull == nil {
		t.Fatal("GetPull returned nil pull")
	}
	if want := ghExpectedPull(); !reflect.DeepEqual(*pull, want) {
		t.Errorf("pull = %#v, want %#v", *pull, want)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

func TestGetPullBoundsResponseBody(t *testing.T) {
	previous := maxResponseBytes
	maxResponseBytes = 1024
	padding := int(maxResponseBytes) - len(ghPullJSON)
	t.Cleanup(func() { maxResponseBytes = previous })
	for _, fixture := range []string{"server", "transport"} {
		t.Run(fixture, func(t *testing.T) {
			for _, tc := range []struct {
				name      string
				body      string
				wantError bool
			}{
				{"under", ghPullJSON + strings.Repeat(" ", padding-1), false},
				{"exact", ghPullJSON + strings.Repeat(" ", padding), false},
				{"over", ghPullJSON + strings.Repeat(" ", padding+1), true},
				{"oversized JSON", `{"title":"` + strings.Repeat("x", int(maxResponseBytes)) + `"}`, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					baseURL := "https://api.github.com"
					if fixture == "server" {
						server := ghNewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							fmt.Fprint(w, tc.body)
						}))
						baseURL = server.URL
					}
					client := ghNewClient(t, baseURL, "secret-token-never-in-errors")
					if fixture == "transport" {
						client.HTTP.Transport = http.NewFileTransportFS(fstest.MapFS{"pull": &fstest.MapFile{Data: []byte(tc.body)}})
					}
					var pull pullResponse
					_, err := client.get(t.Context(), baseURL+"/pull?token="+client.Token, &pull)
					if tc.wantError {
						want := fmt.Sprintf("github: response from %s/pull exceeds %d bytes", baseURL, maxResponseBytes)
						if err == nil || err.Error() != want {
							t.Fatalf("get error = %v, want %q", err, want)
						}
					} else if err != nil || pull.Number != 7 {
						t.Fatalf("get = %#v, %v, want pull number 7", pull, err)
					}
				})
			}
		})
	}
}

func TestGetPullOmitsAnonymousAuthorization(t *testing.T) {
	var requests atomic.Int32
	server := ghNewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		ghAssertHeaders(t, r, "")
		fmt.Fprint(w, ghPullJSON)
	}))
	client := ghNewClient(t, server.URL, "")
	pull, err := client.GetPull(t.Context(), "o", "r", 7)
	if err != nil {
		t.Fatalf("GetPull: %v", err)
	}
	if pull == nil || pull.Number != 7 {
		t.Fatalf("pull = %#v, want number 7", pull)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

func TestGetPullEscapesOwnerAndRepository(t *testing.T) {
	var requests atomic.Int32
	server := ghNewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		want := "/repos/o%20space/r%23%3F/pulls/7"
		if r.URL.EscapedPath() != want || r.RequestURI != want {
			t.Errorf("escaped path = %q, RequestURI = %q, want %q", r.URL.EscapedPath(), r.RequestURI, want)
		}
		fmt.Fprint(w, ghPullJSON)
	}))
	client := ghNewClient(t, server.URL, "")
	if _, err := client.GetPull(t.Context(), "o space", "r#?", 7); err != nil {
		t.Fatalf("GetPull: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

func TestGetPullReturnsStructuredAPIError(t *testing.T) {
	server := ghNewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}))
	client := ghNewClient(t, server.URL, "secret-token-never-in-errors")
	_, err := client.GetPull(t.Context(), "o", "r", 7)
	if err == nil {
		t.Fatal("GetPull succeeded for a 404 response")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr == nil {
		t.Fatalf("error = %T %v, want *APIError", err, err)
	}
	wantURL := server.URL + "/repos/o/r/pulls/7"
	if apiErr.StatusCode != 404 || apiErr.Message != "Not Found" || apiErr.Method != "GET" || apiErr.URL != wantURL {
		t.Errorf("APIError = %#v, want 404 GET %s with message Not Found", apiErr, wantURL)
	}
	if !strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), client.Token) {
		t.Errorf("error must contain status and exclude token: %q", err.Error())
	}
}

func TestAPIErrorFormatsRequestAndStatus(t *testing.T) {
	err := &APIError{StatusCode: 404, Method: "GET", URL: "https://api.github.com/repos/o/r/pulls/7", Message: "Not Found"}
	want := "github: GET https://api.github.com/repos/o/r/pulls/7: 404 Not Found: Not Found"
	if got := err.Error(); got != want {
		t.Errorf("Error = %q, want %q", got, want)
	}
}

func TestMergeBaseReturnsCommit(t *testing.T) {
	var requests atomic.Int32
	server := ghNewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "GET" || r.URL.EscapedPath() != "/repos/o/r/compare/b...h" {
			t.Errorf("request = %s %s, want GET /repos/o/r/compare/b...h", r.Method, r.URL.EscapedPath())
		}
		ghAssertQuery(t, r, url.Values{"per_page": {"1"}})
		ghAssertHeaders(t, r, "tok")
		fmt.Fprint(w, `{"merge_base_commit":{"sha":"merge-sha"}}`)
	}))
	client := ghNewClient(t, server.URL, "tok")
	got, err := client.MergeBase(t.Context(), "o", "r", "b", "h")
	if err != nil {
		t.Fatalf("MergeBase: %v", err)
	}
	if got != "merge-sha" || requests.Load() != 1 {
		t.Errorf("MergeBase = %q, requests = %d, want merge-sha and 1", got, requests.Load())
	}
}

func TestMergeBasePreservesRefSlashesAndEscapesSegments(t *testing.T) {
	var requests atomic.Int32
	server := ghNewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		want := "/repos/o/r/compare/release/1.0...feature/x%20y"
		if got := r.URL.EscapedPath(); got != want {
			t.Errorf("EscapedPath = %q, want %q", got, want)
		}
		if wantURI := want + "?per_page=1"; r.RequestURI != wantURI {
			t.Errorf("RequestURI = %q, want %q", r.RequestURI, wantURI)
		}
		fmt.Fprint(w, `{"merge_base_commit":{"sha":"merge-sha"}}`)
	}))
	client := ghNewClient(t, server.URL, "")
	got, err := client.MergeBase(t.Context(), "o", "r", "release/1.0", "feature/x y")
	if err != nil {
		t.Fatalf("MergeBase: %v", err)
	}
	if got != "merge-sha" || requests.Load() != 1 {
		t.Errorf("MergeBase = %q, requests = %d, want merge-sha and 1", got, requests.Load())
	}
}

func TestMergeBaseRejectsMissingSHA(t *testing.T) {
	for _, body := range []string{`{}`, `{"merge_base_commit":{}}`, `{"merge_base_commit":{"sha":""}}`} {
		t.Run(body, func(t *testing.T) {
			var requests atomic.Int32
			server := ghNewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				fmt.Fprint(w, body)
			}))
			client := ghNewClient(t, server.URL, "")
			if got, err := client.MergeBase(t.Context(), "o", "r", "b", "h"); err == nil {
				t.Errorf("MergeBase = %q, want missing SHA error", got)
			}
			if got := requests.Load(); got != 1 {
				t.Errorf("requests = %d, want 1", got)
			}
		})
	}
}

func TestListOpenPullsFollowsNextLinkInOrder(t *testing.T) {
	var requests atomic.Int32
	server := ghNewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := requests.Add(1)
		ghAssertHeaders(t, r, "tok")
		if r.Method != "GET" || r.URL.Path != "/api/v3/repos/o/r/pulls" {
			t.Errorf("request = %s %s, want GET /api/v3/repos/o/r/pulls", r.Method, r.URL.Path)
		}
		wantQuery := url.Values{"state": {"open"}, "sort": {"updated"}, "direction": {"desc"}, "per_page": {"100"}}
		switch page {
		case 1:
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/api/v3/repos/o/r/pulls?page=2&state=open&sort=updated&direction=desc&per_page=100>; rel="next", <http://%s/api/v3/repos/o/r/pulls?page=2>; rel="last"`, r.Host, r.Host))
			fmt.Fprint(w, "["+ghPullJSON+"]")
		case 2:
			wantQuery.Set("page", "2")
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/api/v3/repos/o/r/pulls?page=1>; rel="prev"`, r.Host))
			fmt.Fprint(w, `[{"number":9,"title":"Older pull","state":"open"}]`)
		default:
			t.Errorf("unexpected page request %d", page)
			http.Error(w, "unexpected page", http.StatusBadRequest)
		}
		ghAssertQuery(t, r, wantQuery)
	}))
	client := ghNewClient(t, server.URL+"/api/v3", "tok")
	pulls, err := client.ListOpenPulls(t.Context(), "o", "r")
	if err != nil {
		t.Fatalf("ListOpenPulls: %v", err)
	}
	want := []Pull{ghExpectedPull(), {Number: 9, Title: "Older pull", State: "open"}}
	if !reflect.DeepEqual(pulls, want) {
		t.Errorf("pulls = %#v, want %#v", pulls, want)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
}

func TestListOpenPullsRejectsForeignNextLink(t *testing.T) {
	var foreignRequests atomic.Int32
	foreign := ghNewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignRequests.Add(1)
		fmt.Fprint(w, `[]`)
	}))
	var sourceRequests atomic.Int32
	server := ghNewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceRequests.Add(1)
		w.Header().Set("Link", "<"+foreign.URL+"/pulls?page=2>; rel=\"next\"")
		fmt.Fprint(w, `[]`)
	}))
	client := ghNewClient(t, server.URL, "secret-token")
	if _, err := client.ListOpenPulls(t.Context(), "o", "r"); err == nil {
		t.Error("ListOpenPulls accepted a foreign next link")
	}
	if got := sourceRequests.Load(); got != 1 {
		t.Errorf("source requests = %d, want 1", got)
	}
	if got := foreignRequests.Load(); got != 0 {
		t.Errorf("foreign server received %d requests, want 0", got)
	}
}

func TestListOpenPullsCapsPagination(t *testing.T) {
	var requests atomic.Int32
	server := ghNewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := requests.Add(1)
		if page > 100 {
			http.Error(w, "pagination exceeded cap", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<http://%s/repos/o/r/pulls?page=%d>; rel="next"`, r.Host, page+1))
		fmt.Fprint(w, `[]`)
	}))
	client := ghNewClient(t, server.URL, "")
	_, _ = client.ListOpenPulls(t.Context(), "o", "r")
	if got := requests.Load(); got != 100 {
		t.Errorf("requests = %d, want cap of 100", got)
	}
}

func TestClientRejectsForeignRedirectBeforeSending(t *testing.T) {
	var foreignRequests atomic.Int32
	foreign := ghNewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignRequests.Add(1)
		fmt.Fprint(w, ghPullJSON)
	}))
	var sourceRequests atomic.Int32
	server := ghNewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceRequests.Add(1)
		http.Redirect(w, r, foreign.URL+"/stolen", http.StatusFound)
	}))
	client := ghNewClient(t, server.URL, "secret-token")
	if _, err := client.GetPull(t.Context(), "o", "r", 7); err == nil {
		t.Error("GetPull accepted a foreign redirect")
	}
	if got := sourceRequests.Load(); got != 1 {
		t.Errorf("source requests = %d, want 1", got)
	}
	if got := foreignRequests.Load(); got != 0 {
		t.Errorf("foreign server received %d requests, want 0", got)
	}
}

func TestClientRedirectPolicyRejectsSchemeChange(t *testing.T) {
	client := ghNewClient(t, "http://example.test/api/v3", "tok")
	if client.HTTP.CheckRedirect == nil {
		t.Fatal("HTTP.CheckRedirect is nil")
	}
	initial, err := http.NewRequest("GET", "http://example.test/api/v3/repos/o/r/pulls/7", nil)
	if err != nil {
		t.Fatal(err)
	}
	redirect, err := http.NewRequest("GET", "https://example.test/api/v3/repos/o/r/pulls/7", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.HTTP.CheckRedirect(redirect, []*http.Request{initial}); err == nil {
		t.Error("redirect policy accepted a different scheme on the same host")
	}
}

func TestClientAllowsSameOriginRedirect(t *testing.T) {
	var requests atomic.Int32
	server := ghNewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		ghAssertHeaders(t, r, "tok")
		switch r.URL.Path {
		case "/repos/o/r/pulls/7":
			http.Redirect(w, r, "/redirected", http.StatusFound)
		case "/redirected":
			fmt.Fprint(w, ghPullJSON)
		default:
			http.NotFound(w, r)
		}
	}))
	client := ghNewClient(t, server.URL, "tok")
	pull, err := client.GetPull(t.Context(), "o", "r", 7)
	if err != nil {
		t.Fatalf("GetPull: %v", err)
	}
	if pull == nil || pull.Number != 7 {
		t.Errorf("pull = %#v, want number 7", pull)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
}

func TestAPIBaseForHost(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"github.com", "https://api.github.com"},
		{"GITHUB.COM", "https://api.github.com"},
		{"ghe.x", "https://ghe.x/api/v3"},
		{"ghe.local:8443", "https://ghe.local:8443/api/v3"},
		{"octo.ghe.com", "https://api.octo.ghe.com"},
		{"Octo.GHE.com", "https://api.Octo.GHE.com"},
		{"ghe.com.example", "https://ghe.com.example/api/v3"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			if got := APIBaseForHost(tc.host); got != tc.want {
				t.Errorf("APIBaseForHost = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewClientNormalizesBaseAndSetsTimeout(t *testing.T) {
	for _, suffix := range []string{"", "/", "///"} {
		t.Run("suffix="+suffix, func(t *testing.T) {
			client := ghNewClient(t, "https://ghe.example.com/api/v3"+suffix, "tok")
			if client.BaseURL != "https://ghe.example.com/api/v3" || client.Token != "tok" {
				t.Errorf("client BaseURL = %q, Token = %q", client.BaseURL, client.Token)
			}
			if client.HTTP.Timeout != 60*time.Second {
				t.Errorf("HTTP timeout = %v, want 60s", client.HTTP.Timeout)
			}
		})
	}
}

func TestResolveTokenPrecedenceAndTrimming(t *testing.T) {
	for _, tc := range []struct {
		name, ghEnv, githubEnv, cliToken string
		cliErr                           error
		want                             string
		wantCLICalls                     int
	}{
		{name: "GH_TOKEN wins", ghEnv: " \tgh-token\n", githubEnv: "github-token", cliToken: "cli-token", want: "gh-token"},
		{name: "GITHUB_TOKEN fallback", githubEnv: " \tgithub-token\n", cliToken: "cli-token", want: "github-token"},
		{name: "blank GH_TOKEN falls through", ghEnv: " \t\n", githubEnv: " github-token ", want: "github-token"},
		{name: "CLI fallback", cliToken: " \tcli-token\n", want: "cli-token", wantCLICalls: 1},
		{name: "blank env falls through", ghEnv: " \n", githubEnv: "\t ", cliToken: " cli-token ", want: "cli-token", wantCLICalls: 1},
		{name: "CLI error is anonymous", cliToken: "discard-on-error", cliErr: errors.New("gh unavailable"), wantCLICalls: 1},
		{name: "blank CLI is anonymous", cliToken: " \t\n", wantCLICalls: 1},
		{name: "empty CLI is anonymous", wantCLICalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"GH_TOKEN": tc.ghEnv, "GITHUB_TOKEN": tc.githubEnv}
			calls := 0
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			got := ResolveToken(ctx, "github.com", func(key string) string {
				return env[key]
			}, func(gotCtx context.Context, host string) (string, error) {
				calls++
				if gotCtx != ctx || host != "github.com" {
					t.Errorf("CLI arguments: context forwarded = %v, host = %q", gotCtx == ctx, host)
				}
				return tc.cliToken, tc.cliErr
			})
			if got != tc.want {
				t.Errorf("ResolveToken = %q, want %q", got, tc.want)
			}
			if calls != tc.wantCLICalls {
				t.Errorf("CLI calls = %d, want %d", calls, tc.wantCLICalls)
			}
		})
	}
}

func TestResolveTokenScopesEnvironmentTokensToTheirHosts(t *testing.T) {
	env := map[string]string{
		"GH_TOKEN": "dotcom-gh", "GITHUB_TOKEN": "dotcom-github",
		"GH_ENTERPRISE_TOKEN": "enterprise-gh", "GITHUB_ENTERPRISE_TOKEN": "enterprise-github",
	}
	for _, tc := range []struct {
		host, unset, want string
	}{
		{host: "github.com", want: "dotcom-gh"},
		{host: "acme.ghe.com", want: "dotcom-gh"},
		{host: "github.com", unset: "GH_TOKEN", want: "dotcom-github"},
		{host: "ghe.example.com:8443", want: "enterprise-gh"},
		{host: "ghe.example.com", unset: "GH_ENTERPRISE_TOKEN", want: "enterprise-github"},
		{host: "github.com.attacker.example", want: "enterprise-gh"},
		{host: "ghe.com.attacker.example", want: "enterprise-gh"},
	} {
		t.Run(tc.host+"-"+tc.unset, func(t *testing.T) {
			got := ResolveToken(t.Context(), tc.host, func(key string) string {
				if key == tc.unset {
					return ""
				}
				return env[key]
			}, func(context.Context, string) (string, error) {
				t.Error("gh was asked although an environment token applies")
				return "", nil
			})
			if got != tc.want {
				t.Errorf("ResolveToken(%q) = %q, want %q", tc.host, got, tc.want)
			}
		})
	}
	// Only gh itself knows which token belongs to another host.
	got := ResolveToken(t.Context(), "ghe.example.com", func(key string) string {
		if strings.Contains(key, "ENTERPRISE") {
			return ""
		}
		return env[key]
	}, func(_ context.Context, host string) (string, error) { return "gh-" + host, nil })
	if got != "gh-ghe.example.com" {
		t.Errorf("enterprise host without enterprise tokens = %q, want the gh token, never GH_TOKEN", got)
	}
}

func TestNonJSONServerErrorReturnsAPIError(t *testing.T) {
	server := ghNewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "upstream unavailable")
	}))
	client := ghNewClient(t, server.URL, "secret-token")
	_, err := client.GetPull(t.Context(), "o", "r", 7)
	if err == nil {
		t.Fatal("GetPull succeeded for non-JSON 500 response")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr == nil {
		t.Fatalf("error = %T %v, want *APIError", err, err)
	}
	if apiErr.StatusCode != 500 || apiErr.Method != "GET" || apiErr.URL != server.URL+"/repos/o/r/pulls/7" {
		t.Errorf("APIError = %#v, want 500 GET with request URL", apiErr)
	}
	if apiErr.Message != "" && !strings.HasPrefix("upstream unavailable", apiErr.Message) {
		t.Errorf("message = %q, want empty or raw response prefix", apiErr.Message)
	}
	if !strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), client.Token) {
		t.Errorf("error must contain status and exclude token: %q", err.Error())
	}
}

func TestAPIErrorURLExcludesQuery(t *testing.T) {
	server := ghNewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ghAssertQuery(t, r, url.Values{"per_page": {"1"}})
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"rate limited"}`)
	}))
	client := ghNewClient(t, server.URL, "secret-token")
	_, err := client.MergeBase(t.Context(), "o", "r", "b", "h")
	if err == nil {
		t.Fatal("MergeBase succeeded for 403 response")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr == nil {
		t.Fatalf("error = %T %v, want *APIError", err, err)
	}
	wantURL := server.URL + "/repos/o/r/compare/b...h"
	if apiErr.URL != wantURL || apiErr.StatusCode != 403 || apiErr.Message != "rate limited" {
		t.Errorf("APIError = %#v, want 403 with URL %q and rate limited message", apiErr, wantURL)
	}
	if strings.Contains(err.Error(), "?") || strings.Contains(err.Error(), client.Token) {
		t.Errorf("error contains query or token: %q", err.Error())
	}
}

func TestGetPullRejectsMalformedRequestURL(t *testing.T) {
	client := ghNewClient(t, "http://%invalid", "")
	pull, err := client.GetPull(t.Context(), "o", "r", 7)
	var urlErr *url.Error
	if pull != nil || !errors.As(err, &urlErr) || !strings.Contains(err.Error(), "create GitHub request") {
		t.Errorf("GetPull = (%+v, %v), want nil pull and wrapped URL error", pull, err)
	}
}

func TestGetPullRejectsTruncatedJSONResponse(t *testing.T) {
	server := ghNewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"number":`)
	}))
	client := ghNewClient(t, server.URL, "")
	pull, err := client.GetPull(t.Context(), "o", "r", 7)
	if pull != nil || !errors.Is(err, io.ErrUnexpectedEOF) || !strings.Contains(err.Error(), "decode GitHub response") {
		t.Errorf("GetPull = (%+v, %v), want nil pull and wrapped unexpected EOF", pull, err)
	}
}

func TestListOpenPullsDiscardsPartialResultsOnPageError(t *testing.T) {
	var requests atomic.Int32
	server := ghNewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/repos/o/r/pulls?page=2>; rel="next"`, r.Host))
			fmt.Fprint(w, "["+ghPullJSON+"]")
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"message":"retry later"}`)
	}))
	client := ghNewClient(t, server.URL, "")
	pulls, err := client.ListOpenPulls(t.Context(), "o", "r")
	var apiErr *APIError
	if pulls != nil || !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable || apiErr.Message != "retry later" {
		t.Errorf("ListOpenPulls = (%+v, %v), want nil pulls and page error", pulls, err)
	}
	if requests.Load() != 2 {
		t.Errorf("requests = %d, want 2", requests.Load())
	}
}

func TestCheckOriginRejectsMalformedAPIRoot(t *testing.T) {
	client := ghNewClient(t, "http://%invalid", "")
	target, err := url.Parse("http://fixture.test/repos/o/r/pulls")
	if err != nil {
		t.Fatal(err)
	}
	var urlErr *url.Error
	if err := client.checkOrigin(target); !errors.As(err, &urlErr) || !strings.Contains(err.Error(), "parse GitHub API root") {
		t.Errorf("checkOrigin = %v, want wrapped API root parsing error", err)
	}
}

func TestNextLinkIgnoresMalformedLinksAndOtherRelations(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []string
		want    string
	}{
		{"missing parameters", []string{`<https://api.github.com/page2>`}, ""},
		{"missing opening bracket", []string{`https://api.github.com/page2>; rel="next"`}, ""},
		{"missing closing bracket", []string{`<https://api.github.com/page2; rel="next"`}, ""},
		{"missing parameter value", []string{`<https://api.github.com/page2>; rel`}, ""},
		{"unrelated parameter", []string{`<https://api.github.com/page2>; type="next"`}, ""},
		{"other relation", []string{`<https://api.github.com/page2>; rel="last"`}, ""},
		{"multiple headers and relations", []string{`broken`, `<https://api.github.com/page2>; type="application/json"; rel="prev next"`}, "https://api.github.com/page2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextLink(tc.headers); got != tc.want {
				t.Errorf("nextLink = %q, want %q", got, tc.want)
			}
		})
	}
}

type ghCountingTransport struct{ requests atomic.Int32 }

func (c *ghCountingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.requests.Add(1)
	return nil, errors.New("transport reached")
}

func TestClientNeverSendsTokenInCleartext(t *testing.T) {
	for _, tc := range []struct {
		base, token string
		refused     bool
	}{
		{"http://ghe.example/api/v3", "secret-token", true},
		{"HTTP://GHE.EXAMPLE/api/v3", "secret-token", true},
		{"http://ghe.example/api/v3", "", false},
		{"http://127.0.0.1:9/api/v3", "secret-token", false},
		{"http://[::1]:9/api/v3", "secret-token", false},
		{"http://localhost:9/api/v3", "secret-token", false},
		{"https://ghe.example/api/v3", "secret-token", false},
	} {
		t.Run(tc.base+"/"+tc.token, func(t *testing.T) {
			client := NewClient(tc.base, tc.token)
			transport := &ghCountingTransport{}
			client.HTTP.Transport = transport
			_, err := client.GetPull(t.Context(), "o", "r", 7)
			if err == nil {
				t.Fatal("GetPull succeeded through a failing transport")
			}
			refused := strings.Contains(err.Error(), "plain HTTP")
			if refused != tc.refused || (transport.requests.Load() == 0) != tc.refused {
				t.Errorf("refused = %t after %d requests (%v), want refused = %t", refused, transport.requests.Load(), err, tc.refused)
			}
		})
	}
}
