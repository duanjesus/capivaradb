package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duanjesus/capivaradb/internal/pgerr"
	"github.com/duanjesus/capivaradb/internal/storage"
)

// reopen closes the database (which checkpoints it) and opens the file again,
// as a server restart would.
func reopen(t *testing.T, db *DB, path string, pool int) *DB {
	t.Helper()
	// Everything must be consistent at the moment of shutdown.
	if _, err := db.Verify(); err != nil {
		t.Fatalf("before closing: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(path, Options{PoolPages: pool, NoSync: true})
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	return db
}

func openFile(t *testing.T, pool int) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "capi.cdb")
	db, err := Open(path, Options{PoolPages: pool, NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	return db, path
}

func TestDataSurvivesRestart(t *testing.T) {
	db, path := openFile(t, 256)
	h := newHarness(t, db)
	h.mustRun(`
		create table dept (id int primary key, name text not null unique);
		create table emp (
			id int primary key,
			name text not null,
			dept_id int,
			salary bigint default 50 * 2,
			note text default 'new' || '!',
			active bool default true,
			score float8,
			unique (name, dept_id));
		create table log (msg text, n int);
		create index emp_dept on emp (dept_id);
		insert into dept values (1, 'eng'), (2, 'ops');
		insert into emp (id, name, dept_id, score) values (1, 'ana', 1, 9.5), (2, 'bia', 1, null), (3, 'caio', 2, -0.25);
		insert into log values ('a', 1), ('b', 2), ('c', 3);
		delete from log where n = 2;
		update emp set salary = 300 where id = 3;
	`)
	// An uncommitted transaction must not survive, and a rolled back one
	// must leave no trace.
	h.mustRun("begin; insert into dept values (9, 'ghost'); drop table log; create table temp (x int); rollback")
	before := h.mustRun("select * from emp order by id")
	h.sess.Close()

	db = reopen(t, db, path, 256)
	defer db.Close()
	h = newHarness(t, db)

	h.expect("select * from emp order by id", before)
	h.expect("select e.name, d.name, e.salary, e.note, e.active from emp e join dept d on d.id = e.dept_id order by e.id",
		"ana|eng|100|new!|t;bia|eng|100|new!|t;caio|ops|300|new!|t")
	h.expect("select * from log", "a|1;c|3")
	h.expect("select count(*) from dept", "2")
	h.expectError("select * from temp", pgerr.UndefinedTable)

	// Everything declared before the restart is still enforced after it.
	h.expectError("insert into emp (id, name) values (1, 'dup')", pgerr.UniqueViolation)
	h.expectError("insert into dept values (3, 'eng')", pgerr.UniqueViolation)
	h.expectError("insert into emp (id, name, dept_id) values (4, 'ana', 1)", pgerr.UniqueViolation)
	h.expectError("insert into emp (id, name) values (5, null)", pgerr.NotNullViolation)
	h.expectError("create index emp_dept on emp (name)", pgerr.DuplicateTable)
	// Defaults were stored as SQL text and compiled again.
	h.mustRun("insert into emp (id, name) values (6, 'fabi')")
	h.expect("select salary, note, active from emp where id = 6", "100|new!|t")
	// Row IDs of the table without a primary key carry on after the last.
	h.mustRun("insert into log values ('d', 4)")
	h.expect("select msg from log", "a;c;d")

	report, err := db.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if report.Tables != 3 || report.Indexes != 3 || report.Rows != 2+4+3 {
		t.Errorf("report: %+v", report)
	}
}

// TestBufferPoolSmallerThanData loads far more than the pool can hold and
// checks that queries still see all of it, across a restart.
func TestBufferPoolSmallerThanData(t *testing.T) {
	const pool, rows = 32, 30000
	db, path := openFile(t, pool)
	h := newHarness(t, db)
	h.mustRun("create table big (id int primary key, grp int, payload text); create index big_grp on big (grp)")

	payload := strings.Repeat("x", 200)
	for start := 0; start < rows; start += 500 {
		var sb strings.Builder
		sb.WriteString("insert into big values ")
		for i := start; i < start+500; i++ {
			if i > start {
				sb.WriteByte(',')
			}
			// Keys arrive shuffled, so that inserts land all over the tree.
			id := (i * 7919) % rows
			fmt.Fprintf(&sb, "(%d, %d, '%s%d')", id, id%10, payload, id)
		}
		h.mustRun(sb.String())
	}
	stats, pages, _ := db.Stats()
	if pages < 20*pool {
		t.Fatalf("the data should dwarf the pool: %d pages for a pool of %d", pages, pool)
	}
	if stats.Evictions == 0 {
		t.Error("nothing was evicted")
	}
	t.Logf("file: %d pages (%d MB), pool: %d pages; %d evictions, %d page writes", pages, pages*storage.PageSize>>20, pool, stats.Evictions, stats.Writes)

	check := func(h *harness) {
		t.Helper()
		h.expect("select count(*), min(id), max(id), sum(id) from big", fmt.Sprintf("%d|0|%d|%d", rows, rows-1, rows*(rows-1)/2))
		h.expect("select grp, count(*) from big group by grp having count(*) <> 3000", "")
		h.expect("select length(payload) from big where id = 12345", "205")
	}
	check(h)
	h.mustRun("delete from big where grp >= 5; update big set payload = 'short' where grp = 0")
	h.expect("select count(*), count(distinct payload) from big", fmt.Sprintf("%d|%d", rows/2, rows/10*4+1))
	h.sess.Close()

	db = reopen(t, db, path, pool)
	defer db.Close()
	h = newHarness(t, db)
	h.expect("select count(*), count(distinct payload) from big", fmt.Sprintf("%d|%d", rows/2, rows/10*4+1))
	h.mustRun("drop table big")
	report, err := db.Verify()
	if err != nil {
		t.Fatal(err)
	}
	// Dropping the table returned its pages: the file is almost all free.
	if report.FreePages < report.Pages*9/10 {
		t.Errorf("after the drop only %d of %d pages are free", report.FreePages, report.Pages)
	}
}

func TestLongValuesAndKeys(t *testing.T) {
	db, path := openFile(t, 256)
	h := newHarness(t, db)
	h.mustRun("create table doc (id int primary key, body text); create table word (w text primary key, n int)")

	// A value far larger than a page goes to overflow pages.
	body := strings.Repeat("capivara ", 50000)
	h.mustRun("insert into doc values (1, $1), (2, 'short')", body)
	h.expect("select id, length(body) from doc order by id", fmt.Sprintf("1|%d;2|5", len(body)))
	h.mustRun("update doc set body = body || '!' where id = 1")
	h.expect("select length(body) from doc where id = 1", fmt.Sprint(len(body)+1))

	// Text keys keep their order, including the awkward cases: prefixes,
	// the empty string, embedded NULs and non-ASCII.
	h.mustRun("insert into word values ('b', 1), ('', 2), ('ab', 3), ('a', 4), ('é', 5), ('B', 6), ($1, 7), ('abc', 8)", "a\x00b")
	h.expect("select n from word order by w", "2;6;4;7;3;8;1;5")
	// The table is stored in key order, so even without ORDER BY the scan
	// comes back sorted.
	h.expect("select n from word", "2;6;4;7;3;8;1;5")

	// A key too long for an index entry is refused cleanly.
	pe := h.expectError("insert into word values ($1, 9)", pgerr.ProgramLimitExceeded, strings.Repeat("k", 2000))
	if !strings.Contains(pe.Message, "index row size") {
		t.Errorf("message: %s", pe.Message)
	}
	h.expect("select count(*) from word", "8")
	h.sess.Close()

	db = reopen(t, db, path, 256)
	defer db.Close()
	h = newHarness(t, db)
	h.expect("select length(body) from doc where id = 1", fmt.Sprint(len(body)+1))
	h.expect("select n from word", "2;6;4;7;3;8;1;5")
}

// Numeric keys must sort numerically, not as byte strings of their text.
func TestKeyOrdering(t *testing.T) {
	h := newHarness(t, New())
	h.mustRun(`
		create table i (k bigint primary key);
		insert into i values (10), (-1), (9223372036854775807), (2), (-9223372036854775808), (0), (-300);
		create table f (k float8 primary key);
		insert into f values (1.5), (-1.5), (0), (-1e300), (1e300), (-0.001), (0.001), (2);
		create table b (k bool primary key);
		insert into b values (true), (false);
		create table c (a int, b text, primary key (a, b));
		insert into c values (2, 'a'), (1, 'b'), (1, 'a'), (-5, 'z'), (2, '');
	`)
	h.expect("select k from i", "-9223372036854775808;-300;-1;0;2;10;9223372036854775807")
	h.expect("select k from f", "-1e+300;-1.5;-0.001;0;0.001;1.5;2;1e+300")
	h.expect("select k from b", "f;t")
	h.expect("select a, b from c", "-5|z;1|a;1|b;2|;2|a")
}

func TestOpenRejectsForeignFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("this is not a database\n", 1000)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, Options{PoolPages: 64}); !errors.Is(err, storage.ErrCorrupt) {
		t.Errorf("expected ErrCorrupt, got %v", err)
	}
}
