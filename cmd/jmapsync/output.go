package main

import (
	"context"
	"fmt"
	"io"

	"jmapsync/internal/syncer"
)

func syncDiagnosticWriter(verbose bool, stderr io.Writer) io.Writer {
	if verbose {
		return stderr
	}
	return io.Discard
}

func runSyncEngine(ctx context.Context, engine *syncer.Engine, stdout io.Writer, dryRun bool) error {
	result, err := engine.Run(ctx)
	if err != nil {
		return err
	}
	prefix := "summary:"
	if dryRun {
		prefix = "DRY RUN summary:"
	}
	fmt.Fprintf(stdout,
		"%s labels-created=%d labels-renamed=%d inserted=%d adopted=%d modified=%d trashed=%d trash-disabled=%d spam-ignored=%d unchanged=%d\n",
		prefix, result.LabelsCreated, result.LabelsRenamed, result.Inserted, result.Adopted,
		result.Modified, result.Trashed, result.SkippedTrash, result.IgnoredSpam, result.Unchanged,
	)
	return nil
}
