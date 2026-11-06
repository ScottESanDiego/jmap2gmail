# jmap2gmail design

Fastmail is the source of truth. jmap2gmail reads it through the JSON Meta
Application Protocol (JMAP) and inserts messages through Gmail's
`users.messages.insert`. It never writes to Fastmail or sends mail.

The command runs once, for example from cron every 5–15 minutes. It has no
daemon mode or persistent connection.

## Mirror rules

Mailbox membership and supported keywords map to Gmail labels. Reconciliation
repairs managed labels and preserves unrelated user labels. It never removes
native `SENT` or `DRAFT`. If a mapped Gmail message disappears, the tool tries
adoption before inserting another copy.

With `delete: true`, every non-spam Gmail message absent from the Fastmail
source view moves to Trash. This includes unrelated and unmarked Gmail mail.
With `delete: false`, extras stay unchanged. The tool never calls Gmail's
permanent-delete API.

Native `SPAM` stays outside matching, reconciliation, and deletion handling.
Trash remains in the mirror: a matching copy returns from Trash while its
source is in view. Extras already in Trash need no further move.

### Role labels

`role_labels` is the sole override for mailbox roles. Without overrides,
`trash` and `junk` use the ordinary labels `Fastmail_Trash` and `Fastmail_Spam`.
Archive has no special default override. The config decoder rejects the removed
`trash_label` and `spam_label` fields before startup writes; it does not migrate
them.

The example config supplies ordinary labels for seven roles: `sent`, `drafts`,
`archive`, `trash`, `junk`, `scheduled`, and `snoozed`. These examples change no
defaults. Overrides affect role mailboxes, not ordinary folders without a role.
Extension roles apply only if Fastmail advertises them. Scheduled and Snoozed
labels transfer no timing behavior.

Gmail forbids manual additions and removals of native `SENT` and `DRAFT`.
Configure ordinary role overrides to avoid requesting those additions. Existing
native labels remain alongside the ordinary labels; the engine does not upload
mail again to replace them. Gmail reserves names beyond its published list.
See [Google's label reference](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.labels).

## Folder selection

`fastmail.include_mailboxes` defines the source view in every sync mode.
Selectors use case-sensitive full Fastmail paths before role mapping:

- An exact path selects that folder alone.
- A trailing `/*` selects descendants at every depth, excluding the parent.
  Add a separate exact path to include the parent.
- Slash boundaries matter: `Inbox/Example/*` excludes `Inbox/Examples/Child`.
- Other `*` positions are invalid. Every selector must match. Unknown or
  ambiguous paths and empty matches fail before persistent writes.

An omitted or empty list selects all mail. Overlapping selectors resolve to
one canonical set of mailbox IDs. Fastmail queries their union on the server,
so a message in several selected folders appears once. Only selected
memberships contribute mailbox labels. Reconciliation preserves unrelated
Gmail user labels on included messages.

Selection reduces Fastmail inventory, metadata, and Message-ID matching work.
It does not reduce the full Gmail inventory, which reads all Gmail metadata
to find matches and extras. A changed canonical selection forces a fresh
inventory; reordering or repeating paths does not. Selection does not change
account identity or message identity headers.

A source deletion, a move outside all selected folders, or config narrowing
can create extras. With deletion enabled, these rules can move any non-spam
Gmail mail outside the view to Trash. Moving within the view updates labels.
Cached, incremental, full, cacheless, and dry runs use the same source view.

## Identity and adoption

One JMAP Email maps to one Gmail message. Its Email ID survives mailbox moves.
Each new insert carries one `X-Jmap2gmail-Account` header and one
`X-Jmap2gmail-Email-ID` header. The insert path strips source-supplied copies
before adding them.

The account value is the hexadecimal SHA-256 hash of
`Session.Username + "|" + accountID`. The account marker is
`jmap2gmail-managed/<account-hash>`. Normal reconciliation preserves this label.
Headers and marker recover mappings; they do not limit which extras move to
Trash.

The tool uses a source RFC Message-ID when available. Otherwise it inserts
`jmap2gmail.<digest>@invalid`, where the digest is the hexadecimal encoding of
the first 16 bytes of SHA-256 of the JMAP Email ID.

Initial adoption matches one source message to one Gmail message by Message-ID.
It adds the marker but cannot add identity headers to existing Gmail mail.
After cache loss, recovery of adopted mail depends on a unique usable
Message-ID. Missing or ambiguous identities can stop recovery. Identity headers
on inserted mail provide stronger recovery; there is no blanket guarantee
against duplicates after cache loss.

Before a rebuild changes Gmail, preflight rejects invalid or conflicting
identity headers, unidentifiable marked legacy mail, duplicate Gmail claims,
and ambiguous adoption matches. Errors identify repairs rather than guessing.
A local Message-ID index avoids per-message Gmail searches during this scan.

Fatal identity and adoption errors request human-readable metadata through
optional diagnostic interfaces. One child context gives all diagnostic reads
a shared 10-second deadline and respects caller cancellation. Reports show
at most 20 sorted candidates per side, plus full counts and truncation.
Lookup failures preserve the primary conflict and mark details unavailable.
Subjects use RFC 2047 decoding; terminal fields use safe quoting. Reports
include sender and recipient fields, sent and received dates with timezones,
full Fastmail folder paths, and Gmail label names. Message-ID queries help
locate mail; API IDs and any thread links serve as secondary references.

Diagnostic reads run only after a fatal conflict. Normal inventory, successful
syncs, and verbose bulk progress do not fetch or log diagnostic message fields.
The journal does not persist them. Diagnostic API requests fetch metadata only;
reports contain no credentials, message bodies, raw mail, or snippets. Normal
OAuth authentication still applies. Diagnostics do not choose a candidate or
resolve conflicts. A shared Message-ID identifies candidates; it does not
prove that their contents match or that a copy is unwanted.

## Cache and resume journal

The optional JSON state caches API results. It stores no message body or
credential. Its contents include:

- Authenticated account identities and committed JMAP and Gmail History cursors.
- JMAP Email ID to Gmail ID mappings and source-deletion marks.
- Mailbox to Gmail label ID and name mappings, plus canonical source selection.
- Source metadata, the label-policy fingerprint, and pending resume work.

The state format remains version 1. The resume journal is an additive field
with its own version 1. Valid checkpoints resume full or incremental work.
Incomplete legacy state without a valid checkpoint requires a rebuild.

Missing, corrupt, invalid, or unsupported state generally triggers a rebuild.
Unresolved insert intent has stricter rules. An unsupported state version with
intent returns an error. An invalid journal with valid intent identity retains
the minimal trusted intent and both account bindings for recovery. Invalid
intent identity returns an error and requires repair.

Cached apply runs use `<state_file>.lock`. State saves create a `0600` temporary
file, sync its contents, rename it atomically, then sync the directory and its
ancestors. A failed checkpoint stops work. The error path does not overwrite
a failed checkpoint with a best-effort save.

### Checkpoint boundaries

A full plan first completes preflight: the Fastmail ID inventory and metadata,
the full Gmail metadata inventory, and identity and adoption validation. Only
then does it create and save the replacement resume journal and source cache.

After planning, repeatable work checkpoints every 100 items and at phase
boundaries. Controlled errors, including cancellation, attempt to save progress
when a journal exists and no checkpoint has failed. Ctrl-C and SIGTERM cancel
the run. A crash or SIGKILL can replay up to 100 repeatable items since the last
successful checkpoint.

An insert saves intent before the destination request. On confirmed success,
it saves the Gmail ID and clears intent immediately, before reporting a source
body-close error. An insert HTTP 400 returns `ErrMutationRejected`, clears
intent, and leaves the item pending. Other ambiguous insert failures preserve
intent.

Unresolved intent requires a complete validated Gmail inventory. Exactly one
message with the matching account and Email ID headers restores the mapping.
Zero or multiple exact matches stop the run with repair guidance. The engine
does not resend or trash mail while that outcome remains unresolved.

Role-policy changes can reevaluate cached metadata without a full scan only
when `SourceEmailsComplete` is true. Otherwise they require full preflight.
Label renames and reuse preserve existing mappings without deleting and
uploading mail again. Source selection changes, expired cursors, Gmail History
gaps, and unknown live Gmail additions can also require a full rebuild.

### Known limits

Inventory scans have no durable partial checkpoint. Errors before full
preflight completes restart Fastmail inventory and metadata scans, Gmail pages
and metadata reads, and local matching on the next rebuild attempt. A legacy
incomplete cache may contain cursors but no resume journal. It still rebuilds,
so a Gmail metadata HTTP 400 `failedPrecondition` can cause repeated full scans.

The initial full source pass reuses validated Gmail inventory metadata for
mapped mail, adoption, and native Spam checks. The engine discards this lookup
before catch-up, source-deletion handling, and extra cleanup. Those paths and
ordinary incremental reads fetch fresh destination metadata. The lookup never
enters the state file. Neither service provides an atomic view of both accounts
during a run.

Without the mailbox mapping, the tool cannot prove that an old Gmail label
came from a renamed Fastmail folder. It uses the target label and retains the
old one to protect user labels.

`--no-cache` always rebuilds with in-memory state. It creates no state cache,
state lock, or temporary state file. Use an external lock such as `flock` to
prevent overlapping cacheless runs. Apply runs still persist OAuth renewals
under the separate token lock.

## Run flow

1. Load state and bind the authenticated Fastmail and Gmail identities.
2. Resolve selected Fastmail paths and derive Gmail label names.
3. Plan incremental work, or complete full preflight and save its resume plan.
4. On resume, catch up source and Gmail History changes before replaying work.
5. Create or rename labels, then reconcile in-scope source mail.
6. Catch up source and Gmail History changes, then process remaining source
   work, including destroyed and out-of-scope messages.
7. Handle Gmail extras, then commit cursors and clear the journal.

Absent or expired cursors require full scans. Catch-up failures that invalidate
the view require fresh preflight before destructive cleanup. Checkpoints record
pending work before committed cursors advance.

### Inventory validation

Every JMAP query page must include a total, the expected position, and a stable
query state and total. Query-state changes restart pagination, up to five
attempts. Empty or duplicate IDs, inconsistent totals, early endings, and
missing or duplicate source metadata stop the scan. Gmail inventory rejects
invalid or duplicate IDs, invalid metadata, and repeated page tokens. A Gmail
metadata 404 skips a message that vanished during the scan. Gmail History also
rejects repeated page tokens and malformed records.

Gmail metadata reads first request only `X-Jmap2gmail-Account`,
`X-Jmap2gmail-Email-ID`, and `Message-ID` headers. A matching response ID with a
missing or null payload triggers one fallback `metadata` request without the
header filter, selecting `id,threadId,labelIds,payload`. A present empty payload
is valid. The fallback uses the same identity and duplicate-header validation;
it never requests `full` or `raw` format or message bodies. A fallback 404 keeps
the vanished-message behavior. Invalid response IDs stop the read. If the
payload remains absent, an error identifies the Gmail API message ID, authorized
account, and available thread ID, with inspection guidance. The tool does not
invent identity headers, skip the message, or change it to repair the response.
Full preflight must finish this validation before Gmail changes begin.

## Gmail request policy

Every Gmail software development kit (SDK) request attempt has a 60-second
child context deadline, including response decoding. The production OAuth
HTTP client has `Timeout: 0`. Backoff waits use the parent context, outside the
attempt deadline. There is no aggregate retry cap or proactive pacing.

Explicit minute quota limits retry until success or parent cancellation.
Backoff starts at one second with jitter and caps at 64 seconds, unless the
server requests a longer wait. Daily quota and authentication failures do not
qualify for these retries.

Timeouts retry only for five reads: `labels.list`, `getProfile`, `messages.get`,
`messages.list`, and `history.list`. They use the same backoff until success or
parent cancellation. An expired parent deadline stops retries. Write timeouts
return errors because the mutation may have succeeded.

Generic HTTP 400 errors, including `failedPrecondition`, HTTP 5xx errors, and
end-of-file (EOF) failures without an expired attempt deadline receive no
automatic retry. A confirmed SDK success
with a nil error remains successful even if cancellation races with completion.
A conclusive HTTP error retains its cause alongside parent cancellation. Only
an insert HTTP 400 clears insert intent through `ErrMutationRejected`.

Insertion spools Multipurpose Internet Mail Extensions (MIME) data into a
`0600` temporary file for quota retries. Deferred cleanup closes and removes
the file on normal returns, errors, and cancellation. A crash or hard kill can
leave the spool behind. The spool is separate from state; dry runs never create
it.

## Output and dry runs

`sync` applies Gmail changes and cache writes by default. `--dry-run` previews
changes without persistent writes. Successful runs print one summary line on
stdout. Dry runs prefix it with `DRY RUN summary:`. Errors always print on
stderr and return a nonzero exit status.

`-verbose` or `--verbose` sends config and folder diagnostics, per-message
actions, progress, retry waits, and retry recovery notices to stderr. Dry runs
list planned label creation or renaming, inserts, adoption, label changes,
Trash moves, and native-spam ignores. Recovery notices include the API method,
retry counts, and elapsed time, without message IDs.

Rebuild progress names Fastmail inventory, metadata loading, Gmail metadata
inventory, and local Message-ID matching. Metadata counts update after each
batch. Matching counts update after the first item, every 100 items, completion,
or an item that crosses five seconds since the last report. Empty scans report
zero. Full source reconciliation starts at zero and follows the same count and
time thresholds after successful messages. Unchanged messages count as complete;
failed messages do not.

Reports run between operations, with no background timer. Pending requests and
retry waits can delay the next progress report.

A dry run writes no Gmail or Fastmail data, cache, state lock, temporary state,
OAuth token, or token lock. It may read an existing cache and requires existing
credential files. Deep cloning includes nested metadata and resume collections,
so planning cannot alter the loaded journal. With `--no-cache`, the journal
stays in memory.

## OAuth renewal

Sync checks the Google token before reconciliation. This initial check falls
outside Gmail SDK attempt deadlines. Apply runs hold `<gmail.token_file>.lock`
while loading, refreshing, and saving tokens. Lock waits stop on cancellation
or after five seconds. This lock also protects `google-auth` token writes;
it does not prevent overlapping syncs.

Persistent apply-run refresh requests have an explicit 30-second deadline.
Each renewed token reaches disk before the HTTP transport uses it, including
during long syncs. If Google omits a refresh token, the tool retains the old
value. Change detection covers access token, refresh token, token type, and
expiry. Saves use `0600` temporary files, file sync, and atomic rename. Token
saves do not sync directories as state saves do.

Dry runs use `config.TokenSource` in memory, without the persistent refresh
path's explicit 30-second deadline or credential writes. An invalid refresh
grant requires explicit `google-auth` reauthorization.

## Permissions and libraries

The tool requests only `gmail.modify`. It uses this scope to read metadata and
history, insert messages, manage labels, and move mail to Trash. The scope also
permits sending mail, but excludes permanent deletion that bypasses Trash.
See [Google's scope documentation](https://developers.google.com/workspace/gmail/api/auth/scopes).

- `git.sr.ht/~rockorager/go-jmap` handles JMAP and blob downloads.
- `google.golang.org/api/gmail/v1` handles Gmail.
- `golang.org/x/oauth2` handles Google OAuth.

Automated tests use fakes and local HTTP servers. They make no live Gmail or
Fastmail changes.
