package syncer

import (
	"context"
	"fmt"
	"mime"
	"net/mail"
	"slices"
	"strings"
	"time"
)

// ConflictSummary contains human metadata fetched only after an identity conflict.
// It must never contain message bodies, previews, or raw messages.
type ConflictSummary struct {
	Subject, From, To, Cc, SentAt, ReceivedAt string
	LocationIDs                               []string
	ThreadID                                  string
}

type SourceConflictMetadata interface {
	ConflictEmails(context.Context, []string) (map[string]ConflictSummary, error)
}
type DestinationConflictMetadata interface {
	ConflictMessage(context.Context, string) (ConflictSummary, error)
}

const conflictTimeout = 10 * time.Second
const conflictLimit = 20

func (e *Engine) conflictError(ctx context.Context, cause error, messageID string, sourceIDs []string, candidates []Message) error {
	return e.conflictErrorWithin(ctx, cause, messageID, sourceIDs, candidates, conflictTimeout)
}

func (e *Engine) conflictErrorWithin(ctx context.Context, cause error, messageID string, sourceIDs []string, candidates []Message, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sourceIDs = slices.Clone(sourceIDs)
	slices.Sort(sourceIDs)
	sourceIDs = slices.Compact(sourceIDs)
	candidates = slices.Clone(candidates)
	slices.SortFunc(candidates, func(a, b Message) int { return strings.Compare(a.ID, b.ID) })
	var out strings.Builder
	fmt.Fprintf(&out, "Message-ID: %s\n", conflictValue(messageID))
	if safeSearchID(messageID) {
		fmt.Fprintf(&out, "Search Gmail:\n  %s\nSearch Fastmail:\n  %s\n", "in:anywhere -in:spam rfc822msgid:"+conflictSearchTerm(messageID), "in:* msgid:"+conflictSearchTerm(messageID))
	} else {
		out.WriteString("Search queries: unavailable (Message-ID cannot safely form a literal search term)\n")
	}
	paths, _ := MailboxPaths(e.sourceMailboxes)
	labels := make(map[string]string)
	for id, name := range e.labelNames {
		labels[id] = name
	}
	for _, label := range e.labels {
		labels[label.ID] = label.Name
	}
	summaries := map[string]ConflictSummary{}
	if provider, ok := e.Source.(SourceConflictMetadata); ok && ctx.Err() == nil {
		if values, err := provider.ConflictEmails(ctx, sourceIDs[:min(len(sourceIDs), conflictLimit)]); err == nil {
			summaries = values
		}
	}
	fmt.Fprintf(&out, "Fastmail candidates: %d (showing %d)\n", len(sourceIDs), min(len(sourceIDs), conflictLimit))
	for _, id := range sourceIDs[:min(len(sourceIDs), conflictLimit)] {
		summary := summaries[id]
		if summary.LocationIDs == nil {
			mailboxIDs := e.snapshot[id].MailboxIDs
			if mailboxIDs == nil && e.state != nil {
				mailboxIDs = e.state.SourceEmails[id].MailboxIDs
			}
			for mailboxID, present := range mailboxIDs {
				if present {
					summary.LocationIDs = append(summary.LocationIDs, mailboxID)
				}
			}
		}
		writeConflictSummary(&out, summary, "folders", paths)
		fmt.Fprintf(&out, "  Fastmail email ID: %q\n", id)
	}
	fmt.Fprintf(&out, "Gmail candidates: %d (showing %d)\n", len(candidates), min(len(candidates), conflictLimit))
	for _, candidate := range candidates[:min(len(candidates), conflictLimit)] {
		summary := ConflictSummary{}
		if provider, ok := e.Destination.(DestinationConflictMetadata); ok && ctx.Err() == nil {
			if value, err := provider.ConflictMessage(ctx, candidate.ID); err == nil {
				summary = value
			}
		}
		if summary.LocationIDs == nil {
			summary.LocationIDs = candidate.LabelIDs
		}
		writeConflictSummary(&out, summary, "labels", labels)
		fmt.Fprintf(&out, "  Gmail message ID: %q; thread ID: %s\n", candidate.ID, conflictValue(summary.ThreadID))
	}
	if len(sourceIDs) > conflictLimit || len(candidates) > conflictLimit {
		out.WriteString("Candidate details truncated to 20 per account; counts above include all eligible candidates.\n")
	}
	if len(sourceIDs) > 1 && messageID != "" {
		out.WriteString("Several Fastmail messages share this Message-ID; these may be valid source copies. An existing Gmail copy cannot be assigned to a particular Fastmail message from Message-ID alone. Preserve legitimate Fastmail copies; restore a known-good mapping or repair/re-import identities only after backup and inspection.\n")
	} else if len(candidates) > 1 {
		out.WriteString("Compare the Gmail matches with the Fastmail message before resolving the extra Gmail candidates.\n")
	}
	out.WriteString("Inspect these candidates in the correct Fastmail and Gmail accounts before resolving their mapping. Matching Message-IDs do not prove identical copies. Preserve Fastmail originals; do not blindly delete source mail. Restore a known-good cache when available, or resolve the conflicting Gmail copies/marker labels after inspection, then retry.")
	return fmt.Errorf("%w\n%s", cause, out.String())
}

func safeSearchID(id string) bool {
	if strings.Count(id, "@") != 1 {
		return false
	}
	local, domain, _ := strings.Cut(id, "@")
	for _, part := range []string{local, domain} {
		for _, component := range strings.Split(part, ".") {
			if component == "" {
				return false
			}
		}
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@.!#$%&'*+-/=?^_`{|}~", r)) {
			return false
		}
	}
	return true
}

// Quote punctuation with search syntax while preserving the literal Message-ID.
func conflictSearchTerm(id string) string {
	for _, r := range id {
		if strings.ContainsRune("!#$%&'*?^`{|}~", r) {
			return "\"" + id + "\""
		}
	}
	return id
}

func conflictValue(value string) string {
	if value == "" {
		return "unavailable"
	}
	if decoded, err := new(mime.WordDecoder).DecodeHeader(value); err == nil {
		value = decoded
	}
	// Quote control bytes as escapes so mail headers cannot inject terminal output.
	runes := []rune(value)
	if len(runes) > 1024 {
		value = string(runes[:1024]) + " [truncated]"
	}
	return fmt.Sprintf("%q", value)
}

func conflictDate(value string) string {
	if parsed, err := mail.ParseDate(value); err == nil {
		return parsed.Format(time.RFC3339)
	}
	return value
}

func writeConflictSummary(out *strings.Builder, s ConflictSummary, location string, names map[string]string) {
	fmt.Fprintf(out, "  subject=%s; from=%s; to=%s; cc=%s\n  sent=%s; received=%s\n", conflictValue(s.Subject), conflictValue(s.From), conflictValue(s.To), conflictValue(s.Cc), conflictValue(conflictDate(s.SentAt)), conflictValue(s.ReceivedAt))
	locations := make([]string, 0, len(s.LocationIDs))
	for _, id := range s.LocationIDs {
		if name := names[id]; name != "" {
			locations = append(locations, conflictValue(name))
		} else {
			locations = append(locations, "unavailable (ID "+fmt.Sprintf("%q", id)+")")
		}
	}
	slices.Sort(locations)
	if len(locations) == 0 {
		locations = append(locations, "unavailable")
	}
	fmt.Fprintf(out, "  %s: %s\n", location, strings.Join(locations, ", "))
}
