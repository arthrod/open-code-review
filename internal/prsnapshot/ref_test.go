// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package prsnapshot

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseRepoRefAcceptedForms(t *testing.T) {
	cases := []struct {
		input string
		want  RepoRef
	}{
		{"o/r", RepoRef{"github.com", "o", "r"}},
		{"ghe.example.com/o/r", RepoRef{"ghe.example.com", "o", "r"}},
		{"https://github.com/o/r", RepoRef{"github.com", "o", "r"}},
		{"https://github.com/o/r.git", RepoRef{"github.com", "o", "r"}},
		{"o/r.git", RepoRef{"github.com", "o", "r"}},
		{"http://ghe.local:8080/o/r/", RepoRef{"ghe.local:8080", "o", "r"}},
		{"GitHub.com/o/r", RepoRef{"github.com", "o", "r"}},
		{"github.com:443/o/r", RepoRef{"github.com", "o", "r"}},
		{"https://github.com:443/o/r", RepoRef{"github.com", "o", "r"}},
		{"HOST:443/o/r", RepoRef{"host", "o", "r"}},
		{"ghe.example.com:8443/o/r", RepoRef{"ghe.example.com:8443", "o", "r"}},
		{"https://GHE.Example.COM:443/O_w-1/r.v2", RepoRef{"ghe.example.com", "O_w-1", "r.v2"}},
		{"a/o/r", RepoRef{"a", "o", "r"}},
		{"a-b.c9:1/o/r", RepoRef{"a-b.c9:1", "o", "r"}},
		{"host:65535/o/r", RepoRef{"host:65535", "o", "r"}},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := ParseRepoRef(tc.input)
			if err != nil {
				t.Fatalf("ParseRepoRef: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestParseRepoRefRejectsInvalidReferences(t *testing.T) {
	for _, input := range []string{
		"", "o", "o/r/x/y", "../r", "./r", "o/..", "o/.", "o/r r", "o o/r",
		"https://github.com/o", "ftp://github.com/o/r", "o/r#1", "o/r.git.git",
		"https://github.com/o/r.git.git", "o/.git", "https://github.com/o/r/extra",
		"o/r\\x", "https://github.com/o%2Fx/r", "https://github.com/o/r%2Fx",
	} {
		t.Run(input, func(t *testing.T) {
			if got, err := ParseRepoRef(input); err == nil {
				t.Errorf("ParseRepoRef(%q) = %#v, want error", input, got)
			}
		})
	}
}

func TestParseReferencesRejectInvalidHosts(t *testing.T) {
	for _, host := range []string{
		"..", ".", "a..b", ".a", "a.", "-a", "a-", "a.-b", "a-.b", "a_b",
		"host:0", "host:65536", "host:-1", "host:abc", "host:", "host:80:90", "[::1]",
	} {
		for _, prefix := range []string{"", "https://"} {
			input := prefix + host + "/o/r"
			t.Run(input, func(t *testing.T) {
				if got, err := ParseRepoRef(input); err == nil {
					t.Errorf("ParseRepoRef = %#v, want invalid host error", got)
				}
				pullInput := input + "#7"
				if prefix != "" {
					pullInput = input + "/pull/7"
				}
				if got, err := ParsePullRef(pullInput); err == nil {
					t.Errorf("ParsePullRef = %#v, want invalid host error", got)
				}
			})
		}
	}
}

func TestParsePullRefAcceptedForms(t *testing.T) {
	cases := []struct {
		input string
		want  PullRef
	}{
		{"o/r#7", PullRef{RepoRef{"github.com", "o", "r"}, 7}},
		{"https://github.com:443/o/r/pull/1", PullRef{RepoRef{"github.com", "o", "r"}, 1}},
		{"github.com:443/o/r#1", PullRef{RepoRef{"github.com", "o", "r"}, 1}},
		{"HOST:443/o/r#1", PullRef{RepoRef{"host", "o", "r"}, 1}},
		{"ghe.example.com:8443/o/r#1", PullRef{RepoRef{"ghe.example.com:8443", "o", "r"}, 1}},
		{"ghe.example.com/o/r#7", PullRef{RepoRef{"ghe.example.com", "o", "r"}, 7}},
		{"https://github.com/o/r/pull/7", PullRef{RepoRef{"github.com", "o", "r"}, 7}},
		{"https://github.com/o/r/pull/7/files", PullRef{RepoRef{"github.com", "o", "r"}, 7}},
		{"https://github.com/o/r/pull/7?x=1#discussion", PullRef{RepoRef{"github.com", "o", "r"}, 7}},
		{"http://GHE.local:8080/o/r/pull/7/commits?x=1#discussion", PullRef{RepoRef{"ghe.local:8080", "o", "r"}, 7}},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := ParsePullRef(tc.input)
			if err != nil {
				t.Fatalf("ParsePullRef: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestParsePullRefRejectsInvalidReferences(t *testing.T) {
	for _, input := range []string{
		"o/r", "o/r#0", "o/r#-1", "o/r#x", "o/r#1#2", "o/r#+7", "o/r#7.0", "o/r# 7",
		"o/r#999999999999999999999999999999", "../r#7", "o/..#7", "o/r r#7",
		"https://github.com/o/r/issues/7", "https://github.com/o/r/pull/",
		"https://github.com/o/r/pull/abc", "https://github.com/o/r/pull/0",
		"https://github.com/o/r/pull/-7", "https://github.com/o/r/pull/+7",
		"https://github.com/o/r/pull/7x", "ftp://github.com/o/r/pull/7",
	} {
		t.Run(input, func(t *testing.T) {
			if got, err := ParsePullRef(input); err == nil {
				t.Errorf("ParsePullRef(%q) = %#v, want error", input, got)
			}
		})
	}
}

func TestRepoRefStringRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		ref  RepoRef
		want string
	}{
		{RepoRef{"github.com", "o", "r"}, "o/r"},
		{RepoRef{"ghe.example.com", "o", "r"}, "ghe.example.com/o/r"},
		{RepoRef{"ghe.local:8080", "Owner", "repo-name"}, "ghe.local:8080/Owner/repo-name"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			got := tc.ref.String()
			if got != tc.want {
				t.Errorf("String = %q, want %q", got, tc.want)
			}
			parsed, err := ParseRepoRef(got)
			if err != nil {
				t.Fatalf("parse String: %v", err)
			}
			if parsed != tc.ref {
				t.Errorf("round trip = %#v, want %#v", parsed, tc.ref)
			}
		})
	}
}

func TestPullRefStringRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		ref  PullRef
		want string
	}{
		{PullRef{RepoRef{"github.com", "o", "r"}, 7}, "o/r#7"},
		{PullRef{RepoRef{"ghe.example.com", "o", "r"}, 7}, "ghe.example.com/o/r#7"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			got := tc.ref.String()
			if got != tc.want {
				t.Errorf("String = %q, want %q", got, tc.want)
			}
			parsed, err := ParsePullRef(got)
			if err != nil {
				t.Fatalf("parse String: %v", err)
			}
			if parsed != tc.ref {
				t.Errorf("round trip = %#v, want %#v", parsed, tc.ref)
			}
		})
	}
}

func TestRepoRefPathComponents(t *testing.T) {
	for _, tc := range []struct {
		ref  RepoRef
		want []string
	}{
		{RepoRef{"github.com", "o", "r"}, []string{"github.com", "o", "r"}},
		{RepoRef{"h:8080", "o", "r"}, []string{"h_8080", "o", "r"}},
		{RepoRef{"ghe.example.com", "O_w-1", "r.v2"}, []string{"ghe.example.com", "o_w-1", "r.v2"}},
		{RepoRef{"github.com", "owner.", "repo."}, []string{"github.com", "owner._", "repo._"}},
		{RepoRef{"host.", "owner", "repo"}, []string{"host._", "owner", "repo"}},
		{RepoRef{"github.com", "COM0", "LPT10"}, []string{"github.com", "com0", "lpt10"}},
		{RepoRef{"github.com", "CONifer", "auxiliary.txt"}, []string{"github.com", "conifer", "auxiliary.txt"}},
		{RepoRef{"github.com", "con.", "nul.tar.gz"}, []string{"github.com", "_con._", "_nul.tar.gz"}},
	} {
		t.Run(tc.ref.Host+"/"+tc.ref.Owner+"/"+tc.ref.Repo, func(t *testing.T) {
			if got := tc.ref.PathComponents(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("PathComponents = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRepoRefPathComponentsIgnoreOwnerAndRepositoryCase(t *testing.T) {
	ref, err := ParseRepoRef("Acme/Book")
	if err != nil {
		t.Fatal(err)
	}
	lower, err := ParseRepoRef("acme/book")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ref.PathComponents(), lower.PathComponents(); !reflect.DeepEqual(got, want) {
		t.Errorf("PathComponents = %q, want %q", got, want)
	}
	if ref.Owner != "Acme" || ref.Repo != "Book" || ref.String() != "Acme/Book" {
		t.Errorf("reference lost typed case: %#v (%s)", ref, ref.String())
	}
}

func TestRepoRefPathComponentsNeverCollide(t *testing.T) {
	names := []string{
		"CON", "_CON", "__CON", "con.txt", "_con.txt",
		"x", "_x", "__x", "x.", "x._", "x.__", "x_", "_x.", "_x._",
		"nul.", "_nul._", "a.b", "a.b_",
	}
	seen := make(map[string]string)
	for _, name := range names {
		got := RepoRef{"github.com", name, "repo"}.PathComponents()[1]
		if other, ok := seen[got]; ok {
			t.Errorf("%q and %q both map to %q", other, name, got)
		}
		seen[got] = name
	}
	for name, want := range map[string]string{"_CON": "__con", "_x": "__x", "x._": "x.__", "x_": "x_"} {
		if got := (RepoRef{"github.com", name, "repo"}).PathComponents()[1]; got != want {
			t.Errorf("PathComponents(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestRepoRefPathComponentsEscapeWindowsDevices(t *testing.T) {
	names := []string{"CON", "PRN", "AUX", "NUL"}
	for digit := '1'; digit <= '9'; digit++ {
		names = append(names, "COM"+string(digit), "LPT"+string(digit))
	}
	for _, name := range names {
		for _, component := range []string{name, strings.ToLower(name), strings.ToLower(name) + ".txt"} {
			for index := 0; index < 3; index++ {
				t.Run(component+"/"+[]string{"host", "owner", "repo"}[index], func(t *testing.T) {
					parts := []string{"github.com", "owner", "repo"}
					parts[index] = component
					ref := RepoRef{parts[0], parts[1], parts[2]}
					want := append([]string(nil), parts...)
					// Windows reserves the stem whatever follows it, so only a
					// prefix makes "nul.txt" usable.
					want[index] = "_" + want[index]
					if index > 0 {
						want[index] = strings.ToLower(want[index])
					}
					if got := ref.PathComponents(); !reflect.DeepEqual(got, want) {
						t.Errorf("PathComponents = %q, want %q", got, want)
					}
				})
			}
		}
	}
}
