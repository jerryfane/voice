package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jerryfane/voice/internal/config"
	"github.com/jerryfane/voice/internal/proposal"
	"github.com/jerryfane/voice/internal/session"
)

// This file is the whole command-line surface of the proposal store, and it is
// the only surface the owner's broker has. The broker runs as the owner, holds
// the GitHub credentials the restricted account must never see, and reaches
// the database exclusively through
//
//	sudo -n -u voice-agent /usr/local/bin/voice --config ... proposals ...
//
// so every exit status and every line printed here is an interface, not a
// convenience. A command that reported failure as a friendly message on stdout
// and exited 0 would have the broker file an issue for a proposal nobody
// approved.

// proposalStatuses is the order statuses are reported in: the life of a
// proposal from asked to finished. Ranging over the count map instead would
// shuffle the lines between runs of `voice doctor`, which makes two healthy
// reports look like a change.
var proposalStatuses = []proposal.Status{
	proposal.Pending,
	proposal.Notified,
	proposal.Approved,
	proposal.Declined,
	proposal.IssueCreated,
	proposal.Implementing,
	proposal.Completed,
	proposal.Failed,
	proposal.Expired,
}

const recordUsage = "usage: voice proposals record REQUEST [--title T] [--scope S] [--risks R]"

// proposalsCommand runs one `voice proposals ...` subcommand.
func proposalsCommand(a *session.Assistant, c config.Config, loaded string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: voice proposals list|show|due|record|notified|approve|decline|issue|implementing|complete|fail|expire")
	}
	store, done, err := proposalStore(a, c, loaded)
	if err != nil {
		return err
	}
	// The store may be this process's own handle, in which case closing it is
	// somebody else's business; either way the close error is part of the
	// command's result, because a write that never reached the disk is the
	// failure this whole feature exists to prevent.
	return errors.Join(proposals(store, c, args), done())
}

// proposalStore returns the open store, plus the close this command owes.
//
// The session already opened the database when the feature is enabled, so the
// normal path reuses that handle rather than taking a second SQLite connection
// to the same file and racing itself for the write lock. A nil store means
// either the feature is off or the open failed, and those are different
// answers: the first is a configuration choice and the second is a fault, so
// the second is re-attempted here to put the real reason in the error.
func proposalStore(a *session.Assistant, c config.Config, loaded string) (*proposal.Store, func() error, error) {
	if !c.Proposals.Enabled {
		where := "the config"
		if loaded != "" {
			where = loaded
		}
		return nil, nil, fmt.Errorf("proposals are disabled: set proposals.enabled to true in %s to record what Voice was asked for and could not do", where)
	}
	if a.Proposals != nil {
		return a.Proposals, func() error { return nil }, nil
	}
	path, err := proposalsFile(c.Proposals)
	if err != nil {
		return nil, nil, err
	}
	store, err := proposal.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return store, store.Close, nil
}

// proposalsFile resolves proposals.file the same way the session does, so the
// CLI and the running service never disagree about which database holds the
// record.
func proposalsFile(p config.Proposals) (string, error) {
	if p.File != "" {
		return p.File, nil
	}
	return config.StatePath("proposals.db")
}

func proposals(s *proposal.Store, c config.Config, args []string) error {
	now := time.Now()
	switch args[0] {
	case "list":
		asJSON, err := jsonFlag("list", args[1:])
		if err != nil {
			return err
		}
		all, err := s.List()
		if err != nil {
			return err
		}
		if asJSON {
			return printProposalsJSON(all)
		}
		if len(all) == 0 {
			stdout.println("No proposals recorded.")
			return nil
		}
		for _, p := range all {
			printProposalLine(p)
		}
		return nil
	case "show":
		if len(args) != 2 {
			return errors.New("usage: voice proposals show ID")
		}
		p, err := s.Get(args[1])
		if err != nil {
			return proposalError(args[1], err)
		}
		events, err := s.Events(p.ID)
		if err != nil {
			return err
		}
		printProposal(p, events)
		return nil
	case "due":
		asJSON, err := jsonFlag("due", args[1:])
		if err != nil {
			return err
		}
		// Reading only: the broker asks what it should carry to the owner,
		// then records `notified` once the question is actually asked. A due
		// list that marked its own rows as notified would lose the question
		// whenever the broker died between the two.
		due, err := s.DueForNotice(now, proposal.Notice{
			MaxPerHour: c.Proposals.MaxPerHour,
			RetryAfter: c.Proposals.RetryAfter.D(),
		})
		if err != nil {
			return err
		}
		if asJSON {
			return printProposalsJSON(due)
		}
		if len(due) == 0 {
			stdout.println("Nothing is waiting for the owner.")
			return nil
		}
		for _, p := range due {
			printProposalLine(p)
		}
		return nil
	case "record":
		p, err := recordArgs(args[1:])
		if err != nil {
			return err
		}
		stored, created, err := s.Record(p, now)
		if err != nil {
			return err
		}
		if created {
			stdout.printf("%s recorded as %s\n", stored.ID, stored.Status)
			return nil
		}
		stdout.printf("%s already recorded as %s, asked %d time(s)\n", stored.ID, stored.Status, stored.Occurrences)
		return nil
	case "notified":
		return transition(s, args, proposal.Notified, "the owner was asked")
	case "approve":
		return transition(s, args, proposal.Approved, "approved by the owner")
	case "complete":
		return transition(s, args, proposal.Completed, "completed")
	case "decline":
		if len(args) < 2 {
			return errors.New("usage: voice proposals decline ID [REASON]")
		}
		// A reason is optional but kept verbatim when given: "not now, the
		// microphone work comes first" is the answer to the next person who
		// asks why this was never built.
		detail := strings.TrimSpace(strings.Join(args[2:], " "))
		if detail == "" {
			detail = "declined by the owner"
		}
		return transitionTo(s, args[1], proposal.Declined, detail, now)
	case "fail":
		if len(args) < 3 {
			return errors.New("usage: voice proposals fail ID DETAIL")
		}
		// Required, unlike a decline reason: a failure nobody described is a
		// proposal that stopped moving for reasons the store cannot report.
		return transitionTo(s, args[1], proposal.Failed, strings.Join(args[2:], " "), now)
	case "issue":
		if len(args) != 3 {
			return errors.New("usage: voice proposals issue ID NUMBER")
		}
		n, err := strconv.Atoi(args[2])
		if err != nil {
			return fmt.Errorf("issue number must be a positive integer, got %q", args[2])
		}
		p, err := s.SetIssue(args[1], n, now)
		if err != nil {
			return proposalError(args[1], err)
		}
		stdout.printf("%s is now %s, issue #%d\n", p.ID, p.Status, p.Issue)
		return nil
	case "implementing":
		if len(args) != 3 {
			return errors.New("usage: voice proposals implementing ID AGENT")
		}
		p, err := s.SetAgent(args[1], args[2], now)
		if err != nil {
			return proposalError(args[1], err)
		}
		stdout.printf("%s is now %s in %s\n", p.ID, p.Status, p.Agent)
		return nil
	case "retry":
		if len(args) != 2 {
			return errors.New("usage: voice proposals retry ID")
		}
		// Only for a step that failed AFTER the owner approved: the store
		// checks the audit trail for that approval, so this cannot revive a
		// proposal the owner declined or never answered. It exists because
		// the first real approval filed its issue and then failed to launch
		// the seat, leaving work the owner had authorized with nobody on it.
		p, err := s.Retry(args[1], now)
		if err != nil {
			return proposalError(args[1], err)
		}
		stdout.printf("%s is back to %s; the broker's next pass will carry on\n", p.ID, p.Status)
		return nil
	case "expire":
		if len(args) != 1 {
			return errors.New("usage: voice proposals expire")
		}
		gone, err := s.Expire(now, c.Proposals.Expire.D())
		if err != nil {
			return err
		}
		if len(gone) == 0 {
			stdout.println("Nothing expired.")
			return nil
		}
		stdout.printf("%d proposal(s) expired after %s unanswered:\n", len(gone), c.Proposals.Expire.D())
		for _, p := range gone {
			printProposalLine(p)
		}
		return nil
	default:
		return fmt.Errorf("unknown proposals command %q: try list, show, due, record, notified, approve, decline, issue, implementing, retry, complete, fail or expire", args[0])
	}
}

// transition handles the subcommands whose only argument is an ID.
func transition(s *proposal.Store, args []string, to proposal.Status, detail string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: voice proposals %s ID", args[0])
	}
	return transitionTo(s, args[1], to, detail, time.Now())
}

func transitionTo(s *proposal.Store, id string, to proposal.Status, detail string, now time.Time) error {
	p, err := s.Transition(id, to, detail, now)
	if err != nil {
		return proposalError(id, err)
	}
	stdout.printf("%s is now %s: %s\n", p.ID, p.Status, p.Detail)
	return nil
}

// proposalError names the ID an unknown one was asked for. The broker passes
// IDs it read from an earlier `due --json`, so a mismatch means the database
// moved, not that the owner mistyped, and the ID is the only thread back to
// which question went unanswered.
func proposalError(id string, err error) error {
	if errors.Is(err, proposal.ErrNotFound) {
		return fmt.Errorf("no proposal %s on record: `voice proposals list` shows the ones there are", id)
	}
	return err
}

// recordArgs parses REQUEST plus the optional interpretation flags.
//
// Hand-parsed rather than handed to a FlagSet because the request comes first
// and may be several unquoted words: flag.Parse stops at the first
// non-flag argument, so `record turn the volume down --title Volume` would
// have silently ignored the title.
func recordArgs(args []string) (proposal.Proposal, error) {
	var p proposal.Proposal
	fields := map[string]*string{"title": &p.Title, "scope": &p.Scope, "risks": &p.Risks}
	var words []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			words = append(words, arg)
			continue
		}
		name, value, inline := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		target, ok := fields[name]
		if !ok {
			return p, fmt.Errorf("unknown flag %q: %s", arg, recordUsage)
		}
		if !inline {
			if i+1 >= len(args) {
				return p, fmt.Errorf("%s needs a value: %s", arg, recordUsage)
			}
			i++
			value = args[i]
		}
		*target = value
	}
	p.Request = strings.Join(words, " ")
	if strings.TrimSpace(p.Request) == "" {
		return p, errors.New(recordUsage)
	}
	return p, nil
}

// jsonFlag parses the single flag list and due accept.
func jsonFlag(command string, args []string) (bool, error) {
	if len(args) == 1 && (args[0] == "--json" || args[0] == "-json") {
		return true, nil
	}
	if len(args) == 0 {
		return false, nil
	}
	return false, fmt.Errorf("usage: voice proposals %s [--json]", command)
}

// proposalView is the JSON shape. It exists rather than tags on
// proposal.Proposal because this is a published interface - the owner's broker
// reads these keys with jq - and it must not change silently when a field in
// the store is renamed.
type proposalView struct {
	ID          string `json:"id"`
	Request     string `json:"request"`
	Title       string `json:"title"`
	Scope       string `json:"scope"`
	Risks       string `json:"risks"`
	Status      string `json:"status"`
	Occurrences int    `json:"occurrences"`
	Issue       int    `json:"issue"`
	Agent       string `json:"agent"`
	Detail      string `json:"detail"`
	Created     string `json:"created"`
	Updated     string `json:"updated"`
	// Notified is empty when the owner has never been asked. A zero time
	// rendered as "0001-01-01T00:00:00Z" reads as a date to every consumer
	// that sorts on it.
	Notified string `json:"notified"`
}

func printProposalsJSON(all []proposal.Proposal) error {
	// Non-nil so an empty result marshals as [] rather than null: the broker
	// pipes this straight into jq, and `null | length` is an error where
	// `[] | length` is nothing to do.
	out := make([]proposalView, 0, len(all))
	for _, p := range all {
		v := proposalView{
			ID: p.ID, Request: p.Request, Title: p.Title, Scope: p.Scope,
			Risks: p.Risks, Status: string(p.Status), Occurrences: p.Occurrences,
			Issue: p.Issue, Agent: p.Agent, Detail: p.Detail,
			Created: stampProposal(p.Created), Updated: stampProposal(p.Updated),
			Notified: stampProposal(p.Notified),
		}
		out = append(out, v)
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("rendering proposals: %w", err)
	}
	stdout.println(string(b))
	return nil
}

func stampProposal(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// printProposalLine is one row of `voice proposals list`. The ID comes first
// because every other command takes it as an argument.
func printProposalLine(p proposal.Proposal) {
	what := p.Title
	if what == "" {
		what = p.Request
	}
	var extra string
	if p.Issue > 0 {
		extra += fmt.Sprintf(" #%d", p.Issue)
	}
	if p.Occurrences > 1 {
		extra += fmt.Sprintf(" (asked %d times)", p.Occurrences)
	}
	stdout.printf("%-26s %-13s %s%s\n", p.ID, p.Status, what, extra)
}

func printProposal(p proposal.Proposal, events []proposal.Event) {
	stdout.printf("%-12s %s\n", "id", p.ID)
	stdout.printf("%-12s %s\n", "status", p.Status)
	// The owner's own words, before any interpretation of them: the whole
	// point of keeping them is that the reader can tell what was actually
	// said from what somebody decided it meant.
	stdout.printf("%-12s %s\n", "request", p.Request)
	for _, field := range []struct {
		label string
		value string
	}{
		{"title", p.Title},
		{"scope", p.Scope},
		{"risks", p.Risks},
		{"detail", p.Detail},
		{"agent", p.Agent},
	} {
		if field.value != "" {
			stdout.printf("%-12s %s\n", field.label, field.value)
		}
	}
	if p.Issue > 0 {
		stdout.printf("%-12s #%d\n", "issue", p.Issue)
	}
	stdout.printf("%-12s %d\n", "occurrences", p.Occurrences)
	stdout.printf("%-12s %s\n", "created", p.Created.Local().Format("2 Jan 15:04"))
	stdout.printf("%-12s %s\n", "updated", p.Updated.Local().Format("2 Jan 15:04"))
	if p.Notified.IsZero() {
		stdout.printf("%-12s %s\n", "notified", "never - the owner has not been asked yet")
	} else {
		stdout.printf("%-12s %s\n", "notified", p.Notified.Local().Format("2 Jan 15:04"))
	}
	for _, e := range events {
		stdout.printf("%-12s %-13s %s %s\n", "", e.Status, e.At.Local().Format("2 Jan 15:04"), e.Detail)
	}
}

// proposalsDoctor reports the proposal board as part of `voice doctor`.
//
// It reports even when there is nothing to report, because the failure this
// feature replaced was silence: a request refused politely, recorded nowhere,
// and found days later only because somebody thought to ask. A queue that
// cannot be read, or that nobody is configured to approve, has to be visible
// here or it is that silence again with a database attached.
func proposalsDoctor(a *session.Assistant, c config.Config, check func(label string, ok bool, detail string)) {
	if !c.Proposals.Enabled {
		return
	}
	switch counts, err := proposalCounts(a); {
	case err != nil:
		check("proposals", false, err.Error())
	case len(counts) == 0:
		// A device nobody has asked for the impossible is the healthy case,
		// and the line says so plainly. An empty queue dressed up as a
		// warning is how people learn to skim past doctor's output.
		check("proposals", true, "none recorded")
	default:
		total := 0
		for _, n := range counts {
			total += n
		}
		check("proposals", true, fmt.Sprintf("%d recorded", total))
		for _, s := range reportedStatuses(counts) {
			stdout.printf("%-12s      %-13s %d\n", "", s, counts[s])
		}
	}
	if strings.TrimSpace(c.Proposals.Approver) == "" {
		check("proposals", false, "proposals.approver is empty: no owner seat is configured to carry the approval question, so anything recorded here waits for an approval nobody will be asked for - set proposals.approver to the owner's Herdr seat")
	}
}

// proposalCounts reads the board, turning both ways it can be unavailable into
// an error an operator can act on. A nil store means the session could not open
// the database - the reason is in the log, the path is not - so the path is
// what this says.
func proposalCounts(a *session.Assistant) (map[proposal.Status]int, error) {
	if a.Proposals != nil {
		counts, err := a.Proposals.Counts()
		if err != nil {
			return nil, fmt.Errorf("enabled but unreadable: %w", err)
		}
		return counts, nil
	}
	return nil, errors.New("enabled but no store is open; nothing can be recorded until it is")
}

// reportedStatuses orders the statuses that have rows, lifecycle first and
// then anything this build does not recognise. The unknowns are printed rather
// than skipped so the per-status lines always add up to the total above them:
// a newer `voice` writing a status this one has never heard of must show as a
// line nobody expected, not as a total that does not match.
func reportedStatuses(counts map[proposal.Status]int) []proposal.Status {
	known := map[proposal.Status]bool{}
	var out []proposal.Status
	for _, s := range proposalStatuses {
		known[s] = true
		if counts[s] > 0 {
			out = append(out, s)
		}
	}
	var rest []string
	for s, n := range counts {
		if n > 0 && !known[s] {
			rest = append(rest, string(s))
		}
	}
	sort.Strings(rest)
	for _, s := range rest {
		out = append(out, proposal.Status(s))
	}
	return out
}
