# jmapsync

`jmapsync` mirrors Fastmail to Gmail. Fastmail remains the source of truth. The
tool reads Fastmail through JMAP and inserts mail through the Gmail API.

It uses [`go-jmap`](https://sr.ht/~rockorager/go-jmap/) and Google's official Go
clients.

## Behavior

- Fastmail mailboxes become Gmail labels, including nested mailboxes.
- `$seen` maps to `UNREAD`; `$flagged` maps to `STARRED`.
- Fastmail Trash maps to `Fastmail_Trash`.
- Fastmail Junk maps to `Fastmail_Spam`.
- Gmail-native `SPAM` is outside the mirror. The tool does not discover, adopt,
  relabel, or trash those messages.
- Source deletions move mapped Gmail messages to Trash when `"delete"` is
  `true`. The tool never calls Gmail's permanent-delete API.

Each inserted message gets one `X-Jmapsync-Account` header and one
`X-Jmapsync-Email-ID` header. The tool strips source copies of both headers
before insertion. It also adds this per-account Gmail label:

```text
jmapsync-managed/<account-hash>
```

The JMAP Email ID does not change when mail moves between Fastmail mailboxes.
The headers and label let the tool rebuild its map after cache loss. Normal
syncs restore the label if another client removes it.

On the first run, the tool may adopt existing Gmail mail by RFC Message-ID. It
adopts one clear match and adds the managed label. Gmail messages cannot
gain new headers after insertion, so adopted mail keeps its original headers.

The tool checks all adoption candidates before it changes Gmail. A missing
candidate ID, shared source ID, duplicate match, or conflicting identity stops
the run with repair guidance.

## Setup

Requires Go 1.25 or newer.

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

   The tool requests `gmail.modify` and no broader scope. This scope can read
   and change mail, manage labels, insert messages, and move messages to Trash.
   It does not grant the API permission to delete messages at once.

   If an old token granted `mail.google.com`, revoke that grant. Then remove the
   token file and authorize again.

5. Preview and apply the first run:

   ```sh
   go run ./cmd/jmapsync sync -config config.json --full
   go run ./cmd/jmapsync sync -config config.json --full --apply
   ```

`sync` is a dry run unless you pass `--apply`. It lists planned label creation,
renames, inserts, adoptions, label changes, Trash moves, and ignored native
spam. It also reports that Gmail and state stay unchanged.

A dry run reads Fastmail, Gmail, the config, credentials, and any existing
cache. It writes no Gmail data, cache, lock, temporary state, or OAuth token.
The required credential and token files must already exist.

## Optional cache

The default state file is optional for message sync. It saves JMAP and Gmail
calls and tracks label rename history. It keeps these items:

- Fastmail and Gmail account identities
- JMAP and Gmail History cursors
- JMAP Email ID to Gmail message ID mappings and deletion marks
- Fastmail mailbox to Gmail label mappings

The cache contains no message bodies or credentials. The tool uses an atomic
write. An `--apply` run with a cache also holds `<state_file>.lock` to stop
overlap.

A missing, corrupt, invalid, or incomplete cache starts a full rebuild from
Fastmail and Gmail. A rebuild checks the managed headers and label before it
changes Gmail.

Without the cache, the tool cannot link a renamed Fastmail mailbox to its old
Gmail label. It creates the new label and leaves the old one in place. This
protects Gmail labels that the user created.

Use `--no-cache` to rebuild on every run:

```sh
go run ./cmd/jmapsync sync -config config.json --no-cache --apply
```

This mode creates no state, state lock, or temporary state file. It makes more
API calls. It also needs an external lock because separate runs share no local
state.

## Cron

Run the tool once every 5–15 minutes. It is a one-shot command, not a daemon,
and it keeps no persistent connection.

Use `flock` to prevent cacheless runs from overlapping. The wrapper's lock is
separate from jmapsync and remains useful for cached runs:

```cron
*/10 * * * * flock -n ~/.local/state/jmapsync/cron.lock /path/to/jmapsync sync -config /path/to/config.json --no-cache --apply
```

Cached runs may omit `--no-cache`. Their state lock also blocks overlap.

## Files

- `config.json`: settings and paths; protect it if it reveals private paths
- Fastmail token: a credential
- Google client JSON: OAuth client data
- Google OAuth token: `google-auth` creates this credential
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
