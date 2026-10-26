// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package prsnapshot

import (
	"context"
	"fmt"
	"strings"
)

type treeEntry struct {
	mode string
	oid  string
}

func (g snapshotGit) changedEntries(ctx context.Context, base, head string, paths []string) (map[string]treeEntry, map[string]treeEntry, int, error) {
	args := []string{"-c", "core.quotepath=false", "diff-tree", "-r", "--no-renames", "-z", "--no-commit-id", base, head, "--"}
	args = append(args, paths...)
	out, err := g.command(ctx, false, "", []string{"GIT_LITERAL_PATHSPECS=1"}, args...)
	if err != nil {
		return nil, nil, 0, err
	}
	return parseRawDiff(out)
}

func parseRawDiff(raw string) (map[string]treeEntry, map[string]treeEntry, int, error) {
	base, head := make(map[string]treeEntry), make(map[string]treeEntry)
	count := 0
	for raw != "" {
		header, rest, ok := strings.Cut(raw, "\x00")
		fields := strings.Fields(header)
		if !ok || len(fields) != 5 || !strings.HasPrefix(fields[0], ":") || fields[4] == "" {
			return nil, nil, 0, fmt.Errorf("parse diff-tree: invalid record header %q", header)
		}
		oldPath, rest, ok := strings.Cut(rest, "\x00")
		if !ok || oldPath == "" {
			return nil, nil, 0, fmt.Errorf("parse diff-tree: missing path for %q", header)
		}
		newPath := oldPath
		status := fields[4][0]
		if status == 'R' || status == 'C' {
			newPath, rest, ok = strings.Cut(rest, "\x00")
			if !ok || newPath == "" {
				return nil, nil, 0, fmt.Errorf("parse diff-tree: missing destination for %q", header)
			}
		}
		if status != 'A' {
			base[oldPath] = treeEntry{mode: strings.TrimPrefix(fields[0], ":"), oid: fields[2]}
		}
		if status != 'D' {
			head[newPath] = treeEntry{mode: fields[1], oid: fields[3]}
		}
		count++
		raw = rest
	}
	return base, head, count, nil
}

func parseTree(raw string) (map[string]treeEntry, error) {
	entries := make(map[string]treeEntry)
	for raw != "" {
		record, rest, ok := strings.Cut(raw, "\x00")
		if !ok {
			return nil, fmt.Errorf("parse ls-tree: unterminated record")
		}
		metadata, path, ok := strings.Cut(record, "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 || path == "" {
			return nil, fmt.Errorf("parse ls-tree: invalid record %q", record)
		}
		entries[path] = treeEntry{mode: fields[0], oid: fields[2]}
		raw = rest
	}
	return entries, nil
}

func (g snapshotGit) addContext(ctx context.Context, source string, paths []string, base, head map[string]treeEntry) (int, error) {
	count := 0
	for _, path := range paths {
		out, err := g.command(ctx, false, "", []string{"GIT_LITERAL_PATHSPECS=1"},
			"-c", "core.quotepath=false", "ls-tree", "-r", "-z", "--full-tree", source, "--", path)
		if err != nil {
			return 0, err
		}
		entries, err := parseTree(out)
		if err != nil {
			return 0, err
		}
		// A path below a symlinked directory cannot match literally. Adding the
		// shortest symlinked prefix, never a directory that would pull in a
		// whole tree, lets the caller follow the link in its next round.
		if len(entries) == 0 {
			parts := strings.Split(path, "/")
			var prefixes []string
			for i := 1; i < len(parts); i++ {
				prefixes = append(prefixes, strings.Join(parts[:i], "/"))
			}
			if len(prefixes) > 0 {
				args := []string{"-c", "core.quotepath=false", "ls-tree", "-z", "--full-tree", source, "--"}
				out, err := g.command(ctx, false, "", []string{"GIT_LITERAL_PATHSPECS=1"}, append(args, prefixes...)...)
				if err != nil {
					return 0, err
				}
				parents, err := parseTree(out)
				if err != nil {
					return 0, err
				}
				for _, prefix := range prefixes {
					if entry, ok := parents[prefix]; ok && entry.mode == "120000" {
						entries[prefix] = entry
						break
					}
				}
			}
		}
		for name, entry := range entries {
			if treePathConflict(name, base) || treePathConflict(name, head) {
				continue
			}
			base[name], head[name] = entry, entry
			count++
		}
	}
	return count, nil
}

func treePathConflict(name string, entries map[string]treeEntry) bool {
	for path := range entries {
		if name == path || strings.HasPrefix(name, path+"/") || strings.HasPrefix(path, name+"/") {
			return true
		}
	}
	return false
}
