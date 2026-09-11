# Conflicts

The server never decides which side of a merge wins. This page describes what a client sees and what it must do.

## How a conflict appears

Before every write (and on the periodic pull) the server runs `git fetch` and `git merge origin/main` on its private clone. When the merge cannot complete cleanly:

- nothing is committed and nothing is pushed;
- Git's merge state stays on disk (`MERGE_HEAD`, index stages, markers in the work tree), so a restart resumes in the same state;
- the server enters `state: conflict` (`kb_sync_status`, `kb_info`);
- every write tool and `kb_sync_now` fail with code `merge_conflict`;
- read tools keep working: `kb_get_note` on a conflicted note returns the marked content plus a `conflict` object.

The `merge_conflict` payload and `kb_conflicts` list, for each path:

| Field | Content |
|---|---|
| `kind` | `content`, `modify/delete` (we modified, remote deleted), `delete/modify` (we deleted, remote modified), `add/add` |
| `content` | Work tree with `<<<<<<< / ======= / >>>>>>>` markers (null when the file is absent) |
| `base` | Common ancestor version (null for add/add) |
| `ours` | The server's version (null when our side deleted) |
| `theirs` | The remote version (null when the remote deleted) |

`remote_commits` lists the commits being merged so the client can see what changed and why.

## What the client does

1. Read the sides (`ours`, `theirs`, `base`), gather context if needed (`kb_history`, `kb_diff`, neighbouring notes).
2. Decide the final content for every conflicted path.
3. Call `kb_resolve_conflict` with one resolution per path:
   - `content`: the merged text (must not contain markers),
   - `take: "ours"` or `take: "theirs"`: keep one side as-is (taking a side that deleted the file deletes it),
   - `delete: true`: remove the note.
   Paths can be resolved across several calls; writes stay blocked until the last one.
4. When all paths are resolved the server commits the merge (`merge: origin/main (resolved: …)` with `KB-Client` and `KB-Instance` trailers), re-indexes, leaves the conflict state and pushes.

If a stale `etag` is the reason a write fails (`code: conflict`), that is not a Git conflict: the payload carries the note's current `content` and `etag`, so the client re-applies its change on top and writes again.
