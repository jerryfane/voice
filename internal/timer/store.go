package timer

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so the static build survives
)

// Store persists the timer set in SQLite. The database is the owner's choice
// (#3): alarms and reminders will share this scheduler, and a transactional
// store gives them one place to keep state without every writer reimplementing
// atomic replacement.
//
// The driver is modernc.org/sqlite, which is pure Go. A cgo driver would break
// the CGO_ENABLED=0 static arm64 binary the release builds produce.
type Store struct {
	Path string

	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS timers (
	id       INTEGER PRIMARY KEY,
	label    TEXT    NOT NULL,
	duration INTEGER NOT NULL, -- nanoseconds
	deadline TEXT    NOT NULL, -- RFC 3339 in UTC
	reminder TEXT    NOT NULL DEFAULT '',
	state    TEXT    NOT NULL DEFAULT '{}'
);`

// Open prepares the database, creating the file and schema if needed. WAL is
// deliberately not enabled: this is a handful of rows written a few times an
// hour, and the default journal keeps the state in one file.
//
// The busy timeout is short on purpose. Every scheduler operation persists
// while holding the scheduler's lock, so a long wait here would stall
// announcing a timer that is already due; two seconds is long enough to ride
// out another process's write and short enough that a stuck peer surfaces as
// an error instead of silence.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("no timer database path configured")
	}
	if err := rejectLegacyState(path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(2000)")
	if err != nil {
		return nil, err
	}
	// One connection: SQLite writers serialise anyway, and the scheduler holds
	// its own lock around every save.
	db.SetMaxOpenConns(1)
	if err := prepare(db, path); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &Store{Path: path, db: db}, nil
}

// prepare creates the schema only when it is missing, and refuses a database
// that belongs to something else. Creating the table unconditionally wrote to
// the file on every start - including for `voice doctor` while the service was
// running - and would quietly add a timers table to whatever database a
// mistyped path pointed at.
func prepare(db *sql.DB, path string) error {
	var timers, others int
	row := db.QueryRow(`SELECT
		COUNT(*) FILTER (WHERE name = 'timers'),
		COUNT(*) FILTER (WHERE name <> 'timers')
		FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err := row.Scan(&timers, &others); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if timers > 0 {
		// Both upgrades share a transaction: a failed migration must leave
		// the previous schema and all timer rows untouched.
		var reminders, state int
		if err := db.QueryRow(`SELECT COUNT(*) FILTER (WHERE name = 'reminder'), COUNT(*) FILTER (WHERE name = 'state') FROM pragma_table_info('timers')`).Scan(&reminders, &state); err != nil {
			return fmt.Errorf("reading timer schema: %w", err)
		}
		if reminders != 0 && state != 0 {
			return nil
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if reminders == 0 {
			if _, err := tx.Exec(`ALTER TABLE timers ADD COLUMN reminder TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("adding reminder content to %s: %w", path, err)
			}
		}
		if state == 0 {
			if _, err := tx.Exec(`ALTER TABLE timers ADD COLUMN state TEXT NOT NULL DEFAULT '{}'`); err != nil {
				return fmt.Errorf("adding reminder state to %s: %w", path, err)
			}
		}
		return tx.Commit()
	}
	if others > 0 {
		return fmt.Errorf("%s is a database with %d other table(s) and no timers table; refusing to modify it (check timers.file)", path, others)
	}
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("preparing %s: %w", path, err)
	}
	return nil
}

// rejectLegacyState refuses to start on a state file written by the JSON store
// that preceded this one. Nothing has been released, so migrating dead data is
// not worth the code; a clear refusal beats silently discarding someone's
// timers.
func rejectLegacyState(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil // missing or unreadable: Open will create or report it
	}
	// Leading whitespace is still JSON. Sniffing byte zero alone let such a
	// file reach SQLite, which reported "file is not a database" instead of
	// saying what to do about it.
	trimmed := strings.TrimLeft(string(b), " \t\r\n")
	if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{") {
		return fmt.Errorf("%s holds JSON timer state from a pre-release build; delete it and restart (timers in it are already expired)", path)
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

type reminderState struct {
	Zone          string     `json:"zone,omitempty"`
	Repeat        Recurrence `json:"repeat,omitempty"`
	Resume        time.Time  `json:"resume,omitempty"`
	LastDelivered time.Time  `json:"last_delivered,omitempty"`
	Archived      bool       `json:"archived,omitempty"`
}

// Load reads the persisted timers.
func (s *Store) Load() (timers []Timer, err error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.Query(`SELECT id, label, duration, deadline, reminder, state FROM timers ORDER BY deadline`)
	if err != nil {
		return nil, err
	}
	// Closing a finished read rarely fails, but when it does it means the
	// rows Voice just read may be incomplete, which is worth saying.
	defer func() { err = errors.Join(err, rows.Close()) }()
	var ts []Timer
	for rows.Next() {
		var (
			t        Timer
			ns       int64
			deadline string
			state    string
		)
		if err := rows.Scan(&t.ID, &t.Label, &ns, &deadline, &t.Reminder, &state); err != nil {
			return nil, err
		}
		when, err := time.Parse(time.RFC3339Nano, deadline)
		if err != nil {
			return nil, fmt.Errorf("timer %d has an unreadable deadline %q: %w", t.ID, deadline, err)
		}
		t.Duration = time.Duration(ns)
		t.Deadline = when
		var metadata reminderState
		if err := json.Unmarshal([]byte(state), &metadata); err != nil {
			return nil, fmt.Errorf("timer %d has unreadable reminder state: %w", t.ID, err)
		}
		t.Zone, t.Repeat, t.Resume = metadata.Zone, metadata.Repeat, metadata.Resume
		t.LastDelivered, t.Archived = metadata.LastDelivered, metadata.Archived
		if t.Repeat.Kind != "" {
			if _, err := t.Repeat.next(t.Deadline, t.Deadline); err != nil {
				return nil, fmt.Errorf("timer %d has invalid recurrence: %w", t.ID, err)
			}
		}
		ts = append(ts, t)
	}
	return ts, rows.Err()
}

// Save replaces the stored set in one transaction, so a crash mid-write leaves
// the previous set rather than half of the new one.
func (s *Store) Save(ts []Timer) (err error) {
	if s == nil || s.db == nil {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		// The rollback is the abort path. After a successful Commit it reports
		// ErrTxDone, which is the expected outcome and not a failure; anything
		// else means the transaction was left behind and belongs in the error.
		if rollback := tx.Rollback(); rollback != nil && !errors.Is(rollback, sql.ErrTxDone) {
			err = errors.Join(err, rollback)
		}
	}()
	if _, err := tx.Exec(`DELETE FROM timers`); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO timers (id, label, duration, deadline, reminder, state) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, stmt.Close()) }()
	for _, t := range ts {
		state, err := json.Marshal(reminderState{Zone: t.Zone, Repeat: t.Repeat, Resume: t.Resume, LastDelivered: t.LastDelivered, Archived: t.Archived})
		if err != nil {
			return err
		}
		if _, err := stmt.Exec(t.ID, t.Label, int64(t.Duration), t.Deadline.UTC().Format(time.RFC3339Nano), t.Reminder, string(state)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
