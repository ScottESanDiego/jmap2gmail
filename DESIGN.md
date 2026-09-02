# jmapsync design

Fastmail is the source of truth. JMAP reads Fastmail. Gmail's
`users.messages.insert` writes Gmail.

The program is a one-shot command for cron every 5–15 minutes. It has no daemon
mode and keeps no persistent connection.

## Identity and recovery

One JMAP Email maps to one Gmail message. Each new Gmail insert gets one
`X-Jmapsync-Account` and one `X-Jmapsync-Email-ID` header. The insert path
removes source-supplied copies first.

The account value is a stable hash of the Fastmail account identity. The tool
also adds `jmapsync-managed/<account-hash>`. Every normal reconcile preserves
that label.

The JMAP Email ID stays stable across mailbox moves. The two headers and marker
label form the durable mapping. They let the tool recover after a crash or lost
cache without inserting a duplicate.

Initial adoption requires one source and one Gmail message to share an RFC
Message-ID. Adoption adds the marker label but cannot rewrite the Gmail
message's headers. Gmail blocks header changes after insertion.

The rebuild preflight rejects bad or conflicting identity headers. It also
rejects missing legacy IDs, duplicate Gmail matches, and shared source matches.
The failure occurs before Gmail changes and tells the user what to repair.

## Mirror rules

- Mailbox membership and supported keywords map to Gmail labels.
- Fastmail Trash and Junk use `Fastmail_Trash` and `Fastmail_Spam`.
- Source deletions move mapped Gmail messages to Gmail Trash. Unmapped messages
  stay unchanged.
- The tool never calls Gmail's permanent-delete API.
- Gmail-native `SPAM` is outside the mirror. Discovery, rebuild, adoption,
  reconcile, and deletion handling all ignore it.
- The next reconcile reverts Gmail-side label changes.
- The tool inserts a mapped message again if it vanishes from Gmail.

## Cache

The optional JSON state is an API-call cache. Message sync remains correct
without it. The cache stores:

- authenticated account identities
- JMAP and Gmail History cursors
- JMAP Email ID to Gmail ID mappings and source-deletion marks
- Fastmail mailbox to Gmail label ID and name mappings

It stores no message body or credential. Cached apply runs use
`<state_file>.lock`. State writes use a temporary file and an atomic rename.

A missing, corrupt, invalid, unsupported, or incomplete state file requests a
full server rebuild. The preflight reads a full Fastmail snapshot and all
marked Gmail metadata before any Gmail change.

The tool marks every cached apply run as incomplete before any Gmail change. A
failed final state commit therefore forces a full rebuild.

`--no-cache` uses in-memory state and always rebuilds. It creates no cache,
state lock, or state temporary file. Operators must stop concurrent cacheless
runs with an external lock such as `flock`.

The mailbox mapping also remembers label renames. Without it, the tool cannot
prove that an old Gmail label came from a renamed Fastmail mailbox. It creates
the new label and leaves the stale label in place to protect user-created
labels.

## Run flow

1. Load the cache, or prepare a full rebuild.
2. Bind the authenticated Fastmail and Gmail identities.
3. Read Fastmail mailboxes and derive Gmail labels.
4. If rebuilding, validate all marked mail and adoption candidates.
5. Create or rename labels, then reconcile Fastmail mail.
6. Move deleted source mail to Gmail Trash when the config enables deletion.
7. Reconcile Gmail History changes and save new cursors.

JMAP and Gmail History cursors make cached runs incremental. An absent or
expired cursor causes a full scan or full mapped-message reconcile.

State saves follow each successful mutation. Cursors advance after all earlier
work succeeds. A message without an RFC Message-ID gets a deterministic
`@invalid` value during insertion.

## Dry run

`sync` plans by default. `--apply` enables Gmail changes and cache writes. A dry
run lists each planned label create or rename, insert, adoption, label change,
Trash move, and native-spam ignore.

A dry run mutates no Gmail or Fastmail data. It also writes no cache, state
lock, temporary state, or OAuth token. It may read an existing cache and needs
existing credential files.

## Permissions and libraries

The Gmail scope is `gmail.modify`. The tool needs it to read metadata and
history, insert messages, manage labels, and move mail to Trash. The scope does
not grant immediate permanent deletion, and the tool requests no broader
scope.

- `git.sr.ht/~rockorager/go-jmap` handles JMAP and blob downloads.
- `google.golang.org/api/gmail/v1` handles Gmail.
- `golang.org/x/oauth2` handles Google OAuth.

Automated tests use fakes and local HTTP servers. They make no live Gmail or
Fastmail changes.
