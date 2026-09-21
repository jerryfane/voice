// Package proposal records the things the Voice agent was asked for and could
// not do. The agent runs under a restricted account with no GitHub credentials
// and no way to change its own code, so "I can't do that" used to be the end of
// the conversation and the request was lost the moment the microphone closed
// (#53). A proposal is that request written down: durable, deduplicated, and
// carrying the state machine that leads from the owner being asked for approval
// to one GitHub issue and one implementation seat.
//
// Nothing here talks to GitHub or spawns anything. The store only remembers,
// which is what lets the restricted account keep the record while the owner's
// own broker does the privileged work.
package proposal

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so the static build survives
)

// Status is where a proposal sits in its life. The zero value is not a status:
// a row always carries one of these.
type Status string

const (
	Pending      Status = "pending"
	Notified     Status = "notified"
	Approved     Status = "approved"
	Declined     Status = "declined"
	IssueCreated Status = "issue_created"
	Implementing Status = "implementing"
	Completed    Status = "completed"
	Failed       Status = "failed"
	Expired      Status = "expired"
)

// Proposal is one capability the Voice agent could not deliver.
type Proposal struct {
	ID          string // stable, opaque, safe in a shell argument
	Request     string // the owner's words, verbatim, single line
	Title       string // the agent's short interpretation
	Scope       string // proposed scope
	Risks       string // material risks
	Status      Status
	Occurrences int    // how many times this was asked for
	Issue       int    // GitHub issue number, 0 when none
	Agent       string // implementation seat name, empty when none
	Detail      string // last transition detail, e.g. a decline reason
	Created     time.Time
	Updated     time.Time
	Notified    time.Time // zero when the owner was never asked
}

// Event is one entry of a proposal's audit trail. The trail is append-only:
// an approval that was later marched to "implementing" must stay visible even
// if the row it describes has moved on.
type Event struct {
	Status Status
	Detail string
	At     time.Time
}

// Notice bounds how often the owner may be asked. Voice can fail at a request
// several times a minute when a whole capability is missing; without a ceiling
// the owner would get one approval question per attempt.
type Notice struct {
	MaxPerHour int
	RetryAfter time.Duration
}

// ErrNotFound is returned for an unknown proposal ID, so a CLI can exit
// nonzero on a typo instead of printing an empty record.
var ErrNotFound = errors.New("no such proposal")

var errClosed = errors.New("the proposal store is not open")

// terminal states end a proposal's life. Nothing moves forward out of one;
// the owner asking again reopens it through Record, which is deliberate - a
// declined proposal must never be able to walk itself back to implementing.
func terminal(s Status) bool {
	switch s {
	case Declined, Completed, Failed, Expired:
		return true
	}
	return false
}

// LegalTransition encodes the state machine. It is the authorization boundary
// of this feature: everything privileged (opening an issue, launching a seat)
// happens downstream of Approved, so a move that skips the owner's answer must
// be impossible rather than merely unusual.
//
// Notified may repeat: an unanswered question is asked again after
// Notice.RetryAfter, and that re-ask is a real event worth recording.
func LegalTransition(from, to Status) bool {
	switch from {
	case Pending:
		return to == Notified || to == Approved || to == Declined || to == Expired
	case Notified:
		return to == Notified || to == Approved || to == Declined || to == Expired
	case Approved:
		return to == IssueCreated || to == Failed
	case IssueCreated:
		return to == Implementing || to == Failed
	case Implementing:
		return to == Completed || to == Failed
	}
	return false
}

// Fingerprint is the deduplication key: the same request spoken twice, with a
// different mood and a different amount of punctuation, is one proposal.
//
// It normalises case, folds every run of whitespace and drops trailing
// punctuation, and stops there. Stemming or dropping common words was
// tempting and is wrong: "remind me at seven" and "remind me at eight" would
// collapse into one proposal and the second request would vanish.
func Fingerprint(request string) string {
	norm := strings.ToLower(strings.Join(strings.Fields(request), " "))
	norm = strings.TrimRightFunc(norm, func(r rune) bool {
		return unicode.IsPunct(r) || unicode.IsSymbol(r) || unicode.IsSpace(r)
	})
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}

// stamp is fixed width on purpose. RFC 3339 with nanoseconds trims trailing
// zeros, so "…:05Z" sorts after "…:05.5Z" as text - and the notice rate limit
// and the expiry sweep both compare timestamps in SQL.
const stamp = "2006-01-02T15:04:05.000000000Z07:00"

func format(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(stamp)
}

func parse(field, value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("proposal has an unreadable %s %q: %w", field, value, err)
	}
	return t, nil
}

// Store persists proposals in SQLite, alongside the timer store and for the
// same reason: the record has to survive a restart, and the owner's answer may
// arrive hours after the request.
type Store struct {
	Path string

	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS proposals (
	id          TEXT    PRIMARY KEY,
	fingerprint TEXT    NOT NULL UNIQUE,
	request     TEXT    NOT NULL,
	title       TEXT    NOT NULL,
	scope       TEXT    NOT NULL,
	risks       TEXT    NOT NULL,
	status      TEXT    NOT NULL,
	occurrences INTEGER NOT NULL,
	issue       INTEGER NOT NULL,
	agent       TEXT    NOT NULL,
	detail      TEXT    NOT NULL,
	created     TEXT    NOT NULL,
	updated     TEXT    NOT NULL,
	notified    TEXT    NOT NULL, -- empty when the owner was never asked
	requested   TEXT    NOT NULL  -- last time the owner asked; drives expiry
);
CREATE TABLE IF NOT EXISTS proposal_events (
	seq      INTEGER PRIMARY KEY,
	proposal TEXT NOT NULL,
	status   TEXT NOT NULL,
	detail   TEXT NOT NULL,
	at       TEXT NOT NULL
);`

const columns = `id, request, title, scope, risks, status, occurrences, issue, agent, detail, created, updated, notified`

// Open prepares the database, creating the file, its directory and the schema
// when they are missing.
//
// The busy timeout matches the timer store: two seconds is long enough to ride
// out the owner's broker writing an approval while the session records a new
// request, and short enough that a stuck peer surfaces as an error instead of
// a session that stops answering.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("no proposal database path configured (set proposals.file)")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(2000)")
	if err != nil {
		return nil, err
	}
	// One connection: the writers here are a session and a once-a-minute
	// broker, and serialising them costs nothing while removing every
	// interleaving question.
	db.SetMaxOpenConns(1)
	if err := prepare(db, path); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &Store{Path: path, db: db}, nil
}

// prepare creates the schema only when it is missing, and refuses a database
// that belongs to something else - a proposals.file pointed at the timer
// database would otherwise grow two extra tables in silence.
func prepare(db *sql.DB, path string) error {
	var mine, others int
	row := db.QueryRow(`SELECT
		COUNT(*) FILTER (WHERE name IN ('proposals', 'proposal_events')),
		COUNT(*) FILTER (WHERE name NOT IN ('proposals', 'proposal_events'))
		FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err := row.Scan(&mine, &others); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if others > 0 && mine == 0 {
		return fmt.Errorf("%s is a database with %d other table(s) and no proposals table; refusing to modify it (check proposals.file)", path, others)
	}
	if mine == 2 {
		return nil
	}
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("preparing %s: %w", path, err)
	}
	return nil
}

// Close releases the database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// newID mints an identifier that sorts by creation, carries no meaning worth
// guessing, and can be pasted into a sudo command line unquoted: the owner's
// broker passes these IDs straight to `voice proposals approve ID`, so a space
// or a quote in one would be a command injection waiting to happen.
func newID(now time.Time) (string, error) {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating a proposal id: %w", err)
	}
	return now.UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:]), nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanProposal(sc scanner) (Proposal, error) {
	var (
		p                            Proposal
		status                       string
		created, updated, notifiedAt string
		err                          error
	)
	if err := sc.Scan(&p.ID, &p.Request, &p.Title, &p.Scope, &p.Risks, &status,
		&p.Occurrences, &p.Issue, &p.Agent, &p.Detail, &created, &updated, &notifiedAt); err != nil {
		return Proposal{}, err
	}
	p.Status = Status(status)
	if p.Created, err = parse("created time", created); err != nil {
		return Proposal{}, err
	}
	if p.Updated, err = parse("updated time", updated); err != nil {
		return Proposal{}, err
	}
	if p.Notified, err = parse("notified time", notifiedAt); err != nil {
		return Proposal{}, err
	}
	return p, nil
}

func loadTx(tx *sql.Tx, id string) (Proposal, error) {
	p, err := scanProposal(tx.QueryRow(`SELECT `+columns+` FROM proposals WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return p, err
}

func saveTx(tx *sql.Tx, p Proposal) error {
	_, err := tx.Exec(`UPDATE proposals SET
		title = ?, scope = ?, risks = ?, status = ?, occurrences = ?,
		issue = ?, agent = ?, detail = ?, updated = ?, notified = ?
		WHERE id = ?`,
		p.Title, p.Scope, p.Risks, string(p.Status), p.Occurrences,
		p.Issue, p.Agent, p.Detail, format(p.Updated), format(p.Notified), p.ID)
	return err
}

func eventTx(tx *sql.Tx, id string, status Status, detail string, at time.Time) error {
	_, err := tx.Exec(`INSERT INTO proposal_events (proposal, status, detail, at) VALUES (?, ?, ?, ?)`,
		id, string(status), detail, format(at))
	return err
}

// tx runs fn in one transaction. Every mutation here writes a row and an audit
// event, and a crash between the two would leave a history that cannot explain
// the state it describes.
func (s *Store) tx(fn func(*sql.Tx) error) (err error) {
	if s == nil || s.db == nil {
		return errClosed
	}
	t, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		// After a successful Commit the rollback reports ErrTxDone, which is
		// the expected outcome; anything else means the transaction was left
		// behind and belongs in the error.
		if rollback := t.Rollback(); rollback != nil && !errors.Is(rollback, sql.ErrTxDone) {
			err = errors.Join(err, rollback)
		}
	}()
	if err := fn(t); err != nil {
		return err
	}
	return t.Commit()
}

// Record stores a request the agent could not satisfy, or folds it into the
// proposal that already covers it. It returns the stored proposal and whether
// this call created it, so a caller can tell the owner "noted, that's the
// third time" instead of opening a second identical thread.
//
// A repeat of a proposal that already ended - declined, completed, failed or
// expired - reopens it as Pending with its occurrence count intact. The owner
// asking again after a no is a new request, not a duplicate of the old one,
// and the count is exactly the evidence that makes the second ask persuasive.
func (s *Store) Record(p Proposal, now time.Time) (stored Proposal, created bool, err error) {
	// The request is stored on one line: it is read back in a JSON array, a
	// terminal table and a chat message, none of which survive a transcript
	// that wrapped mid-sentence.
	request := strings.Join(strings.Fields(p.Request), " ")
	if request == "" {
		return Proposal{}, false, errors.New("a proposal needs the request it came from; pass the owner's words")
	}
	fp := Fingerprint(request)
	err = s.tx(func(tx *sql.Tx) error {
		existing, err := scanProposal(tx.QueryRow(`SELECT `+columns+` FROM proposals WHERE fingerprint = ?`, fp))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			id, err := newID(now)
			if err != nil {
				return err
			}
			stored = Proposal{
				ID: id, Request: request, Title: strings.TrimSpace(p.Title),
				Scope: strings.TrimSpace(p.Scope), Risks: strings.TrimSpace(p.Risks),
				Status: Pending, Occurrences: 1, Detail: "recorded",
				Created: now.UTC(), Updated: now.UTC(),
			}
			if _, err := tx.Exec(`INSERT INTO proposals
				(id, fingerprint, request, title, scope, risks, status, occurrences, issue, agent, detail, created, updated, notified, requested)
				VALUES (?, ?, ?, ?, ?, ?, ?, 1, 0, '', ?, ?, ?, '', ?)`,
				stored.ID, fp, stored.Request, stored.Title, stored.Scope, stored.Risks,
				string(Pending), stored.Detail, format(now), format(now), format(now)); err != nil {
				return err
			}
			created = true
			return eventTx(tx, stored.ID, Pending, stored.Detail, now)
		case err != nil:
			return err
		}

		stored = existing
		stored.Occurrences++
		stored.Updated = now.UTC()
		// The first wording is kept: it is what the owner actually said, and
		// the approval question quotes it. Only the agent's own interpretation
		// is refreshed, and only when this call offers one.
		if t := strings.TrimSpace(p.Title); t != "" {
			stored.Title = t
		}
		if sc := strings.TrimSpace(p.Scope); sc != "" {
			stored.Scope = sc
		}
		if r := strings.TrimSpace(p.Risks); r != "" {
			stored.Risks = r
		}
		if terminal(existing.Status) {
			stored.Detail = fmt.Sprintf("reopened after %s (%d requests)", existing.Status, stored.Occurrences)
			stored.Status = Pending
			stored.Notified = time.Time{}
			// The closed cycle's issue and seat are dropped: pointing a fresh
			// approval at a finished ticket would attach the work to something
			// nobody is watching.
			stored.Issue, stored.Agent = 0, ""
		} else {
			stored.Detail = fmt.Sprintf("requested again (%d times)", stored.Occurrences)
		}
		if err := saveTx(tx, stored); err != nil {
			return err
		}
		// The expiry clock follows the owner, not us: re-asking keeps a
		// proposal alive, while merely re-notifying does not.
		if _, err := tx.Exec(`UPDATE proposals SET requested = ? WHERE id = ?`, format(now), stored.ID); err != nil {
			return err
		}
		return eventTx(tx, stored.ID, stored.Status, stored.Detail, now)
	})
	if err != nil {
		return Proposal{}, false, err
	}
	return stored, created, nil
}

// Get returns one proposal, or ErrNotFound.
func (s *Store) Get(id string) (Proposal, error) {
	if s == nil || s.db == nil {
		return Proposal{}, errClosed
	}
	p, err := scanProposal(s.db.QueryRow(`SELECT `+columns+` FROM proposals WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Proposal{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return p, err
}

// List returns every proposal, oldest first.
func (s *Store) List() ([]Proposal, error) {
	if s == nil || s.db == nil {
		return nil, errClosed
	}
	return s.query(`SELECT ` + columns + ` FROM proposals ORDER BY created, id`)
}

func (s *Store) query(q string, args ...any) (ps []Proposal, err error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	// A failing close on a finished read means the rows just read may be
	// incomplete, which is worth saying rather than swallowing.
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		ps = append(ps, p)
	}
	return ps, rows.Err()
}

// Counts summarises the board by status. Statuses with no proposals are
// absent, which reads correctly from a nil-safe map lookup.
func (s *Store) Counts() (counts map[Status]int, err error) {
	if s == nil || s.db == nil {
		return nil, errClosed
	}
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM proposals GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	counts = make(map[Status]int)
	for rows.Next() {
		var (
			status string
			n      int
		)
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		counts[Status(status)] = n
	}
	return counts, rows.Err()
}

// Events returns the audit trail of one proposal, oldest first.
func (s *Store) Events(id string) (events []Event, err error) {
	if _, err := s.Get(id); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT status, detail, at FROM proposal_events WHERE proposal = ? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var (
			e      Event
			status string
			at     string
		)
		if err := rows.Scan(&status, &e.Detail, &at); err != nil {
			return nil, err
		}
		e.Status = Status(status)
		if e.At, err = parse("event time", at); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// Transition moves a proposal and appends the move to its audit trail. An
// illegal move is refused by name rather than ignored: a declined proposal
// that could be marched to implementing would be an authorization bypass, and
// a silent no-op would hide it.
func (s *Store) Transition(id string, to Status, detail string, now time.Time) (Proposal, error) {
	var out Proposal
	err := s.tx(func(tx *sql.Tx) error {
		p, err := loadTx(tx, id)
		if err != nil {
			return err
		}
		if !LegalTransition(p.Status, to) {
			return fmt.Errorf("proposal %s is %s and cannot move to %s", id, p.Status, to)
		}
		p.Status = to
		p.Detail = detail
		p.Updated = now.UTC()
		if to == Notified {
			p.Notified = now.UTC()
		}
		if err := saveTx(tx, p); err != nil {
			return err
		}
		out = p
		return eventTx(tx, id, to, detail, now)
	})
	if err != nil {
		return Proposal{}, err
	}
	return out, nil
}

// SetIssue attaches the GitHub issue the owner's approval produced and moves
// the proposal to IssueCreated.
//
// It refuses a second, different number. The whole point of the feature is one
// issue per proposal; a broker that retried after a timeout must not be able to
// leave two tickets behind, so the conflicting write fails loudly and the same
// number is accepted as the no-op it is.
func (s *Store) SetIssue(id string, number int, now time.Time) (Proposal, error) {
	if number <= 0 {
		return Proposal{}, fmt.Errorf("proposal %s needs a real issue number, got %d", id, number)
	}
	var out Proposal
	err := s.tx(func(tx *sql.Tx) error {
		p, err := loadTx(tx, id)
		if err != nil {
			return err
		}
		if p.Issue != 0 && p.Issue != number {
			return fmt.Errorf("proposal %s already tracks issue #%d; refusing to repoint it at #%d (one proposal, one issue)", id, p.Issue, number)
		}
		if p.Issue == number && p.Status == IssueCreated {
			out = p
			return nil
		}
		if !LegalTransition(p.Status, IssueCreated) {
			return fmt.Errorf("proposal %s is %s and cannot move to %s", id, p.Status, IssueCreated)
		}
		p.Issue = number
		p.Status = IssueCreated
		p.Detail = fmt.Sprintf("issue #%d", number)
		p.Updated = now.UTC()
		if err := saveTx(tx, p); err != nil {
			return err
		}
		out = p
		return eventTx(tx, id, IssueCreated, p.Detail, now)
	})
	if err != nil {
		return Proposal{}, err
	}
	return out, nil
}

// SetAgent names the implementation seat and moves the proposal to
// Implementing. Renaming while already implementing is allowed - a relaunched
// seat gets a new name - and each name lands in the audit trail.
func (s *Store) SetAgent(id string, name string, now time.Time) (Proposal, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Proposal{}, fmt.Errorf("proposal %s needs the name of the seat implementing it", id)
	}
	var out Proposal
	err := s.tx(func(tx *sql.Tx) error {
		p, err := loadTx(tx, id)
		if err != nil {
			return err
		}
		if p.Agent == name && p.Status == Implementing {
			out = p
			return nil
		}
		if p.Status != Implementing && !LegalTransition(p.Status, Implementing) {
			return fmt.Errorf("proposal %s is %s and cannot move to %s", id, p.Status, Implementing)
		}
		p.Agent = name
		p.Status = Implementing
		p.Detail = "seat " + name
		p.Updated = now.UTC()
		if err := saveTx(tx, p); err != nil {
			return err
		}
		out = p
		return eventTx(tx, id, Implementing, p.Detail, now)
	})
	if err != nil {
		return Proposal{}, err
	}
	return out, nil
}

// Retry reopens a proposal whose post-approval step failed, putting it back at
// the stage it fell over on so the broker's next pass tries again.
//
// It exists because the first real approval did exactly this: the issue was
// created, the implementation seat could not be renamed - the name was longer
// than Herdr allows - and the proposal landed in Failed with an issue already
// filed and nobody working on it. Burying work the owner approved because of a
// transient failure in the step AFTER the approval is the wrong answer.
//
// It is not a way around the owner. Retry requires an approval already in the
// audit trail, so a proposal that failed before the owner answered - or was
// declined - cannot be walked forward by calling this.
func (s *Store) Retry(id string, now time.Time) (Proposal, error) {
	var out Proposal
	err := s.tx(func(tx *sql.Tx) error {
		p, err := loadTx(tx, id)
		if err != nil {
			return err
		}
		if p.Status != Failed {
			return fmt.Errorf("proposal %s is %s, and only a failed one can be retried", id, p.Status)
		}
		approved, err := approvedTx(tx, id)
		if err != nil {
			return err
		}
		if !approved {
			return fmt.Errorf("proposal %s failed before the owner approved it; ask again rather than retrying", id)
		}
		// Back to the last stage that succeeded: with an issue already filed
		// the work resumes at the seat launch, without one it resumes at
		// issue creation. Retrying from the wrong stage is how a proposal
		// ends up with two issues.
		resume := Approved
		detail := "retrying issue creation after: " + p.Detail
		if p.Issue != 0 {
			resume = IssueCreated
			detail = "retrying the implementation seat after: " + p.Detail
		}
		p.Status = resume
		p.Detail = detail
		p.Updated = now.UTC()
		if err := saveTx(tx, p); err != nil {
			return err
		}
		out = p
		return eventTx(tx, id, resume, detail, now)
	})
	if err != nil {
		return Proposal{}, err
	}
	return out, nil
}

// approvedTx reports whether the owner's approval is in the audit trail. The
// trail is the evidence, not the current status: a failed proposal has lost
// its Approved status but must not lose the fact that it was approved.
func approvedTx(tx *sql.Tx, id string) (bool, error) {
	var n int
	err := tx.QueryRow(`SELECT COUNT(*) FROM proposal_events
		WHERE proposal = ? AND status = ?`, id, string(Approved)).Scan(&n)
	return n > 0, err
}

// DueForNotice returns the proposals the owner should be asked about now,
// oldest first: everything Pending, plus anything Notified that has gone
// unanswered for longer than n.RetryAfter.
//
// The result is capped by what n.MaxPerHour leaves after the notices already
// sent in the trailing hour, counted from the audit trail. A missing capability
// produces a failed request every time it is tried, and without that cap one
// bad afternoon would put a dozen approval questions in the owner's seat.
// MaxPerHour of zero means do not ask at all.
func (s *Store) DueForNotice(now time.Time, n Notice) ([]Proposal, error) {
	if s == nil || s.db == nil {
		return nil, errClosed
	}
	if n.MaxPerHour <= 0 {
		return nil, nil
	}
	var recent int
	row := s.db.QueryRow(`SELECT COUNT(*) FROM proposal_events WHERE status = ? AND at > ?`,
		string(Notified), format(now.Add(-time.Hour)))
	if err := row.Scan(&recent); err != nil {
		return nil, err
	}
	budget := n.MaxPerHour - recent
	if budget <= 0 {
		return nil, nil
	}
	where := `status = ?`
	args := []any{string(Pending)}
	if n.RetryAfter > 0 {
		where += ` OR (status = ? AND notified <> '' AND notified <= ?)`
		args = append(args, string(Notified), format(now.Add(-n.RetryAfter)))
	}
	args = append(args, budget)
	return s.query(`SELECT `+columns+` FROM proposals WHERE `+where+` ORDER BY created, id LIMIT ?`, args...)
}

// Expire retires proposals the owner stopped caring about: Pending or Notified
// and not asked for again within after. The clock runs from the last time the
// owner made the request, not from the last notice, or a proposal we keep
// re-asking about would refresh itself forever and never age out.
func (s *Store) Expire(now time.Time, after time.Duration) ([]Proposal, error) {
	// A non-positive window is a programming mistake, not a configuration
	// choice: config.Validate already requires a positive proposals.expire,
	// so reaching here with zero would silently do nothing while the caller
	// believed it had aged out stale proposals. Say so instead.
	if after <= 0 {
		return nil, fmt.Errorf("an expiry window must be positive, got %s", after)
	}
	var expired []Proposal
	err := s.tx(func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT `+columns+` FROM proposals
			WHERE status IN (?, ?) AND requested <= ? ORDER BY created, id`,
			string(Pending), string(Notified), format(now.Add(-after)))
		if err != nil {
			return err
		}
		var stale []Proposal
		err = func() (err error) {
			defer func() { err = errors.Join(err, rows.Close()) }()
			for rows.Next() {
				p, err := scanProposal(rows)
				if err != nil {
					return err
				}
				stale = append(stale, p)
			}
			return rows.Err()
		}()
		if err != nil {
			return err
		}
		detail := fmt.Sprintf("no answer within %s", after)
		for _, p := range stale {
			p.Status = Expired
			p.Detail = detail
			p.Updated = now.UTC()
			if err := saveTx(tx, p); err != nil {
				return err
			}
			if err := eventTx(tx, p.ID, Expired, detail, now); err != nil {
				return err
			}
			expired = append(expired, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return expired, nil
}
