// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package prsnapshot

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseRawDiffRejectsMalformedRecords(t *testing.T) {
	header := ":100644 100755 old-oid new-oid M"
	valid := header + "\x00valid.txt\x00"
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"unterminated header", header, "invalid record header"},
		{"missing field", ":100644 100755 old-oid M\x00file\x00", "invalid record header"},
		{"missing colon", "100644 100755 old-oid new-oid M\x00file\x00", "invalid record header"},
		{"missing status", ":100644 100755 old-oid new-oid\x00file\x00", "invalid record header"},
		{"missing path", header + "\x00", "missing path"},
		{"empty path", header + "\x00\x00", "missing path"},
		{"unterminated path", header + "\x00file", "missing path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, prefix := range []string{"", valid} {
				base, head, count, err := parseRawDiff(prefix + tc.raw)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("parseRawDiff error = %v, want %q", err, tc.want)
				}
				if base != nil || head != nil || count != 0 {
					t.Errorf("malformed diff returned partial results: %v, %v, %d", base, head, count)
				}
			}
		})
	}
}

func TestParseRawDiffRenameAndCopyDestinations(t *testing.T) {
	for _, status := range []string{"R100", "C075"} {
		t.Run(status, func(t *testing.T) {
			header := ":100644 100755 old-oid new-oid " + status + "\x00"
			base, head, count, err := parseRawDiff(header + "old space\tfile\x00new\nfile\x00" +
				":100644 100644 next-old next-new M\x00following\x00")
			if err != nil {
				t.Fatal(err)
			}
			wantBase := map[string]treeEntry{"old space\tfile": {"100644", "old-oid"}, "following": {"100644", "next-old"}}
			wantHead := map[string]treeEntry{"new\nfile": {"100755", "new-oid"}, "following": {"100644", "next-new"}}
			if count != 2 || !reflect.DeepEqual(base, wantBase) || !reflect.DeepEqual(head, wantHead) {
				t.Errorf("parseRawDiff = (%v, %v, %d), want (%v, %v, 2)", base, head, count, wantBase, wantHead)
			}
			for _, destination := range []string{"", "\x00", "unterminated"} {
				base, head, count, err := parseRawDiff(header + "old\x00" + destination)
				if err == nil || !strings.Contains(err.Error(), "missing destination") {
					t.Fatalf("destination %q: error = %v, want missing destination", destination, err)
				}
				if base != nil || head != nil || count != 0 {
					t.Errorf("destination %q returned partial results: %v, %v, %d", destination, base, head, count)
				}
			}
		})
	}
}

func TestParseTreeRejectsMalformedRecords(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"unterminated", "100644 blob oid\tfile", "unterminated record"},
		{"missing separator", "100644 blob oid file\x00", "invalid record"},
		{"missing metadata", "100644 oid\tfile\x00", "invalid record"},
		{"extra metadata", "100644 blob oid extra\tfile\x00", "invalid record"},
		{"empty path", "100644 blob oid\t\x00", "invalid record"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, prefix := range []string{"", "100644 blob valid-oid\tvalid\x00"} {
				entries, err := parseTree(prefix + tc.raw)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("parseTree error = %v, want %q", err, tc.want)
				}
				if entries != nil {
					t.Errorf("malformed tree returned partial entries: %v", entries)
				}
			}
		})
	}
}

func TestParseTreePreservesPathsAndModes(t *testing.T) {
	entries, err := parseTree("100755 blob first-oid\tspace\tand\nnewline\x00160000 commit second-oid\tmodule\x00")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]treeEntry{"space\tand\nnewline": {"100755", "first-oid"}, "module": {"160000", "second-oid"}}
	if !reflect.DeepEqual(entries, want) {
		t.Errorf("parseTree = %v, want %v", entries, want)
	}
}
