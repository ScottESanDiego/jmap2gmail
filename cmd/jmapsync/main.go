package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"jmapsync/internal/config"
	"jmapsync/internal/fastmail"
	"jmapsync/internal/gmailapi"
	"jmapsync/internal/store"
	"jmapsync/internal/syncer"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "jmapsync:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stderr)
		return errors.New("a command is required")
	}
	switch args[0] {
	case "google-auth":
		return runGoogleAuth(ctx, args[1:], stdout, stderr)
	case "sync":
		return runSync(ctx, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return nil
	default:
		printUsage(stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runGoogleAuth(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("google-auth", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "config.json", "path to the JSON config file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("google-auth takes no positional arguments")
	}
	settings, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if err := settings.ValidateForAuth(); err != nil {
		return err
	}
	return gmailapi.Authorize(ctx, settings.Gmail.CredentialsFile, settings.Gmail.TokenFile, stdout)
}

func runSync(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("sync", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "config.json", "path to the JSON config file")
	dryRun := flags.Bool("dry-run", false, "preview changes without Gmail, cache, lock, or OAuth token writes")
	full := flags.Bool("full", false, "scan the full Fastmail view instead of using saved change history")
	noCache := flags.Bool("no-cache", false, "rebuild from servers without a state cache or state lock")
	verbose := flags.Bool("verbose", false, "show configuration and Fastmail folder selection diagnostics on stderr")
	flags.Usage = func() { printSyncUsage(stderr, flags) }
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("sync takes no positional arguments")
	}
	settings, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if err := settings.ValidateForSync(); err != nil {
		return err
	}
	if *verbose {
		fmt.Fprintf(stderr, "verbose: config=%q state=%q dry-run=%t full=%t no-cache=%t delete=%t\n", diagnosticPath(*configPath), diagnosticPath(settings.StateFile), *dryRun, *full, *noCache, settings.Delete)
		fmt.Fprintf(stderr, "verbose: configured mailbox selectors: %q\n", settings.Fastmail.IncludeMailboxes)
	}
	apply := !*dryRun
	authenticateSource := func() (*fastmail.Client, error) {
		tokenData, err := os.ReadFile(settings.Fastmail.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("read Fastmail API token: %w", err)
		}
		return fastmail.New(strings.TrimSpace(string(tokenData)), settings.Fastmail.SessionEndpoint)
	}
	var source *fastmail.Client
	if *verbose || len(settings.Fastmail.IncludeMailboxes) != 0 {
		source, err = authenticateSource()
		if err != nil {
			return err
		}
		mailboxes, err := source.Mailboxes(ctx)
		if err != nil {
			return err
		}
		var paths map[string]string
		if *verbose {
			paths, err = syncer.MailboxPaths(mailboxes)
			if err != nil {
				return err
			}
			printMailboxDiagnostics(stderr, "available", paths, nil)
		}
		selected, err := syncer.ResolveMailboxIDs(mailboxes, settings.Fastmail.IncludeMailboxes)
		if err != nil {
			if *verbose {
				printMailboxSelectorHints(stderr, settings.Fastmail.IncludeMailboxes)
			}
			return err
		}
		if *verbose {
			printMailboxDiagnostics(stderr, "selected", paths, selected)
		}
	}
	stateStore, closeState, err := syncStateBackend(*noCache, apply, settings.StateFile)
	if err != nil {
		return err
	}
	defer closeState()
	if source == nil {
		source, err = authenticateSource()
		if err != nil {
			return err
		}
	}
	httpClient, err := gmailapi.AuthenticatedClient(ctx, settings.Gmail.CredentialsFile, settings.Gmail.TokenFile, apply)
	if err != nil {
		return err
	}
	destination, err := gmailapi.New(ctx, httpClient, settings.Gmail.User, gmailapi.ClientOptions{
		QuotaUnitsPerMinute: settings.Gmail.QuotaUnitsPerMinute,
		Diagnostics:         stderr,
	})
	if err != nil {
		return err
	}
	if *dryRun {
		fmt.Fprintln(stdout, "DRY RUN: no Gmail, cache, lock, or OAuth-token writes")
	}
	engine := &syncer.Engine{
		Source: source, Destination: destination,
		Store: stateStore,
		Options: syncer.Options{
			Apply: apply, Delete: settings.Delete, Full: *full,
			TrashLabel: settings.TrashLabel, SpamLabel: settings.SpamLabel,
			RoleLabels: settings.RoleLabels, Output: stdout,
			IncludeMailboxes: settings.Fastmail.IncludeMailboxes,
		},
	}
	result, err := engine.Run(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout,
		"summary: labels-created=%d labels-renamed=%d inserted=%d adopted=%d modified=%d trashed=%d trash-disabled=%d spam-ignored=%d unchanged=%d\n",
		result.LabelsCreated, result.LabelsRenamed, result.Inserted, result.Adopted,
		result.Modified, result.Trashed, result.SkippedTrash, result.IgnoredSpam, result.Unchanged,
	)
	return nil
}

const syncHelp = `Usage: jmapsync sync [options]

Mirror Fastmail to Gmail. Fastmail is the source of truth.
Sync applies Gmail changes and updates the optional cache by default.
Use --dry-run to preview with zero persistent writes.
Run once, or schedule with cron every 5-15 minutes. There is no daemon mode.

Cache and credentials:
  --no-cache keeps state in memory. Use an external lock to prevent overlap.
  Sync saves OAuth renewals under a separate token lock, even with --no-cache.
  Dry runs create no locks and save no tokens.
  Config and credential/token files are required. The state cache is optional.
  Google access uses only gmail.modify.

Message handling:
  New inserts carry one X-Jmapsync-Account header, one X-Jmapsync-Email-ID
  header, and one account-specific jmapsync-managed label.
  Adopt existing mail only when one source message and one Gmail message
  share a Message-ID.
  With delete enabled, every non-SPAM Gmail message absent from the Fastmail
  source view moves to Trash, including unrelated and unmarked Gmail mail.
  With delete disabled, extra Gmail mail stays. The tool never deletes permanently.
  Gmail-native SPAM stays unchanged.

Folder selection:
  Set fastmail.include_mailboxes to case-sensitive full source paths.
  Exact paths select one folder. A trailing /* selects all descendants at any
  depth, excluding the parent. Include the parent with a separate exact path.
  "Mailing Lists/*" includes "Mailing Lists/Gentoo" and "Mailing Lists/Mailers".
  Each selector must match; other * positions are invalid. Overlap counts once.
  Omit the field or use [] for all mail. There is no folder-selection flag.
  The selection defines the source view in every mode, including --no-cache.
  Narrowing the config can move all non-SPAM Gmail mail outside the view to
  Trash when delete is true, including mail this tool never inserted or marked.
  Other Gmail labels on included mail stay unchanged. Preview with --dry-run.

Progress and timeouts:
  First runs and full rebuilds scan mail before listing message actions.
  Initial scans read all Gmail metadata, even for a small Fastmail selection.
  Rebuild phases, Fastmail metadata counts, and local matching counts show progress.
  Updates run between operations; a pending request can delay output.
  Each Gmail HTTP attempt times out after 60 seconds, excluding quota waits.
  gmail.quota_units_per_minute defaults to 4800; all reads and writes are paced.
  Explicit minute quota limits retry until success or cancellation.
  Pacing and retry waits appear on stderr, including during dry runs.

go-jmap reads Fastmail. Official Google API and OAuth clients access Gmail.

Options:
`

func printSyncUsage(output io.Writer, flags *flag.FlagSet) {
	fmt.Fprint(output, syncHelp)
	flags.PrintDefaults()
}

func syncStateBackend(noCache, apply bool, statePath string) (syncer.StateStore, func(), error) {
	if noCache {
		return store.NewMemory(), func() {}, nil
	}
	if !apply {
		return store.File{Path: statePath}, func() {}, nil
	}
	lock, err := store.AcquireLock(statePath)
	if err != nil {
		return nil, nil, err
	}
	return store.File{Path: statePath}, func() { _ = lock.Close() }, nil
}

func printUsage(output io.Writer) {
	fmt.Fprintln(output, `Usage: jmapsync <command> [options]

Commands:
  google-auth  authorize the Gmail API and save a refresh token
  sync         mirror Fastmail to Gmail; add --dry-run to preview

Run "jmapsync <command> -h" for command options.`)
}
