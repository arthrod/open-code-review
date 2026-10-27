# Reviewing a pull request without cloning: `ocr review-pr`

Some repositories take a very long time to clone, while each of their pull requests is
self-contained: a chapter, a module, a document that never depends on the rest of the tree.
`ocr review-pr` reviews such a pull request without ever cloning the repository. It runs the
same review engine as `ocr review`, with every review flag available. To keep reviewing new
pull request commits continuously, see [`ocr listen`](listen.md).

## How it avoids the clone

1. The GitHub REST API resolves the pull request: head commit, title, body, and the merge
   base with the target branch, which is what the pull request's "Files changed" view
   compares against.
2. A scratch repository fetches only those two commits: shallow (`--depth=1`), with trees but
   without a single file content (`--filter=blob:none`). No history is downloaded.
3. `git diff-tree` lists the changed files, then one more fetch downloads exactly their
   contents, both sides.
4. A synthetic two-commit history is written from those files alone: a base commit with the
   old side of every changed file and a head commit with the new side. The commits are
   deterministic: the same pull request contents always give the same commit ids.
5. The regular range review runs on base..head. Everything the reviewer can read, search or
   list is the pull request's own files, so the review treats the pull request as the
   independent unit it is. The scratch repository forgets where it came from once it is
   built, so a file that was not downloaded is an error, never a download.

The work tree holds the synthetic base commit, never the head: project rules, `.gitignore`
and anything else read from disk (MCP servers included) see the target branch only, so a
pull request cannot rewrite the rules it is reviewed by. `.opencodereview` (with the rule
files its `rule.json` names, even through symlinks), `.gitignore` and `.gitattributes` are
always added from the merge base. Use `--context` to make other unchanged files readable.

## Usage

```bash
# A GitHub pull request, by reference or by URL
ocr review-pr alibaba/open-code-review#1773
ocr review-pr https://github.com/alibaba/open-code-review/pull/1773

# Only the changed files under one directory, with that directory readable as context
ocr review-pr acme/book#42 --path chapters/07 --context chapters/07

# Every review flag works as with `ocr review`
ocr review-pr acme/book#42 --format json --output review.json --effort high

# Any git host, without an API: --base is used as the base commit as is
ocr review-pr --remote https://git.example.com/acme/book.git --base 1a2b3c --head feature/ch7
```

| Flag | Meaning |
|------|---------|
| `--path p1,p2` | Review only the changed files under these repository-relative paths. |
| `--context p1,p2` | Unchanged files or directories, read at the merge base, added identically to both commits: readable by the reviewer, never part of the diff. |
| `--workdir DIR` | Scratch directory. Default: `~/.opencodereview/pr/<host>/<owner>/<repo>/<number>`. |
| `--cleanup` | Remove the scratch directory after the review, whether it succeeded or failed. |
| `--head REV` | PR mode: review this head commit instead of the current one. Generic mode: required. |
| `--base REV` | PR mode: use this base instead of the merge base. Generic mode: required. |
| `--remote URL` | PR mode: fetch from this URL (for example an SSH URL) instead of the API's clone URL. Generic mode: required. A URL carrying credentials (`https://token@host/...`) is refused, since git would keep it in the snapshot's `.git/config`; use a credential helper instead. |
| `--github-api URL` | REST API root. Default: `https://api.github.com`, `https://api.HOST` for GHE.com, or `https://HOST/api/v3` for GitHub Enterprise Server. |

`--repo`, `--from`, `--to` and `--commit` are decided by the snapshot and cannot be set.
Without `--background` or `--background-file`, the pull request's title and body are used
as background (capped at 8000 characters).

### Authentication

The token is chosen the way `gh` chooses it: `$GH_TOKEN` or `$GITHUB_TOKEN` for github.com
and `*.ghe.com`, `$GH_ENTERPRISE_TOKEN` or `$GITHUB_ENTERPRISE_TOKEN` for any other host,
then `gh auth token --hostname <host>`. By default the token only goes to the host it was
issued for. An API root named with `--github-api` is trusted with the token of the pull
request's host, as a proxy in front of GitHub Enterprise Server needs it, so name only an
API root you trust. No token is ever sent over plain HTTP except to a loopback address.
Without a token, public repositories still work within the anonymous API rate limit. When
no token was found and GitHub answers 401, 403 or 404, the error says so and names the
variable to set for that host. GitHub answers 404 for a private repository read anonymously,
so a missing login otherwise looks like a missing pull request. The same token authenticates
the HTTPS fetches. It is passed to git through `GIT_CONFIG_*` environment variables, never on a
command line, only for the clone URL's host, and with redirects disabled so git cannot carry it
elsewhere. Without a token, git's own credential helpers are used, and prompts are disabled so
nothing ever hangs.

### Scratch directory and sessions

Sessions are stored per repository path, so the default scratch directory is the same for
every run on one pull request: `ocr session list --repo <workdir>` shows its history and
`--resume` keeps working. A scratch directory is only ever emptied or removed when ocr
created it; any other non-empty `--workdir` is refused. A run interrupted while preparing the
directory leaves it recoverable: the next run, or `--cleanup`, takes it over. A lock next to
it (`<workdir>.lock`) stops two runs from using one scratch directory at once.

## Limitations

- The reviewer only sees the pull request's own files plus `--context`. That is the point
  for independent pull requests, but a change that really depends on unchanged code
  elsewhere gets a shallower review than `ocr review` on a full clone.
- The two endpoint commits are fetched with their full trees, without file contents. For
  most repositories that is a small download. In monorepos with a very large number of
  directories, it can still be tens of megabytes.
- Generic mode (`--remote`) has no API, so `--base` is taken as the base commit as is. Pass
  the merge base to avoid reviewing changes from the target branch.
- The host must support partial clone and fetching commits by id, as GitHub, GitLab and
  current Gitea and Bitbucket do. A server that ignores the blob filter sends full file
  contents; ocr warns and carries on.
- The changed files are downloaded in full whatever their size: a pull request that adds
  gigabytes of binaries costs what it weighs, even though the review skips such files.
