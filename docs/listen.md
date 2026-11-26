# Reviewing new pull request commits continuously: `ocr listen`

`ocr listen` keeps reviewing the open pull requests of one or more repositories, one new
head commit at a time, by running [`ocr review-pr`](review-pr.md) for each: nothing is ever
cloned. Several listeners running in different folders of one machine share the work.

```bash
# One listener, in the current folder
ocr listen --github-repo acme/book

# Three listeners in three folders, sharing the work
for n in 1 2 3; do
  mkdir -p ~/ocr-listeners/$n
  (cd ~/ocr-listeners/$n && nohup ocr listen --github-repo acme/book > listen.log 2>&1 &)
done

# One listener per chapter, each posting its review as a pull request comment
ocr listen --github-repo acme/book --path chapters/07 --format text \
  --on-review 'gh pr comment "$OCR_LISTEN_PR_URL" --body-file "$OCR_LISTEN_RESULT"'

# Flags after -- go to every `ocr review-pr` run
ocr listen --github-repo acme/book -- --provider anthropic --model claude-opus-4-6 --effort high
```

Every `--interval` (default one minute) a listener lists the open pull requests of each
`--github-repo` and reviews each head commit it has not reviewed yet, one at a time, by
running `ocr review-pr` as a child process. A new push to a pull request is a new head
commit, so it is reviewed again.

| Flag | Meaning |
|------|---------|
| `--github-repo R1,R2` | Repositories to watch: `OWNER/REPO`, `HOST/OWNER/REPO` or a URL. |
| `--dir DIR` | This listener's folder (default: the current directory). |
| `--state-dir DIR` | State shared by cooperating listeners (default: `~/.opencodereview/listen`). |
| `--path p1,p2` | Passed to `review-pr --path`. |
| `--base-branch B` | Only pull requests targeting branch `B`. |
| `--include-drafts` | Also review draft pull requests. |
| `--format F` | `json` (default), `text` or `sarif`. |
| `--interval D` | Time between polls, for example `30s` or `5m`. |
| `--once` | Poll once and exit, for cron or CI. The exit status is non-zero when a review or a listing failed. |
| `--max-attempts N` | Give up on a head commit after N failed reviews (default 3). |
| `--review-timeout D` | Stop a review that runs longer than `D`, and separately a hook (default `1h`, `0` for no limit). A timeout counts as a failed review. |
| `--on-review CMD` | Shell command run after each successful review. |
| `--keep-work` | Keep each review's scratch directory. |
| `--github-api URL` | REST API root, as for `review-pr`. |

## What a listener writes

```
<dir>/ocr-listen/reviews/<host>/<owner>/<repo>/pr-<N>-<sha12>-<scope>.<json|txt|sarif>
<dir>/ocr-listen/logs/<host>/<owner>/<repo>/pr-<N>-<sha12>-<scope>.log
<dir>/ocr-listen/work/<host>/<owner>/<repo>/pr-<N>-<sha12>-<scope>/   (removed unless --keep-work)
<dir>/ocr-listen/work/<host>/<owner>/<repo>/pr-<N>-<sha12>-<scope>.ok    (written once the review succeeded)
```

Each review attempt appends to the log, starting with a `=== ocr listen: ... ===` header, so the
output of earlier failed attempts stays readable. Every poll ends with one summary line on
stderr: open, reviewed, failed, skipped, busy and listing errors.

## How listeners share the work

Listeners that share a `--state-dir` cooperate. Each unit of work, one head commit of one
pull request, is claimed with an operating-system file lock under
`<state-dir>/<host>/<owner>/<repo>/locks/`. Whoever holds the lock reviews; the others skip
the unit and move on. The operating system releases the lock when its holders exit, so a
crash never leaves a stale claim behind. A finished unit is recorded under `done/`, and a
failed one counts against `--max-attempts` under `attempts/`. When a head runs out of
attempts, the listener logs the `attempts/` file to delete for one more try; a new push is a
new head and starts afresh.

On Linux and macOS the review child inherits the claim. A listener killed in the middle of a
review leaves that review running and still claimed; the next listener to claim the unit
afterwards adopts its finished result (recorded under `inflight/` when it started) instead of
reviewing again. Only a result that `review-pr` marked as successful, through the `.ok` file
beside the work directory, is adopted; a review that failed or was interrupted is retried.
On Windows a lock goes away with the process that took it, so a listener killed mid-review
can lead to a second review of that head.

The unit also carries a scope: a fingerprint of `--path`, `--format` and the flags after
`--`. Listeners that review the same pull request differently, such as one per chapter, have
different scopes and never suppress each other.

These locks are reliable between processes of one machine. They are not reliable across
machines sharing a network filesystem.

Cooperating listeners must run as the same operating-system user. The state directory is
trusted: its records name the result files a listener adopts and hands to the hook, so a
user who can write to it can steer another user's listener. Keep it writable by that user
only, as the default under your home directory is.

## The `--on-review` hook

The command runs through `sh -c` (`cmd /C` on Windows) with these variables set. Values come
in through the environment, never by splicing them into the command text:

| Variable | Value |
|----------|-------|
| `OCR_LISTEN_REPO` | `OWNER/REPO` (or `HOST/OWNER/REPO`) |
| `OCR_LISTEN_PR` | Pull request number |
| `OCR_LISTEN_PR_URL` | Pull request web URL |
| `OCR_LISTEN_HEAD_SHA` | Reviewed head commit |
| `OCR_LISTEN_RESULT` | Result file |
| `OCR_LISTEN_FORMAT` | `json`, `text` or `sarif` |
| `OCR_LISTEN_LOG` | Log file |

A failing hook is logged and does not undo the review. The hook keeps running when the
listener is asked to stop, up to `--review-timeout`, because the review is already recorded
as done and nothing would run the hook again. On Linux and macOS the hook runs in its own
process group, and a timeout stops the whole group, including anything the hook started;
on Windows a timeout stops the hook's shell only.

The hook runs once per unit, in the listener that reviewed it. Listeners that share a
`--state-dir` and a scope share their outcomes, so listeners with different hooks need
separate state directories.

## Limitations

- Everything listed for [`ocr review-pr`](review-pr.md#limitations) applies to each review.
- A listener lists every open pull request of every repository on each poll. With many
  repositories or listeners, use a token (5,000 requests an hour instead of 60) and a
  longer `--interval`.
- The state directory keeps a few small files per reviewed head. Old entries under `done/`
  and `attempts/` can be deleted at any time; never delete `locks/` while listeners run.
