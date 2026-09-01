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
	configPath := flags.String("config", "config.json", "path to configuration JSON")
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
	configPath := flags.String("config", "config.json", "path to configuration JSON")
	apply := flags.Bool("apply", false, "apply planned Gmail changes; cache them unless --no-cache")
	full := flags.Bool("full", false, "scan all Fastmail mail instead of using the JMAP cursor")
	noCache := flags.Bool("no-cache", false, "rebuild from servers; create no state or state-lock files")
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
	stateStore, closeState, err := syncStateBackend(*noCache, *apply, settings.StateFile)
	if err != nil {
		return err
	}
	defer closeState()
	tokenData, err := os.ReadFile(settings.Fastmail.TokenFile)
	if err != nil {
		return fmt.Errorf("read Fastmail API token: %w", err)
	}
	source, err := fastmail.New(strings.TrimSpace(string(tokenData)), settings.Fastmail.SessionEndpoint)
	if err != nil {
		return err
	}
	httpClient, err := gmailapi.AuthenticatedClient(ctx, settings.Gmail.CredentialsFile, settings.Gmail.TokenFile)
	if err != nil {
		return err
	}
	destination, err := gmailapi.New(ctx, httpClient, settings.Gmail.User)
	if err != nil {
		return err
	}
	if !*apply {
		fmt.Fprintln(stdout, "DRY RUN: no Gmail, cache, lock, or OAuth-token writes")
	}
	engine := &syncer.Engine{
		Source: source, Destination: destination,
		Store: stateStore,
		Options: syncer.Options{
			Apply: *apply, Delete: settings.Delete, Full: *full,
			TrashLabel: settings.TrashLabel, SpamLabel: settings.SpamLabel,
			RoleLabels: settings.RoleLabels, Output: stdout,
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

Plans use zero persistent writes by default. Use --apply to change Gmail and update the optional cache.
Run this one-shot command from cron every 5-15 minutes. It has no daemon mode.
--no-cache keeps state only in memory and creates no state or lock. Prevent overlap with an external lock.
New inserts carry one X-Jmapsync-Account header, one X-Jmapsync-Email-ID header, and one account-specific jmapsync-managed label.
The tool adopts existing mail only when one source and one Gmail message share a Message-ID.
Source deletions use Gmail Trash. The tool ignores Gmail-native SPAM.
Google access uses only gmail.modify.
Config and credential/token files are required inputs. The state cache is optional.
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
	fmt.Fprintln(output, `usage: jmapsync <command> [options]

commands:
  google-auth  authorize the Gmail API and save a refresh token
  sync         plan a mirror run; add --apply to change Gmail

run "jmapsync <command> -h" for command options`)
}
