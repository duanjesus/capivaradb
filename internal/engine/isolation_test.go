package engine

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/duanjesus/capivaradb/internal/pgerr"
	"github.com/duanjesus/capivaradb/internal/pgwire"
)

// The isolation tests script several sessions, one statement at a time, and
// check what each statement returns — including whether it has to wait.
// They are written as transcripts: a list of (session, statement, expected
// outcome), which is also how they are printed with -v. The format follows
// PostgreSQL's own isolation tester.
//
// An expected outcome is the rows ("1|a;2|b"), the command tag for
// statements without rows, "ERROR <sqlstate>", or "waits" if the statement
// must block on another transaction. A blocked statement's eventual outcome
// is checked by a later step with an empty statement.

type isoStep struct {
	sess string
	sql  string
	want string
}

type isoTest struct {
	t        *testing.T
	db       *DB
	sessions map[string]*isoSession
}

type isoSession struct {
	sess    *Session
	pending chan string // the outcome of a statement that blocked
	cancel  context.CancelFunc
}

func newIsoTest(t *testing.T, setup string) *isoTest {
	t.Helper()
	db := New()
	h := newHarness(t, db)
	h.mustRun(setup)
	return &isoTest{t: t, db: db, sessions: make(map[string]*isoSession)}
}

func (it *isoTest) session(name string) *isoSession {
	if s := it.sessions[name]; s != nil {
		return s
	}
	sess, _ := it.db.NewSession(map[string]string{"user": name})
	s := &isoSession{sess: sess.(*Session)}
	it.sessions[name] = s
	it.t.Cleanup(sess.Close)
	return s
}

// outcome runs one statement and renders its result as a transcript entry.
func outcome(ctx context.Context, sess pgwire.Session, query string) (out string) {
	fail := func(err error) string {
		sess.OnError()
		return "ERROR " + pgerr.From(err).Code
	}
	stmts, err := sess.Parse(query)
	if err != nil {
		return fail(err)
	}
	for _, st := range stmts {
		p, err := sess.Prepare(st, nil)
		if err != nil {
			return fail(err)
		}
		res, err := p.Execute(ctx, nil)
		if err != nil {
			return fail(err)
		}
		var lines []string
		for {
			row, err := res.Next(ctx)
			if err == io.EOF {
				break
			}
			if err != nil {
				return fail(err)
			}
			cells := make([]string, len(row))
			for i, v := range row {
				cells[i] = "NULL"
				if v != nil {
					cells[i] = pgwire.TextValue(v)
				}
			}
			lines = append(lines, strings.Join(cells, "|"))
		}
		out = res.Tag()
		if p.Columns() != nil {
			out = strings.Join(lines, ";")
		}
	}
	return out
}

// run plays a transcript.
func (it *isoTest) run(steps []isoStep) {
	it.t.Helper()
	for i, st := range steps {
		s := it.session(st.sess)
		var got string
		switch {
		case st.sql == "":
			// Collect the outcome of this session's blocked statement.
			if s.pending == nil {
				it.t.Fatalf("step %d: session %s has no statement in progress", i+1, st.sess)
			}
			select {
			case got = <-s.pending:
				s.pending = nil
			case <-time.After(10 * time.Second):
				it.t.Fatalf("step %d: session %s is still waiting", i+1, st.sess)
			}
		case st.sql == "cancel":
			s.cancel()
			got = "cancel"
		default:
			if s.pending != nil {
				it.t.Fatalf("step %d: session %s is blocked and cannot run %q", i+1, st.sess, st.sql)
			}
			ctx, cancel := context.WithCancel(context.Background())
			s.cancel = cancel
			done := make(chan string, 1)
			go func() { done <- outcome(ctx, s.sess, st.sql) }()
			// Either the statement finishes, or the session reports that
			// it is waiting for another transaction. No timing involved.
			for got == "" {
				select {
				case got = <-done:
					if got == "" {
						got = "(no rows)"
					}
				default:
					if s.sess.blocked.Load() {
						got, s.pending = "waits", done
					} else {
						time.Sleep(50 * time.Microsecond)
					}
				}
			}
		}
		if got == "" {
			got = "(no rows)"
		}
		it.t.Logf("%-3s %-62s %s", st.sess+":", st.sql, got)
		if got != st.want {
			it.t.Fatalf("step %d: session %s: %s\n got %s\nwant %s", i+1, st.sess, st.sql, got, st.want)
		}
	}
	for name, s := range it.sessions {
		if s.pending != nil {
			it.t.Fatalf("session %s is still blocked at the end of the transcript", name)
		}
	}
	if _, err := it.db.Verify(); err != nil {
		it.t.Fatalf("after the transcript: %v", err)
	}
}

const accounts = "create table acct (id int primary key, bal int not null); insert into acct values (1, 100), (2, 100)"

func TestNoDirtyRead(t *testing.T) {
	newIsoTest(t, accounts).run([]isoStep{
		{"a", "begin", "BEGIN"},
		{"a", "update acct set bal = 0 where id = 1", "UPDATE 1"},
		{"a", "insert into acct values (3, 999)", "INSERT 0 1"},
		// b sees none of it, and is not made to wait either.
		{"b", "select * from acct order by id", "1|100;2|100"},
		// a sees its own changes.
		{"a", "select * from acct order by id", "1|0;2|100;3|999"},
		{"a", "rollback", "ROLLBACK"},
		{"b", "select * from acct order by id", "1|100;2|100"},
		{"a", "select * from acct order by id", "1|100;2|100"},
	})
}

// Under read committed each statement sees what was committed before it
// started, so two reads in one transaction can differ.
func TestReadCommittedSeesNewCommits(t *testing.T) {
	newIsoTest(t, accounts).run([]isoStep{
		{"a", "begin", "BEGIN"},
		{"a", "show transaction isolation level", "read committed"},
		{"a", "select bal from acct where id = 1", "100"},
		{"b", "update acct set bal = 50 where id = 1", "UPDATE 1"},
		{"b", "insert into acct values (3, 1)", "INSERT 0 1"},
		{"a", "select bal from acct where id = 1", "50"}, // non-repeatable read
		{"a", "select count(*) from acct", "3"},          // phantom
		{"a", "commit", "COMMIT"},
	})
}

// Under repeatable read the transaction keeps the snapshot of its first
// statement: no non-repeatable reads, no phantoms.
func TestRepeatableReadIsStable(t *testing.T) {
	newIsoTest(t, accounts).run([]isoStep{
		{"a", "begin isolation level repeatable read", "BEGIN"},
		{"a", "show transaction isolation level", "repeatable read"},
		{"a", "select bal from acct where id = 1", "100"},
		{"b", "update acct set bal = 50 where id = 1", "UPDATE 1"},
		{"b", "insert into acct values (3, 1)", "INSERT 0 1"},
		{"b", "delete from acct where id = 2", "DELETE 1"},
		{"a", "select bal from acct where id = 1", "100"},
		{"a", "select count(*), sum(bal) from acct", "2|200"},
		{"a", "select * from acct order by id", "1|100;2|100"},
		{"a", "commit", "COMMIT"},
		// A new transaction sees the present.
		{"a", "select * from acct order by id", "1|50;3|1"},
	})
}

// The snapshot is taken by the first statement, not by BEGIN.
func TestSnapshotIsTakenByFirstStatement(t *testing.T) {
	newIsoTest(t, accounts).run([]isoStep{
		{"a", "begin isolation level repeatable read", "BEGIN"},
		{"b", "update acct set bal = 50 where id = 1", "UPDATE 1"},
		{"a", "select bal from acct where id = 1", "50"},
		{"b", "update acct set bal = 25 where id = 1", "UPDATE 1"},
		{"a", "select bal from acct where id = 1", "50"},
		{"a", "commit", "COMMIT"},
	})
}

// Two increments of the same row under read committed: the second waits for
// the first, then applies to the new value. Neither is lost.
func TestReadCommittedUpdateWaitsAndReapplies(t *testing.T) {
	newIsoTest(t, accounts).run([]isoStep{
		{"a", "begin", "BEGIN"},
		{"b", "begin", "BEGIN"},
		{"a", "update acct set bal = bal + 10 where id = 1", "UPDATE 1"},
		{"b", "update acct set bal = bal + 10 where id = 1", "waits"},
		{"a", "commit", "COMMIT"},
		{"b", "", "UPDATE 1"},
		{"b", "select bal from acct where id = 1", "120"},
		{"b", "commit", "COMMIT"},
		{"a", "select bal from acct where id = 1", "120"},
	})
}

// If the first writer rolls back, the waiter goes ahead on the original row.
func TestWaiterProceedsAfterRollback(t *testing.T) {
	newIsoTest(t, accounts).run([]isoStep{
		{"a", "begin", "BEGIN"},
		{"a", "delete from acct where id = 1", "DELETE 1"},
		{"b", "update acct set bal = bal + 1 where id = 1", "waits"},
		{"a", "rollback", "ROLLBACK"},
		{"b", "", "UPDATE 1"},
		{"b", "select bal from acct where id = 1", "101"},
	})
}

// ...and if it commits a delete, there is nothing left to update.
func TestWaiterFindsRowGone(t *testing.T) {
	newIsoTest(t, accounts).run([]isoStep{
		{"a", "begin", "BEGIN"},
		{"a", "delete from acct where id = 1", "DELETE 1"},
		{"b", "update acct set bal = bal + 1 where id = 1", "waits"},
		{"a", "commit", "COMMIT"},
		{"b", "", "UPDATE 0"},
	})
}

// Under repeatable read a transaction may not overwrite a change it cannot
// see: that would be a lost update. It fails, to be retried.
func TestRepeatableReadPreventsLostUpdate(t *testing.T) {
	newIsoTest(t, accounts).run([]isoStep{
		{"a", "begin isolation level repeatable read", "BEGIN"},
		{"b", "begin isolation level repeatable read", "BEGIN"},
		{"a", "select bal from acct where id = 1", "100"},
		{"b", "select bal from acct where id = 1", "100"},
		{"a", "update acct set bal = 100 + 10 where id = 1", "UPDATE 1"},
		{"a", "commit", "COMMIT"},
		{"b", "update acct set bal = 100 + 20 where id = 1", "ERROR 40001"},
		{"b", "select 1", "ERROR 25P02"},
		{"b", "rollback", "ROLLBACK"},
		{"b", "select bal from acct where id = 1", "110"},
	})
}

// The same when the conflicting change is still uncommitted: wait to find
// out, then fail if it committed.
func TestRepeatableReadWaitsThenFails(t *testing.T) {
	newIsoTest(t, accounts).run([]isoStep{
		{"a", "begin isolation level repeatable read", "BEGIN"},
		{"b", "begin isolation level repeatable read", "BEGIN"},
		{"b", "select count(*) from acct", "2"},
		{"a", "update acct set bal = 1 where id = 1", "UPDATE 1"},
		{"b", "delete from acct where id = 1", "waits"},
		{"a", "commit", "COMMIT"},
		{"b", "", "ERROR 40001"},
		{"b", "rollback", "ROLLBACK"},
	})
}

func TestRepeatableReadWaitsThenProceeds(t *testing.T) {
	newIsoTest(t, accounts).run([]isoStep{
		{"a", "begin isolation level repeatable read", "BEGIN"},
		{"b", "begin isolation level repeatable read", "BEGIN"},
		{"b", "select count(*) from acct", "2"},
		{"a", "update acct set bal = 1 where id = 1", "UPDATE 1"},
		{"b", "delete from acct where id = 1", "waits"},
		{"a", "rollback", "ROLLBACK"},
		{"b", "", "DELETE 1"},
		{"b", "commit", "COMMIT"},
		{"a", "select count(*) from acct", "1"},
	})
}

// Snapshot isolation is not serializability. Each transaction checks that
// someone else stays on call and then goes off; each sees the other still
// on, because it does not see the other's change; both commit. No serial
// order of the two could end with nobody on call. This is write skew, the
// anomaly that separates snapshot isolation from SERIALIZABLE — and the
// reason the server reports "repeatable read" when asked for serializable.
func TestWriteSkewIsPossible(t *testing.T) {
	setup := "create table doctors (name text primary key, on_call bool not null); insert into doctors values ('ana', true), ('bia', true)"
	newIsoTest(t, setup).run([]isoStep{
		{"a", "begin isolation level serializable", "BEGIN"},
		{"b", "begin isolation level serializable", "BEGIN"},
		{"a", "show transaction isolation level", "repeatable read"},
		{"a", "select count(*) from doctors where on_call", "2"},
		{"b", "select count(*) from doctors where on_call", "2"},
		{"a", "update doctors set on_call = false where name = 'ana'", "UPDATE 1"},
		{"b", "update doctors set on_call = false where name = 'bia'", "UPDATE 1"},
		{"a", "commit", "COMMIT"},
		{"b", "commit", "COMMIT"},
		{"a", "select count(*) from doctors where on_call", "0"},
	})
}

func TestDeadlockIsDetected(t *testing.T) {
	newIsoTest(t, accounts).run([]isoStep{
		{"a", "begin", "BEGIN"},
		{"b", "begin", "BEGIN"},
		{"a", "update acct set bal = bal - 1 where id = 1", "UPDATE 1"},
		{"b", "update acct set bal = bal - 1 where id = 2", "UPDATE 1"},
		{"a", "update acct set bal = bal + 1 where id = 2", "waits"},
		// b waiting for a would close the cycle: b is the one told.
		{"b", "update acct set bal = bal + 1 where id = 1", "ERROR 40P01"},
		{"b", "rollback", "ROLLBACK"},
		{"a", "", "UPDATE 1"},
		{"a", "commit", "COMMIT"},
		{"a", "select * from acct order by id", "1|99;2|101"},
	})
}

func TestThreeWayDeadlock(t *testing.T) {
	setup := accounts + "; insert into acct values (3, 100)"
	newIsoTest(t, setup).run([]isoStep{
		{"a", "begin", "BEGIN"},
		{"b", "begin", "BEGIN"},
		{"c", "begin", "BEGIN"},
		{"a", "update acct set bal = 0 where id = 1", "UPDATE 1"},
		{"b", "update acct set bal = 0 where id = 2", "UPDATE 1"},
		{"c", "update acct set bal = 0 where id = 3", "UPDATE 1"},
		{"a", "update acct set bal = 1 where id = 2", "waits"},
		{"b", "update acct set bal = 1 where id = 3", "waits"},
		{"c", "update acct set bal = 1 where id = 1", "ERROR 40P01"},
		{"c", "rollback", "ROLLBACK"},
		{"b", "", "UPDATE 1"},
		{"b", "commit", "COMMIT"},
		{"a", "", "UPDATE 1"},
		{"a", "commit", "COMMIT"},
		{"a", "select * from acct order by id", "1|0;2|1;3|1"},
	})
}

// Uniqueness looks at what is alive, not at what a snapshot sees. A key
// inserted by an open transaction makes the next insert wait to find out.
func TestUniqueConflictWaits(t *testing.T) {
	setup := "create table u (id int primary key, tag text unique); insert into u values (1, 'x')"
	newIsoTest(t, setup).run([]isoStep{
		{"a", "begin", "BEGIN"},
		{"a", "insert into u values (2, 'y')", "INSERT 0 1"},
		{"b", "insert into u values (2, 'z')", "waits"},
		{"a", "commit", "COMMIT"},
		{"b", "", "ERROR 23505"},

		{"a", "begin", "BEGIN"},
		{"a", "insert into u values (3, 'w')", "INSERT 0 1"},
		{"b", "insert into u values (4, 'w')", "waits"}, // the unique index, not the key
		{"a", "rollback", "ROLLBACK"},
		{"b", "", "INSERT 0 1"},

		// A key freed by an uncommitted delete is not free yet.
		{"a", "begin", "BEGIN"},
		{"a", "delete from u where id = 1", "DELETE 1"},
		{"b", "insert into u values (1, 'again')", "waits"},
		{"a", "commit", "COMMIT"},
		{"b", "", "INSERT 0 1"},
		{"b", "select * from u order by id", "1|again;2|y;4|w"},
	})
}

// A row committed after a snapshot was taken is invisible to it, and still
// a duplicate.
func TestUniqueViolationOnInvisibleRow(t *testing.T) {
	setup := "create table u (id int primary key)"
	newIsoTest(t, setup).run([]isoStep{
		{"a", "begin isolation level repeatable read", "BEGIN"},
		{"a", "select count(*) from u", "0"},
		{"b", "insert into u values (1)", "INSERT 0 1"},
		{"a", "select count(*) from u", "0"},
		{"a", "insert into u values (1)", "ERROR 23505"},
		{"a", "rollback", "ROLLBACK"},
	})
}

func TestOwnChangesWithinTransaction(t *testing.T) {
	newIsoTest(t, accounts).run([]isoStep{
		{"a", "begin isolation level repeatable read", "BEGIN"},
		{"a", "insert into acct values (3, 1)", "INSERT 0 1"},
		{"a", "update acct set bal = bal + 1 where id = 3", "UPDATE 1"},
		{"a", "update acct set bal = bal + 1 where id = 3", "UPDATE 1"},
		{"a", "select bal from acct where id = 3", "3"},
		{"a", "delete from acct where id = 3", "DELETE 1"},
		{"a", "insert into acct values (3, 7)", "INSERT 0 1"}, // the key is free again, for a
		{"a", "update acct set id = 30 where id = 3", "UPDATE 1"},
		{"a", "delete from acct where id = 1", "DELETE 1"},
		{"a", "insert into acct values (1, 5)", "INSERT 0 1"},
		{"a", "select * from acct order by id", "1|5;2|100;30|7"},
		{"b", "select * from acct order by id", "1|100;2|100"},
		{"a", "commit", "COMMIT"},
		{"b", "select * from acct order by id", "1|5;2|100;30|7"},
	})
}

// A statement that fails half-way leaves none of its versions behind, and
// the transaction's earlier work stays.
func TestStatementAtomicityUnderMVCC(t *testing.T) {
	newIsoTest(t, accounts).run([]isoStep{
		{"a", "update acct set bal = 100 / (bal - 100 + id - 1)", "ERROR 22012"},
		{"a", "select * from acct order by id", "1|100;2|100"},
		{"a", "insert into acct values (3, 1), (4, 1), (1, 1)", "ERROR 23505"},
		{"a", "select count(*) from acct", "2"},
	})
}

func TestWaitCanBeCancelled(t *testing.T) {
	newIsoTest(t, accounts).run([]isoStep{
		{"a", "begin", "BEGIN"},
		{"a", "update acct set bal = 0 where id = 1", "UPDATE 1"},
		{"b", "update acct set bal = 1 where id = 1", "waits"},
		{"b", "cancel", "cancel"},
		{"b", "", "ERROR 57014"},
		{"b", "select bal from acct where id = 1", "100"},
		{"a", "commit", "COMMIT"},
		{"b", "select bal from acct where id = 1", "0"},
	})
}

func TestIsolationLevelCommands(t *testing.T) {
	newIsoTest(t, accounts).run([]isoStep{
		{"a", "show transaction isolation level", "read committed"},
		{"a", "begin isolation level read uncommitted", "BEGIN"},
		{"a", "show transaction isolation level", "read committed"}, // there is no reading uncommitted data
		{"a", "set transaction isolation level repeatable read", "SET"},
		{"a", "show transaction isolation level", "repeatable read"},
		{"a", "select count(*) from acct", "2"},
		{"a", "set transaction isolation level read committed", "ERROR 25001"},
		{"a", "rollback", "ROLLBACK"},
		{"a", "set session characteristics as transaction isolation level repeatable read", "SET"},
		{"a", "show default_transaction_isolation", "repeatable read"},
		{"a", "begin", "BEGIN"},
		{"a", "show transaction isolation level", "repeatable read"},
		{"a", "commit", "COMMIT"},
		{"a", "begin isolation level read committed", "BEGIN"},
		{"a", "show transaction isolation level", "read committed"},
		{"a", "vacuum", "ERROR 25001"},
		{"a", "rollback", "ROLLBACK"},
	})
}

// versions returns how many row versions the database holds.
func versions(t *testing.T, db *DB) int {
	t.Helper()
	report, err := db.Verify()
	if err != nil {
		t.Fatal(err)
	}
	return report.Versions
}

// Vacuum removes versions nobody can see, and only those: an old snapshot
// keeps the versions it needs alive.
func TestVacuumRespectsSnapshots(t *testing.T) {
	it := newIsoTest(t, accounts)
	it.run([]isoStep{
		{"old", "begin isolation level repeatable read", "BEGIN"},
		{"old", "select * from acct order by id", "1|100;2|100"},
		{"w", "update acct set bal = bal + 1", "UPDATE 2"},
		{"w", "update acct set bal = bal + 1", "UPDATE 2"},
		{"w", "delete from acct where id = 2", "DELETE 1"},
	})
	if got := versions(t, it.db); got != 6 {
		t.Fatalf("expected 6 versions before vacuum, got %d", got)
	}
	it.run([]isoStep{
		{"w", "vacuum", "VACUUM"},
		// What "old" sees is kept, and so is the current version of row 1.
		// Everything in between is gone, including the last version of
		// row 2, which was created and deleted after "old" began.
		{"old", "select * from acct order by id", "1|100;2|100"},
	})
	if got := versions(t, it.db); got != 3 {
		t.Fatalf("with an old snapshot open, vacuum should leave 3 versions, got %d", got)
	}
	it.run([]isoStep{
		{"old", "commit", "COMMIT"},
		{"w", "vacuum acct", "VACUUM"},
		{"w", "select * from acct", "1|102"},
	})
	if got := versions(t, it.db); got != 1 {
		t.Fatalf("after the snapshot is gone, vacuum should leave 1 version, got %d", got)
	}
}

func TestAutoVacuum(t *testing.T) {
	db := New()
	h := newHarness(t, db)
	h.mustRun("create table counter (id int primary key, n int); insert into counter values (1, 0)")
	for i := 0; i < 3*autoVacuumThreshold; i++ {
		h.mustRun("update counter set n = n + 1")
	}
	h.expect("select n from counter", fmt.Sprint(3*autoVacuumThreshold))
	// Without vacuum there would be one version per update.
	if got := versions(t, db); got > autoVacuumThreshold+1 {
		t.Errorf("%d versions are left: autovacuum did not run", got)
	}
}

// TestConcurrentTransfers hammers the database from several goroutines.
// Money moves between accounts in transactions that are retried when they
// fail to serialize or deadlock; meanwhile readers add up the balances.
//
// Two invariants are checked. The total at the end is what it was at the
// start: no update was lost. And every reader, at every moment, sees that
// same total: a snapshot never shows half a transfer.
func TestConcurrentTransfers(t *testing.T) {
	for _, level := range []string{"read committed", "repeatable read"} {
		t.Run(level, func(t *testing.T) {
			const nAccounts, initial, workers, transfers = 10, 1000, 8, 150
			db := New()
			h := newHarness(t, db)
			h.mustRun("create table acct (id int primary key, bal int not null)")
			for i := 0; i < nAccounts; i++ {
				h.mustRun(fmt.Sprintf("insert into acct values (%d, %d)", i, initial))
			}
			total := fmt.Sprint(nAccounts * initial)

			var retries atomic.Int64
			var stop atomic.Bool
			var readers, writers sync.WaitGroup
			fail := make(chan string, 64)

			for r := 0; r < 3; r++ {
				readers.Add(1)
				go func() {
					defer readers.Done()
					sess, _ := db.NewSession(map[string]string{"user": "reader"})
					defer sess.Close()
					for !stop.Load() {
						if got := outcome(context.Background(), sess, "select sum(bal) from acct"); got != total {
							fail <- fmt.Sprintf("a reader saw a total of %s, expected %s", got, total)
							return
						}
					}
				}()
			}
			for w := 0; w < workers; w++ {
				writers.Add(1)
				go func() {
					defer writers.Done()
					rng := rand.New(rand.NewSource(int64(w)))
					sess, _ := db.NewSession(map[string]string{"user": "writer"})
					defer sess.Close()
					run := func(q string) string { return outcome(context.Background(), sess, q) }
					for i := 0; i < transfers; i++ {
						from, to := rng.Intn(nAccounts), rng.Intn(nAccounts)
						amount := 1 + rng.Intn(20)
						for {
							run("begin isolation level " + level)
							a := run(fmt.Sprintf("update acct set bal = bal - %d where id = %d", amount, from))
							b := a
							if a == "UPDATE 1" {
								b = run(fmt.Sprintf("update acct set bal = bal + %d where id = %d", amount, to))
							}
							if b == "UPDATE 1" && run("commit") == "COMMIT" {
								break
							}
							run("rollback")
							// Failing to serialize and deadlocking are part
							// of the contract; anything else is a bug.
							if b != "ERROR 40001" && b != "ERROR 40P01" {
								fail <- fmt.Sprintf("transfer failed with %s", b)
								return
							}
							retries.Add(1)
						}
					}
				}()
			}
			writers.Wait()
			stop.Store(true)
			readers.Wait()
			close(fail)
			for msg := range fail {
				t.Error(msg)
			}
			h.expect("select sum(bal), count(*) from acct", total+"|"+fmt.Sprint(nAccounts))
			t.Logf("%d transfers by %d workers, %d retried after a serialization failure or deadlock; total unchanged at %s",
				workers*transfers, workers, retries.Load(), total)
		})
	}
}

// An index entry points at a version; whether a reader may see that version
// is decided in the table, exactly as for a scan. This transcript only
// means something if the queries really go through the index, so that is
// checked first.
func TestIndexScanRespectsSnapshot(t *testing.T) {
	setup := "create table item (id int primary key, owner int not null, v int not null); create index item_owner on item (owner)"
	for i := 0; i < 300; i++ {
		setup += fmt.Sprintf("; insert into item values (%d, %d, 0)", i, i/3)
	}
	it := newIsoTest(t, setup+"; analyze")
	h := &harness{t: t, sess: it.session("check").sess}
	for _, q := range []string{"select v from item where id = 7", "select id, v from item where owner = 2 order by id"} {
		if plan := h.plan(q); !strings.Contains(plan, "Index Scan") {
			t.Fatalf("%s is not planned as an index scan:\n%s", q, plan)
		}
	}
	it.run([]isoStep{
		{"old", "begin isolation level repeatable read", "BEGIN"},
		{"old", "select id, v from item where owner = 2 order by id", "6|0;7|0;8|0"},
		{"a", "begin", "BEGIN"},
		{"a", "update item set v = 1 where id = 7", "UPDATE 1"},
		{"a", "delete from item where id = 8", "DELETE 1"},
		{"a", "insert into item values (1000, 2, 9)", "INSERT 0 1"},
		// Through the primary key and through the secondary index, b sees
		// none of a's uncommitted work, and a sees all of its own.
		{"b", "select v from item where id = 7", "0"},
		{"b", "select id, v from item where owner = 2 order by id", "6|0;7|0;8|0"},
		{"a", "select id, v from item where owner = 2 order by id", "6|0;7|1;1000|9"},
		{"a", "commit", "COMMIT"},
		{"b", "select id, v from item where owner = 2 order by id", "6|0;7|1;1000|9"},
		// The older snapshot still reads the old versions through the index.
		{"old", "select v from item where id = 7", "0"},
		{"old", "select id, v from item where owner = 2 order by id", "6|0;7|0;8|0"},
		{"old", "commit", "COMMIT"},
	})
}
