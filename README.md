# jmapsync

`jmapsync` mirrors Fastmail to Gmail. Fastmail remains the source of truth. The
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

Each inserted message gets one `X-Jmapsync-Account` header and one
`X-Jmapsync-Email-ID` header. The tool strips source copies of both headers
before insertion. It also adds this per-account Gmail label:

```text
jmapsync-managed/<account-hash>
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
   go run ./cmd/jmapsync google-auth -config config.json
   ```

   The tool requests `gmail.modify` and no broader scope. It uses this scope to
   read and change mail, manage labels, insert messages, and move messages to
   Trash. The scope also permits sending mail; jmapsync does not send mail.
   It does not allow permanent deletion that bypasses Trash.
   See [Google's scope documentation](https://developers.google.com/workspace/gmail/api/auth/scopes).

   If an old token granted `mail.google.com`, revoke that grant. Then remove the
   token file and authorize again.

5. Preview and apply the first run:

   ```sh
   go run ./cmd/jmapsync sync -config config.json --full --dry-run
   go run ./cmd/jmapsync sync -config config.json --full
   ```

`sync` applies changes by default. Pass `--dry-run` to preview without writes.
Successful apply and dry runs write one summary line to stdout. Dry-run
summaries start with `DRY RUN summary:`. Add `--verbose` to see planned label
creation, renames, inserts, adoptions, label changes, Trash moves, and ignored
native spam on stderr. Errors, including timeout failures, always go to stderr
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
The example config also sets `archive` to `Fastmail_Archive`; this is an
explicit override, not an automatic default.

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
./jmapsync sync --dry-run --verbose -config config.json
```

Verbose details go to stderr; stdout still contains only the summary. Details
include config and state paths, mode flags, quoted selectors, sorted Fastmail
folder paths, the resolved selection, message actions, reconciliation progress,
and quota retry waits and recoveries. They contain no HTTP dumps, token values,
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
line. Each Gmail HTTP attempt has a 60-second timeout, in normal and dry runs.
Quota retry waits fall outside this limit, so an operation can take longer.

## Gmail quota retries

Gmail requests proceed without proactive pacing, including during dry runs.

When Gmail explicitly reports a minute quota limit, the tool waits and retries
until the request succeeds or you cancel. Retry delays start at one second and
grow exponentially with jitter, capped at 64 seconds. A later server retry hint
takes precedence. With `--verbose`, wait notices go to stderr. Other
failures can end the run. An uncertain insert failure returns an error so the
next run can recover message identity before inserting again.

After a quota-limited request succeeds, `--verbose` reports the API method,
retry count, and elapsed time on stderr, without message IDs. Failed or canceled
requests do not report recovery.

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
calls and tracks label renames. It stores:

- Fastmail and Gmail account identities
- JMAP and Gmail History cursors
- JMAP Email ID to Gmail message ID mappings and deletion marks
- Fastmail mailbox to Gmail label mappings

The cache contains no message bodies or credentials. The tool writes it with
an atomic replacement. A sync that applies changes with a cache also holds
`<state_file>.lock` to stop overlap.

A missing, corrupt, invalid, or incomplete cache starts a full rebuild from
Fastmail and Gmail. A rebuild validates message identity and adoption matches
against the Gmail inventory before changing Gmail.

Without the cache, the tool cannot link a renamed Fastmail mailbox to its old
Gmail label. It creates the new label and leaves the old one in place. This
protects Gmail labels that the user created.

Use `--no-cache` to rebuild on every run:

```sh
go run ./cmd/jmapsync sync -config config.json --no-cache
```

This mode creates no state, state lock, or temporary state file. It makes more
API calls and needs an external lock to prevent overlapping syncs.

## Cron

Run the tool once every 5–15 minutes. It is a one-shot command, not a daemon,
and it keeps no persistent connection. Default output is one summary line;
add `--verbose` for details on stderr.

Use `flock` to prevent cacheless runs from overlapping. This external lock also
works for cached runs:

```cron
*/10 * * * * flock -n ~/.local/state/jmapsync/cron.lock /path/to/jmapsync sync -config /path/to/config.json --no-cache
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
- Gmail may reject changes to `SENT` and `DRAFT`. The defaults use these labels
  for IMAP-like behavior. If Gmail rejects them, use ordinary labels:

  ```json
  "role_labels": {
    "sent": "Fastmail_Sent",
    "drafts": "Fastmail_Drafts"
  }
  ```

- Gmail performs final MIME and size checks. If Gmail rejects a message, it
  stays unmapped, and the JMAP cursor does not advance. The next run tries it
  again.

## Development

```sh
make check
```

See `DESIGN.md` for the sync rules. Tests use local fakes; they do not run
against live Fastmail or Gmail accounts.
