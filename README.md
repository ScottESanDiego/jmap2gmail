# jmap2gmail

`jmap2gmail` mirrors Fastmail to Gmail. Fastmail remains the source of truth. The
tool reads Fastmail through JMAP and inserts mail through the Gmail API.

With `"delete": true`, it moves every non-spam Gmail message absent from the
Fastmail source view to Trash, including mail it never inserted or marked.
With `false`, it keeps those extra messages. Preview with `--dry-run`.

It uses [`go-jmap`](https://sr.ht/~rockorager/go-jmap/) and Google's official Go
clients.

## Behavior

- Fastmail mailboxes become Gmail labels, including nested mailboxes.
- Messages without `$seen` get `UNREAD`; messages with `$seen` do not.
  `$flagged` maps to `STARRED`.
- Fastmail Trash maps to `Fastmail_Trash`.
- Fastmail Junk maps to `Fastmail_Spam`.
- Gmail-native `SPAM` is outside the mirror. The tool does not adopt, relabel,
  or trash those messages.
- With `"delete": true`, Gmail messages absent from the source view move to
  Trash, whether marked or unmarked. The tool never calls Gmail's permanent-delete API.

Each inserted message gets one `X-Jmap2gmail-Account` header and one
`X-Jmap2gmail-Email-ID` header. The tool strips source copies of both headers
before insertion. It also adds this per-account Gmail label:

```text
jmap2gmail-managed/<account-hash>
```

The JMAP Email ID stays the same when mail moves between Fastmail mailboxes.
The headers and label let the tool rebuild its mapping after cache loss. Normal
syncs restore the label if another client removes it. These markers recover
message identity; they do not limit which Gmail messages can move to Trash.

On the first run, the tool may adopt existing Gmail mail by RFC Message-ID.
Adoption requires one source message and one Gmail match. The tool adds the
managed label. Gmail blocks header changes after insertion, so adopted mail
keeps its original headers.

During a rebuild, the tool inventories Gmail and checks adoption candidates
before it changes Gmail. It stops if a marked legacy message lacks a usable Message-ID. Shared
source Message-IDs, duplicate Gmail matches, and conflicting identities also
stop the run with repair guidance.

## Setup

Requires Go 1.27.2 or newer.

1. Copy `config.example.json` to `config.json`. Update its paths. Relative paths
   use the config directory, and the tool expands `~/`.
2. Create a Fastmail API token under **Settings → Privacy & Security → Manage
   API tokens**. Put the token alone in `fastmail.token_file`. An IMAP app
   password will not work.
3. Enable the Gmail API in Google Cloud. Create a **Desktop app** OAuth client.
   Save its JSON at `gmail.credentials_file`.
4. Authorize Gmail:

   ```sh
   go run ./cmd/jmap2gmail google-auth -config config.json
   ```

   The tool requests `gmail.modify` and no broader scope. It uses this scope to
   read and change mail, manage labels, insert messages, and move messages to
   Trash. The scope also permits sending mail; jmap2gmail does not send mail.
   It does not allow permanent deletion that bypasses Trash.
   See [Google's scope documentation](https://developers.google.com/workspace/gmail/api/auth/scopes).

   If an old token granted `mail.google.com`, revoke that grant. Then remove the
   token file and authorize again.

5. Preview and apply the first run:

   ```sh
   go run ./cmd/jmap2gmail sync -config config.json --full --dry-run
   go run ./cmd/jmap2gmail sync -config config.json --full
   ```

`sync` applies changes by default. Pass `--dry-run` to preview without writes.
Successful apply and dry runs write one summary line to stdout. Dry-run
summaries start with `DRY RUN summary:`. Add `--verbose` to see planned label
creation, renames, inserts, adoptions, label changes, Trash moves, and ignored
native spam on stderr. Errors returned by sync always go to stderr
and return a nonzero exit status.

A dry run reads Fastmail, Gmail, the config, credentials, and any existing
cache. It writes no Gmail data, cache, lock, temporary state, or OAuth token.
The required credential and token files must already exist.

## Role labels

Use `role_labels` to override Gmail labels for Fastmail mailbox roles. It is
the only role-label setting. Omitted `trash` and `junk` entries default to
`Fastmail_Trash` and `Fastmail_Spam`. Trash and Junk overrides must be nonblank,
distinct ordinary Gmail labels; native Trash and Spam are invalid.
The role key for Junk is `junk`, not `spam`.
For a mirror that uses ordinary Gmail labels for special folders, use these
explicit overrides, also shown in `config.example.json`:

```json
"role_labels": {
  "sent": "Fastmail_Sent",
  "drafts": "Fastmail_Drafts",
  "archive": "Fastmail_Archive",
  "trash": "Fastmail_Trash",
  "junk": "Fastmail_Spam",
  "scheduled": "Fastmail_Scheduled",
  "snoozed": "Fastmail_Snoozed"
}
```

Gmail does not allow manually applying `SENT` or `DRAFT`. Its reserved-label
list is not exhaustive. These ordinary labels avoid those system-label
restrictions. See [Google's label guide](https://developers.google.com/workspace/gmail/api/guides/labels).
Existing native Sent or Draft labels may remain after an override because
Gmail also forbids removing them. The tool adds the ordinary labels without
deleting and uploading the messages again. See [Google's label reference](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.labels).
Scheduled and Snoozed are [registered mailbox attributes](https://www.iana.org/assignments/imap-mailbox-name-attributes),
but their overrides apply only when Fastmail advertises those roles. Labels do
not transfer sending schedules or snooze times. `role_labels` does not rename
ordinary folders that have no role.

These recommendations do not change the built-in defaults: `inbox` uses
`INBOX`, `important` uses `IMPORTANT`, `flagged` uses `STARRED`, and `all` adds
no label. Other overrides, including Archive, remain explicit configuration.

The old `trash_label` and `spam_label` fields now cause a config error. Replace
`"trash_label": "X"` and `"spam_label": "Y"` with:

```json
"role_labels": {
  "trash": "X",
  "junk": "Y"
}
```

Remove the old fields; the tool does not convert them or accept aliases.

## Select Fastmail folders

Set `include_mailboxes` in the config's existing `fastmail` object to mirror
selected folders:

```json
"include_mailboxes": ["Inbox", "Projects/Test"]
```

Use case-sensitive full Fastmail paths, before Gmail role-label mapping.
Exact paths such as `Inbox` or `Inbox/Example` select only that folder.
Add `/*` to select all descendants at any depth, excluding the parent itself.
For example, `Mailing Lists/*` includes `Mailing Lists/Gentoo`,
`Mailing Lists/Mailers`, and deeper folders. Add `Mailing Lists` separately to
include the parent. `Inbox/Example/*` does not match `Inbox/Examples/Child`.

Omit the field or use `[]` to mirror all mail. Only a trailing `/*` is supported;
other `*` positions are invalid. Every selector must match at least one folder.
Unknown or ambiguous paths and empty matches stop sync before persistent
writes. Overlapping selectors count each folder once. A message in several
selected folders gets one Gmail copy with those
folder labels. Only selected folders contribute managed mailbox labels; other
Gmail labels on included mail stay unchanged.

The selection defines the source view for the whole Gmail mirror. With
`"delete": true`, every non-spam Gmail message absent from that view moves to
Trash. This includes unrelated Gmail mail and copies the tool never marked.
Deleting source mail, moving it out of all selected folders, or narrowing the
config can therefore move Gmail mail to Trash. With `false`, extra mail stays.

For example, changing `["Inbox", "Projects/Test"]` to `["Inbox"]` makes every
non-spam Gmail message without an Inbox source match eligible for Trash when
deletion is enabled. Moving mail between selected folders updates its labels.

The filter works with cached, full, incremental, cacheless, and dry runs. It
reduces Fastmail scanning and local matching work. A first run still inventories
all Gmail metadata, even for a small Fastmail folder. Changing the selection
triggers a fresh inventory. Recovery uses the same account marker and identity
headers, including with `--no-cache`. Native `SPAM` remains excluded. Gmail
Trash is included: a matching copy returns from Trash when its source is in
the selected view. Preview selection changes with `--dry-run`.

### Debug folder selection

Use `-verbose` or `--verbose` to diagnose folder selection:

```sh
./jmap2gmail sync --dry-run --verbose -config config.json
```

Verbose details go to stderr; stdout still contains only the summary. Details
include config and state paths, mode flags, quoted selectors, sorted Fastmail
folder paths and their advertised roles, the resolved selection, message
actions, reconciliation progress,
and retry waits and recoveries. They contain no HTTP dumps, token values,
credential contents, or message contents.

A bare trailing slash does not select descendants. If
`Inbox/Don Trust and Will/` is unknown, use `Inbox/Don Trust and Will` for the
exact folder or `Inbox/Don Trust and Will/*` for all descendants, excluding the
parent. The tool does not trim the slash or change the selector for you.

## Rebuild progress

The first run and later full rebuilds can take time before the summary appears.
The tool inventories Fastmail, loads its message metadata, inventories
Gmail metadata, and matches Message-IDs using a local index. Matching does not
make a Gmail search request for each source message. With `--verbose`, stderr
shows each phase and Fastmail metadata batch progress. Local matching progress
shows completed counts and elapsed time. Gmail changes follow rebuild
validation.

With `--verbose`, full reconciliation also reports completed and total
messages, unchanged messages, and elapsed time. This progress starts at zero and
continues even when no Gmail changes are needed. Full runs reuse the validated
Gmail inventory during the initial message pass, reducing repeated reads.
Deletion checks and later changes still read current Gmail state.

Progress updates run between operations; a pending request can delay the next
line. Each Gmail request attempt has a 60-second context deadline, covering
response headers and body decoding in normal and dry runs. Retry waits fall
outside this deadline, so an operation can take longer.

## Gmail retries

Gmail requests proceed without proactive pacing, including during dry runs.

When Gmail explicitly reports a minute quota limit, the tool waits and retries
until the request succeeds or you cancel. Retry delays start at one second and
grow exponentially with jitter, capped at 64 seconds. A later server retry hint
takes precedence.

Timed-out Gmail reads also retry with exponential backoff until they succeed
or you cancel. A caller deadline or cancellation stops retries. Write timeouts
return errors because Gmail may already have applied the change. Other network
or server failures can still end the run. An uncertain insert failure returns
an error so the next run can recover message identity before inserting again.

With `--verbose`, retry waits and successful recovery notices go to stderr.
Notices include the API method, retry count, and elapsed time, without message
IDs. Failed or canceled requests do not report recovery.

Inserts stream MIME through a short-lived temporary file with `0600`
permissions so quota retries can replay the message. The tool removes the file
when the insert returns, including on failure. It is separate from the cache;
dry runs create no spool files.

## OAuth token renewal

`google-auth` saves an access token, its expiry, and a refresh token. Sync checks
credentials before reconciling messages. It renews expired access tokens,
including during long runs.

Apply runs save each renewed token before using it, with an atomic replacement
and `0600` permissions. A separate `<gmail.token_file>.lock` lets one process at
a time load, renew, and save tokens. It also covers `--no-cache` runs.
Lock waits stop after five seconds. Apply-run token refresh requests time out
after 30 seconds.
The token directory must be writable. Token locking protects credentials;
cacheless syncs still need an external lock to prevent overlapping Gmail work.

Dry runs renew tokens only in memory and create no token lock or token files.
If Google revokes or expires the refresh token, run `google-auth` again.

## Optional cache

The state file is optional for message sync. It reduces JMAP and Gmail API
calls, tracks label renames, and resumes interrupted cached runs. It stores:

- Fastmail and Gmail account identities
- JMAP and Gmail History cursors
- JMAP Email ID to Gmail message ID mappings and deletion marks
- Fastmail mailbox to Gmail label mappings
- source message metadata and a versioned checkpoint of pending work

The cache contains no message bodies or credentials. The tool writes it with
an atomic replacement. A sync that applies changes with a cache also holds
`<state_file>.lock` to stop overlap.

A missing, corrupt, invalid, or incompatible cache starts a full rebuild from
Fastmail and Gmail. An incomplete older cache without a valid checkpoint also
rebuilds. A rebuild validates message identity and adoption matches against
the Gmail inventory before changing Gmail.

Without the cache, the tool cannot link a renamed Fastmail mailbox to its old
Gmail label. It creates the new label and leaves the old one in place. This
protects Gmail labels that the user created.

Use `--no-cache` to rebuild on every run:

```sh
go run ./cmd/jmap2gmail sync -config config.json --no-cache
```

This mode creates no state, state lock, or temporary state file. It makes more
API calls and needs an external lock to prevent overlapping syncs.

## Resume failed runs

Cached runs save pending work and completed progress during initial full and
incremental syncs. A rejected request, such as an invalid Gmail label returning
HTTP 400, still ends the run with an error. Fix the cause and rerun `sync`
without `--full` to resume from the checkpoint. Changing `role_labels`
reevaluates cached message metadata and updates labels without deleting and
uploading those messages again. It can still require API reads.

The tool checkpoints every 100 repeatable work items, at phase boundaries, and
on controlled error exits. Ctrl-C and SIGTERM request cancellation and attempt
a checkpoint before exit. A crash or SIGKILL can replay up to 100 items from
the last saved checkpoint. Insert intent and results save immediately because
inserts need separate duplicate protection.

If an insert's reply is lost, the next run checks a complete Gmail inventory
for its exact identity headers. One match recovers the existing copy. No match
or conflicting matches stop the run with repair guidance; the tool does not
blindly resend. Source and Gmail changes are caught up before destructive
cleanup. Expired change history, changed source selections, unknown Gmail
changes, or unusable checkpoints can still require a full rebuild.

`--no-cache` keeps this work only in memory. Dry runs clone state for planning
and write no checkpoints. Default output remains one summary line; errors
remain visible, and `--verbose` shows progress.

## Cron

Run the tool once every 5–15 minutes. It is a one-shot command, not a daemon,
and it keeps no persistent connection. Default output is one summary line;
add `--verbose` for details on stderr.

Use `flock` to prevent cacheless runs from overlapping. This external lock also
works for cached runs:

```cron
*/10 * * * * flock -n ~/.local/state/jmap2gmail/cron.lock /path/to/jmap2gmail sync -config /path/to/config.json --no-cache
```

Cached runs may omit `--no-cache`. Their state lock also blocks overlap.

## Files

- `config.json`: settings and paths; protect it if it reveals private paths
- Fastmail token: a credential
- Google client JSON: OAuth client data
- Google OAuth token: `google-auth` creates it; apply runs save renewals
- `<gmail.token_file>.lock`: protects authorization and apply-time token renewal
- State JSON: an optional cache
- `<state_file>.lock`: an apply-time lock for cached runs

Store credentials apart from the optional cache. An ephemeral container can
run the tool if you mount the config and credentials. Mount the cache to save
API calls.

## Gmail limits

- `insert` can use a message's `Date` header for Gmail's internal date. It
  cannot set Fastmail's JMAP `receivedAt` value.
- Gmail cannot manually apply `SENT` and `DRAFT`. Use the ordinary labels in
  the recommended `role_labels` configuration above.
- Gmail performs final MIME and size checks. If Gmail rejects a message, it
  stays unmapped, and the JMAP cursor does not advance. The next run tries it
  again.

## Development

```sh
make check
```

See `DESIGN.md` for the sync rules. Tests use local fakes; they do not run
against live Fastmail or Gmail accounts.
