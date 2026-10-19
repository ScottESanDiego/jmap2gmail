# jmapsync design

Fastmail is the source of truth. The tool reads Fastmail through JMAP and writes
Gmail through `users.messages.insert`.

The program is a one-shot command for cron every 5–15 minutes. It has no daemon
mode and keeps no persistent connection.

## Identity and recovery

One JMAP Email maps to one Gmail message. Each new Gmail insert gets one
`X-Jmapsync-Account` and one `X-Jmapsync-Email-ID` header. The insert path
removes source-supplied copies first.

The account value is a stable hash of the Fastmail account identity. The tool
also adds `jmapsync-managed/<account-hash>`. Every normal reconcile preserves
that label. The marker and headers recover identity; they do not define a
deletion boundary.

The JMAP Email ID stays the same across mailbox moves. The two headers and
marker label preserve the mapping. They let the tool recover after a crash or
cache loss without inserting a duplicate.

Initial adoption requires one source and one Gmail message to share an RFC
Message-ID. Adoption adds the marker label but cannot rewrite the Gmail
message's headers. Gmail blocks header changes after insertion.

The rebuild preflight rejects bad or conflicting identity headers. It also
rejects missing legacy IDs, duplicate Gmail matches, and shared source matches.
These checks run before the rebuild changes Gmail. Errors tell the user what
to repair.

## Mirror rules

- Mailbox membership and supported keywords map to Gmail labels.
- Fastmail Trash and Junk use `Fastmail_Trash` and `Fastmail_Spam`.
- With `delete` enabled, every non-spam Gmail message absent from the Fastmail
  source view moves to Trash, including unmarked or unrelated Gmail mail.
  With deletion disabled, extra Gmail messages stay unchanged.
- The tool never calls Gmail's permanent-delete API.
- Gmail-native `SPAM` is outside the mirror. Matching, reconciliation, and
  deletion handling ignore it.
- The next reconcile reverts Gmail-side label changes.
- The tool inserts a mapped message again if it vanishes from Gmail.

`role_labels` is the sole override for mailbox role labels. Missing `trash`
and `junk` overrides use `Fastmail_Trash` and `Fastmail_Spam`. Archive has no
special default override. The config decoder rejects the removed `trash_label`
and `spam_label` fields before startup writes; there is no automatic migration.

## Folder selection

`fastmail.include_mailboxes` defines the source view in every sync mode.
Selectors use case-sensitive full Fastmail paths before Gmail role mapping.
Exact paths select only that folder. A trailing `/*` selects all descendants
at any depth, excluding the parent. Include the parent with a separate exact
path. Matching respects slash boundaries: `Inbox/Example/*` does not select
`Inbox/Examples/Child`. No other `*` position is valid.

An omitted or empty list includes all mail. Each selector must match at least
one folder. Unknown or ambiguous paths, unsupported patterns, and empty matches
fail before persistent writes. Overlapping selectors resolve to one canonical
set of mailbox IDs.

Fastmail queries the union of selected mailbox IDs on the server. A message
in several selected folders appears once. Only selected mailbox memberships
contribute Gmail labels; other Gmail labels on included messages stay
unchanged. The selection reduces inventory, metadata loads, and Message-ID
matching work. It does not reduce the initial Gmail inventory: the tool must
read all Gmail metadata to find matches and extras. The cache stores a canonical
selection fingerprint. A changed
selection forces a fresh inventory; reordering or repeating the same paths
does not.

Every non-spam Gmail message absent from this view is an extra, regardless of
its marker or history. With `delete: true`, extras move to Trash; with `false`,
they stay. Source deletion, a move out of all selected folders, and config
narrowing can all create extras. Moving within the view updates labels. The
same rules apply to cached, incremental, full, cacheless, and dry runs.

Selection does not change account identity or marker headers. Recovery uses
the account marker and headers to restore known mappings. A local Message-ID
index identifies adoption candidates among other Gmail mail. Native `SPAM`
always stays excluded; Gmail Trash does not. A matching copy returns from Trash
when its source is in the view. Extras already in Trash need no further move.

## Cache

The optional JSON state is an API-call cache. Message sync remains correct
without it. The cache stores:

- authenticated account identities
- JMAP and Gmail History cursors
- JMAP Email ID to Gmail ID mappings and source-deletion marks
- Fastmail mailbox to Gmail label ID and name mappings
- canonical mailbox selection

It stores no message body or credential. Cached apply runs use
`<state_file>.lock`. State writes use a temporary file and an atomic rename.

A missing, corrupt, invalid, unsupported, or incomplete state file triggers a
full server rebuild. The rebuild preflight reads a complete snapshot of the
source view and all Gmail metadata before changing Gmail. It validates identity
claims, inventory completeness, and adoption matches before Gmail mutations.

Before changing Gmail, the tool marks the cached apply run as incomplete. If
the final state save fails, the next run must rebuild from the servers.

`--no-cache` uses in-memory state and always rebuilds. It creates no cache,
state lock, or state temporary file. Use an external lock such as `flock` to
prevent overlapping cacheless runs.

The mailbox mapping also tracks label renames. Without it, the tool cannot
prove that an old Gmail label came from a renamed Fastmail mailbox. It creates
the new label and keeps the old one to protect labels the user created.

## Run flow

1. Load the cache, or prepare a full rebuild.
2. Bind the authenticated Fastmail and Gmail identities.
3. Resolve selected Fastmail paths and derive their Gmail labels.
4. When a full scan is needed, inventory Gmail and validate identity and matches.
5. Create or rename labels, then reconcile Fastmail mail.
6. Move Gmail mail absent from the source view to Trash if deletion is enabled.
7. Reconcile Gmail History changes and save new cursors.

JMAP and Gmail History cursors make cached runs incremental. An absent or
expired cursor causes a full scan. Full scans, selection changes, gaps in Gmail
History, and unknown live Gmail additions require fresh source and destination
inventories. A local Message-ID index replaces per-message Gmail searches.

State saves follow each successful mutation. Cursors advance after all earlier
work succeeds. A message without an RFC Message-ID gets a deterministic
`@invalid` value during insertion.

The initial full source pass reuses validated Gmail inventory metadata for
existing mappings, adopted mail, and native Spam checks. This lookup exists
only for that pass. The engine discards it before source-deletion handling,
extra-message cleanup, and incremental catch-up. Those paths and ordinary
incremental reads use fresh destination metadata. This adds no persistent
state or cache format changes. Accounts can change during a run; the inventory
does not provide an atomic view of both accounts.

## Rebuild progress

With `--verbose`, rebuilds print phases on stderr before Fastmail inventory,
metadata loading, Gmail metadata inventory, and local Message-ID matching.
Fastmail metadata progress reports completed and total messages after each
batch. Local matching reports
completed counts and elapsed time after the first item, each 100 items, and the
final item. It also reports after an item if five seconds have passed since the
last update. Empty scans report zero completed work.

With `--verbose`, full source reconciliation reports completed and total
messages, unchanged messages within the pass, and elapsed time. It starts at
zero, then reports after the first successful message, every 100 messages, and
completion. It also
reports after a successful message when five seconds have passed since the
last update. Unchanged messages count as completed work. Failed messages do
not advance the count.

Reports run between operations, so pending requests can delay output. No
background timer prints progress. Rebuild validation still precedes Gmail
changes.

The authenticated Gmail HTTP client has a 60-second timeout per HTTP attempt in
normal and dry runs. Quota backoff waits fall outside that timeout.
This limit does not cover the initial OAuth token check, which runs before the
client is created. Apply-run token refresh keeps its separate 30-second limit.

## Gmail request policy

Gmail requests proceed without proactive pacing. The same policy applies to
reads, writes, and dry runs.

Explicit minute quota errors retry until success or context cancellation.
Exponential backoff starts at one second with jitter and caps its delay at
64 seconds, unless the server requests a later retry. With `--verbose`, retry
waits report to stderr. The policy does not promise retries for every transient
or server error. In particular, an
ambiguous insert failure returns to the engine for next-run identity recovery.

After actual quota retries succeed, verbose diagnostics report recovery with
the API method, retry count, and elapsed time, without message IDs. Requests
that fail, are canceled, or never encounter a quota limit emit no recovery
notice. Default output remains the single summary line and any errors.

Insertion streams MIME into a temporary `0600` file and replays it for quota
retries. Each return path removes the spool, including cancellation and
failure. The spool is separate from the state cache and exists only during an
insert. Dry runs never create it.

## Dry run

`sync` applies Gmail changes and cache writes by default. `--dry-run` previews
without persistent writes. Successful apply and dry runs print one summary line
on stdout; dry runs use the prefix `DRY RUN summary:`. `-verbose` or `--verbose`
enables config and folder diagnostics, per-message actions, rebuild progress,
and quota retry waits on stderr. Verbose dry runs list each planned label create
or rename, insert, adoption, label change, Trash move, and native-spam ignore.
Errors, including timeout failures, always print on stderr and return a
nonzero exit status, regardless of verbosity.

A dry run mutates no Gmail or Fastmail data. It also writes no cache, state
lock, temporary state, or OAuth token. It may read an existing cache and needs
existing credential files.

## OAuth renewal

Sync validates or renews the Google token before starting reconciliation.
Apply runs hold `<gmail.token_file>.lock` while loading, renewing, and saving
tokens. Each save uses an atomic replacement. The tool saves each renewed token
before the HTTP transport uses it, including during long syncs. If Google omits
a refresh token, the tool keeps the existing value. It checks for changes in
the access token, refresh token, token type, and expiry. Token files use `0600`
permissions.

The lock is independent of the optional state cache and also protects token
writes from `google-auth`. Lock waits stop on cancellation or after five
seconds. Renewal requests time out after 30 seconds. This lock does not prevent
overlapping Gmail syncs.

Dry runs use an in-memory OAuth token source with no credential or lock writes.
An invalid refresh grant requires explicit `google-auth` reauthorization.

## Permissions and libraries

The tool requests only `gmail.modify`. It uses this scope to read metadata and
history, insert messages, manage labels, and move mail to Trash. The scope also
permits sending mail; jmapsync does not send mail. It does not allow permanent
deletion that bypasses Trash. See
[Google's scope documentation](https://developers.google.com/workspace/gmail/api/auth/scopes).

- `git.sr.ht/~rockorager/go-jmap` handles JMAP and blob downloads.
- `google.golang.org/api/gmail/v1` handles Gmail.
- `golang.org/x/oauth2` handles Google OAuth.

Automated tests use fakes and local HTTP servers. They make no live Gmail or
Fastmail changes.
