package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/duanjesus/capivaradb/internal/pgerr"
	"github.com/duanjesus/capivaradb/internal/pgwire"
	"github.com/duanjesus/capivaradb/internal/storage"
)

// The crash tests run a random workload against a database on a simulated
// disk that loses power at a random moment, then recover and check two
// things:
//
//  1. Durability and atomicity: the database holds exactly the transactions
//     that were acknowledged as committed — all of each, and nothing of any
//     other. (The one transaction in flight at the moment of the crash may
//     legitimately be on either side.)
//  2. Integrity: every B+tree is valid, every index matches its table, no
//     page is leaked.
//
// The expected contents come from a shadow database: a second, in-memory
// database that is given the same statements and never crashes.

// execSQL runs one statement on a session, as the protocol layer would.
func execSQL(sess pgwire.Session, query string) (err error) {
	defer func() {
		if err != nil {
			sess.OnError()
		}
	}()
	stmts, err := sess.Parse(query)
	if err != nil {
		return err
	}
	for _, st := range stmts {
		p, err := sess.Prepare(st, nil)
		if err != nil {
			return err
		}
		rows, err := p.Execute(context.Background(), nil)
		if err != nil {
			return err
		}
		for {
			if _, err := rows.Next(context.Background()); err == io.EOF {
				break
			} else if err != nil {
				return err
			}
		}
	}
	return nil
}

// dump renders everything a database holds: its tables and indexes, and
// every row. Two databases with the same dump are, for these tests, equal.
func dump(t *testing.T, db *DB) string {
	t.Helper()
	db.mu.RLock()
	defer db.mu.RUnlock()
	var names []string
	for name := range db.tables {
		names = append(names, name)
	}
	sort.Strings(names)
	var sb strings.Builder
	for _, name := range names {
		tbl := db.tables[name]
		var indexes []string
		for _, ix := range tbl.indexes {
			indexes = append(indexes, fmt.Sprintf("%s%v unique=%v", ix.name, ix.cols, ix.unique))
		}
		sort.Strings(indexes)
		fmt.Fprintf(&sb, "table %s indexes %v\n", name, indexes)
		// Only what is committed: the view of a transaction starting now.
		rows, err := db.scan(tbl, db.newSnapshot(0))
		if err != nil {
			t.Fatalf("scanning %s: %v", name, err)
		}
		// Tables without a primary key are kept in row ID order, and row
		// IDs differ between the two databases after rollbacks; compare
		// them as sets.
		lines := make([]string, len(rows))
		for i, r := range rows {
			lines[i] = fmt.Sprint(r.vals)
		}
		if tbl.pk == nil {
			sort.Strings(lines)
		}
		for _, line := range lines {
			// Long values are shortened: equal lengths and equal ends
			// are evidence enough, and keep failures readable.
			if len(line) > 80 {
				line = fmt.Sprintf("%s...%s (%d bytes)", line[:30], line[len(line)-30:], len(line))
			}
			sb.WriteString("  " + line + "\n")
		}
	}
	return sb.String()
}

// crashRig is a database on a simulated disk, together with its shadow.
type crashRig struct {
	t      *testing.T
	disk   *storage.SimDisk
	rng    *rand.Rand
	db     *DB
	shadow *DB
	// One session pair per simulated client. Clients write to disjoint
	// key ranges, so that the order in which interleaved transactions are
	// rolled back cannot change the outcome.
	sess       []pgwire.Session
	shadowSess []pgwire.Session
	opts       Options
	// txLog holds the statements of each client's open transaction block.
	txLog       [crashClients][]string
	metaRebuilt int
	crashes     int
	// trace lists the statements run since the last crash, for the report
	// of a failure.
	trace      []string
	recoveries storage.RecoveryInfo
}

const crashClients = 3

func newCrashRig(t *testing.T, seed int64) *crashRig {
	r := &crashRig{
		t:      t,
		disk:   storage.NewSimDisk(seed),
		rng:    rand.New(rand.NewSource(seed)),
		shadow: New(),
		// A small pool forces evictions, so that pages of uncommitted
		// transactions do reach the data file; a small checkpoint
		// threshold makes checkpoints part of what can be interrupted.
		opts: Options{PoolPages: 12, CheckpointBytes: 256 << 10},
	}
	for i := 0; i < crashClients; i++ {
		s, _ := r.shadow.NewSession(map[string]string{"user": "shadow"})
		r.shadowSess = append(r.shadowSess, s)
	}
	r.open(false)
	return r
}

// open opens the database on whatever the disk holds, recovering if needed.
// With faulty set, the disk may crash again while recovery is running, as
// many times as it takes: recovery must itself be restartable.
func (r *crashRig) open(faulty bool) {
	r.t.Helper()
	for attempt := 0; ; attempt++ {
		if faulty && attempt < 3 && r.rng.Intn(3) == 0 {
			r.disk.CrashAfter(r.rng.Intn(40))
		}
		db, err := r.tryOpen()
		if err == nil {
			r.disk.CrashAfter(-1)
			r.db = db
			break
		}
		if !r.disk.Crashed() {
			r.t.Fatalf("recovery failed: %v", err)
		}
		r.disk.Crash()
		r.crashes++
	}
	info := r.db.Recovery()
	r.recoveries.PagesRedone += info.PagesRedone
	r.recoveries.TornPages += info.TornPages
	r.recoveries.RolledBack += info.RolledBack
	r.recoveries.UndoApplied += info.UndoApplied
	r.recoveries.Completed += info.Completed
	if info.MetaRebuilt {
		r.metaRebuilt++
	}
	r.sess = nil
	for i := 0; i < crashClients; i++ {
		s, _ := r.db.NewSession(map[string]string{"user": "real"})
		r.sess = append(r.sess, s)
	}
}

func (r *crashRig) tryOpen() (db *DB, err error) {
	// A crash in the middle of recovery's own undo surfaces as a panic,
	// like any storage failure during a rollback.
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return OpenFiles(r.disk.Open("data"), r.disk.Open("wal"), r.opts)
}

// run executes a statement on the real database. crashed is true if the
// disk gave out while it ran, in which case nothing is known about whether
// it took effect.
func (r *crashRig) run(client int, query string) (err error, crashed bool) {
	defer func() {
		if p := recover(); p != nil {
			if !r.disk.Crashed() {
				panic(p)
			}
			err, crashed = fmt.Errorf("panic: %v", p), true
		}
	}()
	err = execSQL(r.sess[client], query)
	short := query
	if len(short) > 90 {
		short = short[:90] + "..."
	}
	r.trace = append(r.trace, fmt.Sprintf("client %d: %s -> %v", client, short, err))
	return err, err != nil && r.disk.Crashed()
}

// both runs a statement on the real database and, if the disk held, on the
// shadow, and requires them to agree on whether and how it failed.
func (r *crashRig) both(client int, query string) (crashed bool) {
	r.t.Helper()
	err, crashed := r.run(client, query)
	if crashed {
		return true
	}
	switch query {
	case "begin", "commit", "rollback":
		r.txLog[client] = nil
	default:
		r.txLog[client] = append(r.txLog[client], query)
	}
	shadowErr := execSQL(r.shadowSess[client], query)
	if code(err) != code(shadowErr) {
		r.t.Fatalf("the two databases disagree on %q:\n real:   %v\n shadow: %v", query, err, shadowErr)
	}
	return false
}

func code(err error) string {
	if err == nil {
		return ""
	}
	var pe *pgerr.Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	return err.Error()
}

// statement returns a random data-modifying statement for a client, on the
// keys that client owns.
func (r *crashRig) statement(client int) string {
	base := client * 1000
	id := func() int { return base + r.rng.Intn(60) }
	switch r.rng.Intn(19) {
	case 0, 1, 2, 3:
		// Occasionally a value large enough to need overflow pages.
		note := fmt.Sprintf("n%d", r.rng.Intn(1000))
		if r.rng.Intn(6) == 0 {
			note = strings.Repeat(fmt.Sprintf("%04d", r.rng.Intn(10000)), 500+r.rng.Intn(4000))
		}
		return fmt.Sprintf("insert into acct values (%d, %d, '%s')", id(), r.rng.Intn(1000), note)
	case 4, 5:
		lo := id()
		return fmt.Sprintf("update acct set bal = bal + %d where id between %d and %d", r.rng.Intn(100)-50, lo, lo+r.rng.Intn(20))
	case 6:
		// Changing the primary key moves the row within the tree.
		return fmt.Sprintf("update acct set id = id + 500 where id = %d", id())
	case 7:
		return fmt.Sprintf("update acct set id = id - 500 where id >= %d and id < %d", base+500, base+1000)
	case 8:
		return fmt.Sprintf("delete from acct where id >= %d and id < %d and bal %% 5 = %d", base, base+1000, r.rng.Intn(5))
	case 9:
		return fmt.Sprintf("delete from acct where id = %d", id())
	case 10, 11:
		return fmt.Sprintf("insert into log (client, msg) values (%d, 'm%d')", client, r.rng.Intn(100))
	// Schema changes inside transactions, on a table private to the client:
	// rolling these back means freeing trees, which must happen exactly
	// once however often recovery is interrupted.
	case 14:
		return fmt.Sprintf("create table p%d (k int primary key, v text unique)", client)
	case 15, 16:
		return fmt.Sprintf("insert into p%d values (%d, 'v%d')", client, r.rng.Intn(40), r.rng.Intn(40))
	case 17:
		return fmt.Sprintf("drop table p%d", client)
	case 18:
		if r.rng.Intn(2) == 0 {
			return fmt.Sprintf("create index p%d_kv on p%d (v, k)", client, client)
		}
		return fmt.Sprintf("drop index p%d_kv", client)
	case 12:
		return fmt.Sprintf("delete from log where client = %d and msg = 'm%d'", client, r.rng.Intn(100))
	}
	return fmt.Sprintf("insert into acct select id + 100, bal, 'copy' from acct where id >= %d and id < %d and bal < 50", base, base+60)
}

// ddl returns a random schema change. Only client 0 issues them, and only
// outside other clients' transactions, because a drop is refused while
// others have uncommitted changes.
func (r *crashRig) ddl() string {
	n := r.rng.Intn(3)
	switch r.rng.Intn(8) {
	case 0:
		return "create index acct_bal on acct (bal)"
	case 1:
		return "drop index if exists acct_bal"
	case 2:
		return "create index if not exists log_msg on log (msg, client)"
	case 3:
		return "drop index if exists log_msg"
	case 4, 5:
		return fmt.Sprintf("create table if not exists side%d (k int primary key, v text unique); insert into side%d values (%d, 'v%d')", n, n, r.rng.Intn(50), r.rng.Intn(50))
	case 6:
		return fmt.Sprintf("drop table if exists side%d", n)
	}
	return fmt.Sprintf("create table tmp%d (x int); insert into tmp%d values (1), (2); drop table tmp%d", n, n, n)
}

// step runs one unit of work: a statement on its own, a transaction block,
// or two interleaved transactions. It returns true if the disk crashed, and
// then the dumps the recovered database is allowed to match.
func (r *crashRig) step() (crashed bool, allowed []string) {
	before := dump(r.t, r.shadow)
	// Having seen the crash while the work was still uncommitted, only the
	// state before it is acceptable: the work must vanish.
	lost := func() (bool, []string) {
		for _, s := range r.shadowSess {
			execSQL(s, "rollback")
		}
		return true, []string{before}
	}

	switch r.rng.Intn(10) {
	case 0: // schema change, autocommit
		query := r.ddl()
		if _, crashed := r.run(0, query); crashed {
			// A multi-statement string is one statement at a time, each
			// its own transaction: any prefix may have been committed.
			allowed = []string{before}
			for _, part := range strings.Split(query, ";") {
				execSQL(r.shadowSess[0], part)
				allowed = append(allowed, dump(r.t, r.shadow))
			}
			return true, allowed
		}
		if err := execSQL(r.shadowSess[0], query); err != nil && code(err) == err.Error() {
			r.t.Fatalf("shadow: %v", err)
		}

	case 1, 2, 3, 4: // one statement, autocommit
		client := r.rng.Intn(crashClients)
		query := r.statement(client)
		if _, crashed := r.run(client, query); crashed {
			// It was in flight: committed or not, both are right.
			execSQL(r.shadowSess[client], query)
			return true, []string{before, dump(r.t, r.shadow)}
		}
		execSQL(r.shadowSess[client], query)

	case 5, 6, 7: // a transaction block
		client := r.rng.Intn(crashClients)
		if r.both(client, "begin") {
			return lost()
		}
		for n := 1 + r.rng.Intn(6); n > 0; n-- {
			if r.both(client, r.statement(client)) {
				return lost()
			}
		}
		if r.rng.Intn(4) == 0 {
			if r.both(client, "rollback") {
				return lost()
			}
			break
		}
		if _, crashed := r.run(client, "commit"); crashed {
			execSQL(r.shadowSess[client], "commit")
			return true, []string{before, dump(r.t, r.shadow)}
		}
		execSQL(r.shadowSess[client], "commit")

	default: // two transactions, interleaved statement by statement
		a, b := 1, 2
		for _, c := range []int{a, b} {
			if r.both(c, "begin") {
				return lost()
			}
		}
		for n := 2 + r.rng.Intn(8); n > 0; n-- {
			c := []int{a, b}[r.rng.Intn(2)]
			if r.both(c, r.statement(c)) {
				return lost()
			}
		}
		// The first commits; the second commits, rolls back, or is left
		// open to be caught by a later crash or closed by the next step.
		if _, crashed := r.run(a, "commit"); crashed {
			execSQL(r.shadowSess[b], "rollback")
			execSQL(r.shadowSess[a], "commit")
			return true, []string{before, dump(r.t, r.shadow)}
		}
		execSQL(r.shadowSess[a], "commit")
		end := []string{"commit", "rollback"}[r.rng.Intn(2)]
		if _, crashed := r.run(b, end); crashed {
			if end == "rollback" {
				execSQL(r.shadowSess[b], "rollback")
				return true, []string{dump(r.t, r.shadow)}
			}
			// b's changes were made while a's were uncommitted but touch
			// other keys, so "a committed, b not" is a's commit alone.
			withoutB := r.shadowWithout(b)
			execSQL(r.shadowSess[b], "commit")
			return true, []string{withoutB, dump(r.t, r.shadow)}
		}
		execSQL(r.shadowSess[b], end)
	}
	return false, nil
}

func testCrashes(t *testing.T, seeds, rounds int) {
	var crashes, undone, redone, torn, completed, meta, commits int
	for seed := int64(1); seed <= int64(seeds); seed++ {
		r := newCrashRig(t, seed)
		setup := "create table acct (id int primary key, bal int not null, note text); create table log (client int, msg text)"
		if r.both(0, setup) {
			t.Fatal("crash during setup")
		}

		for round := 0; round < rounds; round++ {
			r.trace = nil
			r.disk.CrashAfter(r.rng.Intn(250))
			var allowed []string
			for crashed := false; !crashed; {
				crashed, allowed = r.step()
				if !crashed {
					commits++
				}
			}
			r.disk.Crash()
			r.crashes++
			r.open(true)

			if _, err := r.db.Verify(); err != nil {
				t.Fatalf("seed %d round %d: the recovered database is inconsistent: %v", seed, round, err)
			}
			got := dump(t, r.db)
			match := -1
			for i, want := range allowed {
				if got == want {
					match = i
				}
			}
			if match < 0 {
				tail := r.trace
				if len(tail) > 30 {
					tail = tail[len(tail)-30:]
				}
				t.Logf("last statements before the crash:\n%s", strings.Join(tail, "\n"))
				t.Fatalf("seed %d round %d: the recovered database matches none of the %d allowed states.\n--- recovered:\n%s--- allowed[0]:\n%s--- allowed[last]:\n%s",
					seed, round, len(allowed), got, allowed[0], allowed[len(allowed)-1])
			}
			// Bring the shadow to the state the real database is in.
			r.syncShadow(allowed, match, got)
		}
		crashes += r.crashes
		undone += r.recoveries.UndoApplied
		redone += r.recoveries.PagesRedone
		torn += r.recoveries.TornPages
		completed += r.recoveries.Completed
		meta += r.metaRebuilt
	}
	t.Logf("%d crashes survived (%d units of work committed between them): %d page changes replayed, %d pages rebuilt from log images, %d changes undone, %d interrupted commits completed, %d meta pages rebuilt",
		crashes, commits, redone, torn, undone, completed, meta)
}

// syncShadow makes the shadow hold the state the real database recovered
// to. The shadow was advanced to the last allowed state while computing
// them; if the real database is in an earlier one, the shadow is rebuilt.
func (r *crashRig) syncShadow(allowed []string, match int, got string) {
	if match == len(allowed)-1 && dump(r.t, r.shadow) == got {
		return
	}
	// Rebuild the shadow from the real database's contents.
	r.shadow = New()
	r.shadowSess = nil
	for i := 0; i < crashClients; i++ {
		s, _ := r.shadow.NewSession(map[string]string{"user": "shadow"})
		r.shadowSess = append(r.shadowSess, s)
	}
	copyDatabase(r.t, r.db, r.shadow)
	if again := dump(r.t, r.shadow); again != got {
		r.t.Fatalf("could not rebuild the shadow:\n--- real:\n%s--- shadow:\n%s", got, again)
	}
}

// copyDatabase recreates in dst the tables, indexes and rows of src.
func copyDatabase(t *testing.T, src, dst *DB) {
	t.Helper()
	sess, _ := dst.NewSession(map[string]string{"user": "copy"})
	defer sess.Close()
	run := func(q string) {
		if err := execSQL(sess, q); err != nil {
			t.Fatalf("rebuilding the shadow: %s: %v", q, err)
		}
	}
	src.mu.RLock()
	defer src.mu.RUnlock()
	for _, name := range src.tableNames() {
		tbl := src.tables[name]
		var cols []string
		for _, c := range tbl.cols {
			def := fmt.Sprintf("%s %s", c.name, c.typ)
			if c.notNull && tbl.pk == nil {
				def += " not null"
			}
			cols = append(cols, def)
		}
		if tbl.pk != nil {
			cols = append(cols, "primary key ("+tbl.colNames(tbl.pk, ", ")+")")
		}
		// Not-null columns outside the key are declared after the fact
		// only for acct, the one table that has them.
		create := fmt.Sprintf("create table %s (%s)", tbl.name, strings.Join(cols, ", "))
		if tbl.name == "acct" {
			create = "create table acct (id int primary key, bal int not null, note text)"
		}
		run(create)
		rows, err := src.scan(tbl, src.newSnapshot(0))
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			vals := make([]string, len(row.vals))
			for i, v := range row.vals {
				switch v := v.(type) {
				case nil:
					vals[i] = "null"
				case string:
					vals[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
				default:
					vals[i] = fmt.Sprint(v)
				}
			}
			run(fmt.Sprintf("insert into %s values (%s)", tbl.name, strings.Join(vals, ", ")))
		}
		for _, ix := range tbl.indexes {
			// Indexes that back UNIQUE constraints are recreated under
			// their own names, which gives the same catalog.
			unique := ""
			if ix.unique {
				unique = "unique "
			}
			run(fmt.Sprintf("create %sindex %s on %s (%s)", unique, ix.name, tbl.name, tbl.colNames(ix.cols, ", ")))
		}
	}
}

func TestCrashRecovery(t *testing.T) {
	seeds, rounds := 60, 12
	if testing.Short() {
		seeds = 10
	}
	testCrashes(t, seeds, rounds)
}

// shadowWithout returns what the shadow would hold if the client's open
// transaction did not commit, and leaves the shadow as it found it.
//
// Rows of an open transaction are invisible to the dump anyway, but schema
// changes are not: the catalog is not versioned, so a table the client
// created is already listed. The only way to see the state without the
// transaction is to roll it back; its statements are then replayed.
// Clients work on disjoint keys, so replaying them after the fact gives the
// same result as their original interleaving.
func (r *crashRig) shadowWithout(client int) string {
	replay := append([]string(nil), r.txLog[client]...)
	execSQL(r.shadowSess[client], "rollback")
	without := dump(r.t, r.shadow)
	execSQL(r.shadowSess[client], "begin")
	for _, q := range replay {
		execSQL(r.shadowSess[client], q)
	}
	return without
}
