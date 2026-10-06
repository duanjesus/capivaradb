package engine

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/duanjesus/capivaradb/internal/pgerr"
	"github.com/duanjesus/capivaradb/internal/pgwire"
)

// ---- set operations, with known answers ----

func TestSetOperations(t *testing.T) {
	h := newHarness(t, New())
	h.mustRun(`
		create table a (x int, s text);
		create table b (x int, s text);
		insert into a values (1, 'one'), (2, 'two'), (2, 'two'), (3, 'three'), (null, null), (null, null);
		insert into b values (2, 'two'), (3, 'three'), (3, 'three'), (4, 'four'), (null, null)`)
	cases := []struct{ query, want string }{
		// UNION removes duplicates, including between NULLs; ALL keeps
		// everything, left side first.
		{"select x from a union select x from b order by 1", "1;2;3;4;NULL"},
		{"select x from a union all select x from b", "1;2;2;3;NULL;NULL;2;3;3;4;NULL"},
		{"select x from a intersect select x from b order by 1", "2;3;NULL"},
		// ALL counts: a row comes out as often as both sides have it.
		{"select x from a intersect all select x from b order by 1", "2;3;NULL"},
		{"select x from b intersect all select x from a order by 1", "2;3;NULL"},
		{"select x from a except select x from b order by 1", "1"},
		{"select x from a except all select x from b order by 1", "1;2;NULL"},
		{"select x from b except all select x from a order by 1", "3;4"},
		// Whole rows are compared, not single columns.
		{"select x, s from a intersect select x, s from b order by 1", "2|two;3|three;NULL|NULL"},
		{"select x, 'k' from a except select x, s from b order by 1", "1|k;2|k;3|k;NULL|k"},
		// INTERSECT binds tighter than UNION; parentheses override.
		{"select 1 union select 2 intersect select 3 order by 1", "1"},
		{"(select 1 union select 2) intersect select 2", "2"},
		{"select 1 except select 1 union select 5", "5"},
		{"select 1 except (select 1 union select 5)", ""},
		// ORDER BY, LIMIT and OFFSET apply to the whole.
		{"select x from a union select x from b order by x desc limit 2", "NULL;4"},
		{"select x from a union select x from b order by x nulls first limit 2 offset 1", "1;2"},
		{"(select x from a order by x limit 1) union all (select x from b order by x desc limit 1)", "1;NULL"},
		// The result takes its names from the left side and a type wide
		// enough for both.
		{"select x as n from a where x = 1 union select 2.5 order by n", "1;2.5"},
		{"select null union select x from b where x = 4", "NULL;4"},
		{"select s from a where x = 1 union select 'zzz' order by 1", "one;zzz"},
		// In every place a query can appear.
		{"select count(*) from (select x from a union select x from b) u", "5"},
		{"select x from a where x in (select x from b except select 3) order by 1", "2;2"},
		{"select exists (select 1 intersect select 2)", "f"},
		{"select (select max(x) from (select x from a union all select x from b) u)", "4"},
		// Aggregates and grouping inside the branches.
		{"select count(*) from a union all select count(*) from b", "6;5"},
		{"select x, count(*) from a group by x intersect select x, count(*) + 1 from b group by x order by 1", "2|2;NULL|2"},
	}
	for _, tc := range cases {
		h.expect(tc.query, tc.want)
	}
	h.mustRun("insert into a select x + 10, s from a union select 99, 'new'")
	h.expect("select count(*) from a", "11")

	h.expectError("select 1, 2 union select 3", pgerr.SyntaxError)
	h.expectError("select 1 union select 'a'", pgerr.DatatypeMismatch)
	h.expectError("select x from a union select s from b", pgerr.DatatypeMismatch)
	h.expectError("select x from a union select x from b order by x + 1", pgerr.FeatureNotSupported)
	h.expectError("select x from a union select x from b order by 2", "42P10")
}

// ---- the outer joins and USING, with known answers ----

func TestOuterJoins(t *testing.T) {
	h := newHarness(t, New())
	h.mustRun(`
		create table l (k int, a text);
		create table r (k int, b text);
		insert into l values (1, 'l1'), (2, 'l2'), (2, 'l2b'), (null, 'ln');
		insert into r values (2, 'r2'), (3, 'r3'), (null, 'rn')`)
	// Every method of joining must give these answers: nested loops (the
	// tables are tiny, so that is what is chosen), hash joins in memory,
	// and whatever is left when hashing is not allowed either.
	h.mustRun("set enable_nestloop = off")
	if plan := h.plan("select * from l full join r on l.k = r.k"); !strings.Contains(plan, "Hash Full Join") {
		t.Fatalf("expected a hash join with nested loops off:\n%s", plan)
	}
	for _, setting := range []string{
		"set enable_nestloop = on",
		"set enable_nestloop = off",
		"set enable_nestloop = off; set enable_hashjoin = off",
	} {
		h.mustRun("set enable_hashjoin = on; " + setting)
		h.expect("select l.k, a, r.k, b from l left join r on l.k = r.k order by a",
			"1|l1|NULL|NULL;2|l2|2|r2;2|l2b|2|r2;NULL|ln|NULL|NULL")
		h.expect("select l.k, a, r.k, b from l right join r on l.k = r.k order by b",
			"2|l2|2|r2;2|l2b|2|r2;NULL|NULL|3|r3;NULL|NULL|NULL|rn")
		h.expect("select l.k, a, r.k, b from l full join r on l.k = r.k order by a, b",
			"1|l1|NULL|NULL;2|l2|2|r2;2|l2b|2|r2;NULL|ln|NULL|NULL;NULL|NULL|3|r3;NULL|NULL|NULL|rn")
		// A condition in ON only stops rows matching; in WHERE it removes
		// them, after the join has put its NULLs in.
		h.expect("select a, b from l full join r on l.k = r.k and a <> 'l2' order by a, b",
			"l1|NULL;l2|NULL;l2b|r2;ln|NULL;NULL|r3;NULL|rn")
		h.expect("select a, b from l full join r on l.k = r.k where a is null or b is null order by a, b",
			"l1|NULL;ln|NULL;NULL|r3;NULL|rn")
		h.expect("select a, b from l right join r on l.k = r.k where a is null order by b", "NULL|r3;NULL|rn")
		// A full join with no equality in it can only be a nested loop.
		h.expect("select a, b from l full join r on l.k < r.k order by a, b",
			"l1|r2;l1|r3;l2|r3;l2b|r3;ln|NULL;NULL|rn")
		h.expect("select count(*) from l full join r on false", "7")
		h.expect("select count(*) from l full join r on true", "12")
		// An empty side.
		h.expect("select a, b from l full join (select * from r where false) r on l.k = r.k order by a",
			"l1|NULL;l2|NULL;l2b|NULL;ln|NULL")
		h.expect("select a, b from (select * from l where false) l full join r on l.k = r.k order by b",
			"NULL|r2;NULL|r3;NULL|rn")
	}

	h.mustRun("set enable_nestloop = on; set enable_hashjoin = on")

	// USING: one condition per column, and the column appears once, first.
	h.expect("select * from l join r using (k) order by a", "2|l2|r2;2|l2b|r2")
	h.expect("select * from l left join r using (k) order by a", "1|l1|NULL;2|l2|r2;2|l2b|r2;NULL|ln|NULL")
	// In a right join the column shown is the right side's.
	h.expect("select * from l right join r using (k) order by b", "2|l2|r2;2|l2b|r2;3|NULL|r3;NULL|NULL|rn")
	h.expect("select k, l.k, r.k from l right join r using (k) order by b", "2|2|2;2|2|2;3|NULL|3;NULL|NULL|NULL")
	h.expect("select r.* from l join r using (k) order by 2", "2|r2;2|r2")
	h.mustRun("create table m (k int, a text, c text); insert into m values (2, 'l2', 'm')")
	h.expect("select * from l join m using (k, a)", "2|l2|m")
	h.expect("select * from l join r using (k) join m using (k) order by 2", "2|l2|r2|l2|m;2|l2b|r2|l2|m")
	h.expectError("select * from l join r using (nope)", pgerr.UndefinedColumn)
	h.expectError("select * from l join r using (a)", pgerr.UndefinedColumn)
	h.expectError("select * from l join m using (k) join r using (a)", pgerr.AmbiguousColumn)
	h.expectError("select * from l full join r using (k)", pgerr.FeatureNotSupported)
	// Unqualified, "a" is ambiguous between l and m; "k" is not.
	h.expectError("select a from l join m using (k)", pgerr.AmbiguousColumn)
	h.expect("select k from l join m using (k)", "2;2")
}

// ---- what streaming is for ----

// big creates a table of n rows with a payload wide enough that a few
// thousand rows exceed the smallest work_mem.
func big(t *testing.T, h *harness, name string, n int) {
	t.Helper()
	h.mustRun(fmt.Sprintf("create table %s (id int primary key, grp int not null, k int not null, pad text not null)", name))
	for start := 0; start < n; start += 500 {
		var sb strings.Builder
		fmt.Fprintf(&sb, "insert into %s values ", name)
		for i := start; i < start+500 && i < n; i++ {
			if i > start {
				sb.WriteByte(',')
			}
			// k is a permutation of 0..n-1 in scrambled order.
			fmt.Fprintf(&sb, "(%d, %d, %d, '%s')", i, i%97, (i*7919)%n, strings.Repeat("x", 40+i%20))
		}
		h.mustRun(sb.String())
	}
	h.mustRun("analyze " + name)
}

func explainAnalyze(h *harness, query string) string {
	return strings.ReplaceAll(h.mustRun("explain (analyze, costs off, timing off) "+query), ";", "\n")
}

// A LIMIT stops the scan below it; EXISTS stops at the first row. Neither
// reads the table.
func TestLimitStopsTheScan(t *testing.T) {
	h := newHarness(t, New())
	big(t, h, "big", 5000)
	plan := explainAnalyze(h, "select id from big limit 3")
	if !strings.Contains(plan, "Seq Scan on big (actual rows=3 loops=1)") {
		t.Errorf("the scan under LIMIT 3 did not stop after 3 rows:\n%s", plan)
	}
	// Through a filter and a join as well: rows are pulled one at a time
	// all the way down.
	h.mustRun("set enable_hashjoin = off; set enable_mergejoin = off")
	plan = explainAnalyze(h, "select a.id from big a join big b on b.id = a.id where a.grp = 5 limit 2")
	if !strings.Contains(plan, "Index Scan using big_pkey on big b (actual rows=2 loops=2)") {
		t.Errorf("the join under LIMIT 2 did more than two lookups:\n%s", plan)
	}
	plan = explainAnalyze(h, "select exists (select 1 from big where grp = 3)")
	if !strings.Contains(plan, "Result (actual rows=1 loops=1)") {
		t.Errorf("unexpected plan:\n%s", plan)
	}
	h.expect("select exists (select 1 from big where grp = 3), exists (select 1 from big where grp = 300)", "t|f")
	// LIMIT 0 reads nothing at all.
	plan = explainAnalyze(h, "select id from big limit 0")
	if !strings.Contains(plan, "Seq Scan on big (never executed)") {
		t.Errorf("LIMIT 0 ran its input:\n%s", plan)
	}
	// The second branch of a UNION ALL is not opened if the first is enough.
	plan = explainAnalyze(h, "select id from big union all select id from big limit 4")
	if strings.Count(plan, "never executed") != 1 {
		t.Errorf("the second branch of UNION ALL ran although the first sufficed:\n%s", plan)
	}
}

// cursor opens a query without reading it.
func cursor(t *testing.T, h *harness, query string) pgwire.Rows {
	t.Helper()
	stmts, err := h.sess.Parse(query)
	if err != nil {
		t.Fatal(err)
	}
	p, err := h.sess.Prepare(stmts[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := p.Execute(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func fetch(t *testing.T, rows pgwire.Rows, n int) []int64 {
	t.Helper()
	var ids []int64
	for n < 0 || len(ids) < n {
		// A context of its own each time, cancelled afterwards, as the
		// protocol layer does between two fetches from a portal.
		ctx, cancel := context.WithCancel(context.Background())
		row, err := rows.Next(ctx)
		cancel()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, row[0].(int64))
	}
	return ids
}

// A cursor reads the state of the moment it was opened, however long it is
// left open and whatever happens meanwhile: rows deleted, updated, inserted
// and vacuumed under it. Its scan finds its place again between batches by
// key, so this is the test of that.
func TestCursorReadsItsSnapshot(t *testing.T) {
	db := New()
	h, other := newHarness(t, db), newHarness(t, db)
	big(t, h, "big", 2000)

	rows := cursor(t, h, "select id, grp from big")
	defer rows.Close()
	got := fetch(t, rows, 10)

	// More than a batch has not been read yet. Change all of it.
	other.mustRun("delete from big where id % 3 = 0")
	other.mustRun("update big set grp = -1 where id % 3 = 1")
	other.mustRun("insert into big select id + 100000, grp, k, pad from big where id % 3 = 2")
	other.mustRun("vacuum big")
	// The session that owns the cursor sees the new state in new
	// statements...
	h.expect("select count(*), min(grp) from big", "1999|-1")
	// ...while its cursor goes on reading the old one: every row, once, in
	// order, none of the new ones.
	got = append(got, fetch(t, rows, -1)...)
	if len(got) != 2000 {
		t.Fatalf("the cursor returned %d rows, want the 2000 that existed when it was opened", len(got))
	}
	for i, id := range got {
		if id != int64(i) {
			t.Fatalf("row %d of the cursor is id %d, want %d", i, id, i)
		}
	}
	if rows.Tag() != "SELECT 2000" {
		t.Errorf("tag is %q", rows.Tag())
	}
	// With the cursor closed, vacuum is free to remove what it protected.
	rows.Close()
	other.mustRun("vacuum big")
	h.expect("select count(*) from big", "1999")
}

// The same through an index, where the scan's place is an index key.
func TestIndexCursorReadsItsSnapshot(t *testing.T) {
	db := New()
	h, other := newHarness(t, db), newHarness(t, db)
	big(t, h, "big", 3000)
	h.mustRun("create index big_k on big (k)")
	query := "select k from big where k >= 100 and k < 700"
	if plan := h.plan(query); !strings.Contains(plan, "Index Scan using big_k") {
		t.Fatalf("not an index scan:\n%s", plan)
	}
	rows := cursor(t, h, query)
	defer rows.Close()
	got := fetch(t, rows, 5)
	other.mustRun("update big set k = k + 100000 where k % 2 = 0; delete from big where k % 5 = 1; vacuum big")
	got = append(got, fetch(t, rows, -1)...)
	if len(got) != 600 {
		t.Fatalf("the cursor returned %d rows, want 600", len(got))
	}
	for i, k := range got {
		if k != int64(100+i) {
			t.Fatalf("row %d of the cursor has k = %d, want %d", i, k, 100+i)
		}
	}
}

// A cursor abandoned half-way must release what it holds: its snapshot and
// any temporary files. The harness checks the files; this checks vacuum.
func TestAbandonedCursor(t *testing.T) {
	db := New()
	h, other := newHarness(t, db), newHarness(t, db)
	big(t, h, "big", 3000)
	h.mustRun("set work_mem = '64kB'")
	before := db.spills.Load()
	rows := cursor(t, h, "select a.id from big a join big b on a.k = b.k order by a.pad, a.id")
	fetch(t, rows, 3)
	if db.spills.Load() == before {
		t.Fatal("the query was expected to use temporary files")
	}
	if db.openSpills.Load() == 0 {
		t.Fatal("the open cursor was expected to hold temporary files")
	}
	other.mustRun("delete from big")
	rows.Close()
	rows.Close() // closing twice is harmless
	other.mustRun("vacuum big")
	if stats, _ := db.Verify(); stats.Versions != 0 {
		t.Errorf("%d row versions survive vacuum after the cursor was closed", stats.Versions)
	}
}

// ---- larger than memory ----

// Sorts, hash joins and nested loops that do not fit in work_mem go to
// disk, and must give exactly what they give in memory.
func TestSpillingGivesTheSameRows(t *testing.T) {
	db := New()
	h := newHarness(t, db)
	big(t, h, "big", 6000)
	h.mustRun("create table small (k int, label text); insert into small select k, pad from big where id % 4 = 0; insert into small values (null, 'none')")
	queries := []struct{ name, setup, query, method string }{
		{"external sort", "", "select id, k from big order by k desc", "Sort Method: external merge"},
		{"external sort, many keys", "", "select grp, pad, id from big order by grp desc, pad, id desc", "Sort Method: external merge"},
		{"hash join", "set enable_mergejoin = off",
			"select a.id, b.id from big a join big b on a.k = b.id", "Batches: 256"},
		{"hash left join", "set enable_mergejoin = off",
			"select s.label, b.id from small s left join big b on b.k = s.k", "Batches: 256"},
		{"hash full join", "set enable_mergejoin = off",
			"select s.k, b.id from small s full join big b on b.k = s.k and b.grp < 50", "Batches: 256"},
		{"merge join with sorts", "set enable_hashjoin = off",
			"select a.id, b.id from big a join big b on a.k = b.grp", "Sort Method: external merge"},
		{"nested loop", "set enable_hashjoin = off; set enable_mergejoin = off",
			"select a.id, s.k from big a join small s on a.id < s.k and a.id > s.k - 40 where a.grp < 4", ""},
	}
	for _, q := range queries {
		reset := "set enable_hashjoin = on; set enable_mergejoin = on; set enable_indexscan = off; "
		h.mustRun(reset + q.setup + "; set work_mem = '64MB'")
		want := sortedRows(h.mustRun(q.query))
		h.mustRun("set work_mem = '64kB'")
		before := db.spills.Load()
		got := sortedRows(h.mustRun(q.query))
		if db.spills.Load() == before {
			t.Errorf("%s: the query did not use temporary files", q.name)
		}
		if got != want {
			t.Errorf("%s: %d rows on disk differ from %d rows in memory", q.name, strings.Count(got, ";")+1, strings.Count(want, ";")+1)
		}
		if strings.Count(want, ";") < 100 {
			t.Errorf("%s: only %d rows; the test proves little", q.name, strings.Count(want, ";")+1)
		}
		if plan := explainAnalyze(h, q.query); !strings.Contains(plan, q.method) {
			t.Errorf("%s: expected %q in the plan:\n%s", q.name, q.method, plan)
		}
	}
	// An ordered result is compared in order: an external sort must be as
	// stable as one in memory, row for row.
	const ordered = "select grp, id from big order by grp"
	h.mustRun("set work_mem = '64MB'")
	want := h.mustRun(ordered)
	h.mustRun("set work_mem = '64kB'")
	if got := h.mustRun(ordered); got != want {
		t.Error("an external sort returned equal rows in a different order than a sort in memory")
	}
}

// A hash join whose build side is one key repeated cannot be split by
// hashing, however often it tries. It must notice and carry on.
func TestHashJoinWithOneHugeKey(t *testing.T) {
	h := newHarness(t, New())
	h.mustRun("create table dup (k int not null, n int not null); create table probe (k int not null)")
	for start := 0; start < 4000; start += 500 {
		var sb strings.Builder
		sb.WriteString("insert into dup values ")
		for i := start; i < start+500; i++ {
			if i > start {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "(7, %d)", i)
		}
		h.mustRun(sb.String())
	}
	h.mustRun("insert into probe values (7), (8), (7); analyze; set work_mem = '64kB'; set enable_mergejoin = off; set enable_indexscan = off")
	if plan := h.plan("select count(*), sum(n) from probe p join dup d on d.k = p.k"); !strings.Contains(plan, "Hash Join") {
		t.Fatalf("not a hash join:\n%s", plan)
	}
	h.expect("select count(*), sum(n) from probe p join dup d on d.k = p.k", "8000|15996000")
}

// ORDER BY with LIMIT keeps only the rows it will return. Whatever the
// limit and offset, the answer is the corresponding slice of the full sort.
func TestTopN(t *testing.T) {
	h := newHarness(t, New())
	big(t, h, "big", 3000)
	full := strings.Split(h.mustRun("select grp, id from big order by grp desc"), ";")
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 40; i++ {
		limit, offset := rng.Intn(60), rng.Intn(40)
		if i%10 == 0 {
			offset = 2990 // past most of the rows
		}
		want := full[min(offset, len(full)):min(offset+limit, len(full))]
		got := h.mustRun(fmt.Sprintf("select grp, id from big order by grp desc limit %d offset %d", limit, offset))
		if got != strings.Join(want, ";") {
			t.Fatalf("limit %d offset %d: got %.80s..., want %.80s...", limit, offset, got, strings.Join(want, ";"))
		}
	}
	if plan := explainAnalyze(h, "select id from big order by k limit 5"); !strings.Contains(plan, "Sort Method: top-N heapsort") {
		t.Errorf("ORDER BY with a small LIMIT did not use a heap:\n%s", plan)
	}
}

// ---- the pieces, on their own ----

// The sorter against the standard library, with memory small enough to
// force many runs and keys repetitive enough to make stability matter.
func TestExternalSortIsStable(t *testing.T) {
	db := New()
	h := newHarness(t, db)
	q := h.sess.(*Session).newEnv(context.Background(), nil, false).q
	defer q.cleanup()
	rng := rand.New(rand.NewSource(3))
	for _, n := range []int{0, 1, 2, 700, 5000} {
		q.workMem = 16 << 10
		rows := make([][]any, n)
		for i := range rows {
			rows[i] = []any{int64(rng.Intn(50)), int64(i), strings.Repeat("p", rng.Intn(30))}
		}
		cmp := func(a, b []any) int { return int(a[0].(int64) - b[0].(int64)) }
		so := &sorter{q: q, cmp: cmp}
		for _, r := range rows {
			if err := so.add(r); err != nil {
				t.Fatal(err)
			}
		}
		it, err := so.finish()
		if err != nil {
			t.Fatal(err)
		}
		got, err := drain(it)
		if err != nil {
			t.Fatal(err)
		}
		want := slices.Clone(rows)
		slices.SortStableFunc(want, cmp)
		if len(got) != len(want) {
			t.Fatalf("n=%d: %d rows out", n, len(got))
		}
		for i := range want {
			if got[i][1] != want[i][1] || got[i][2] != want[i][2] {
				t.Fatalf("n=%d: row %d is %v, want %v (runs: %d)", n, i, got[i], want[i], len(so.runs))
			}
		}
		if n == 5000 && so.method != "external merge" {
			t.Errorf("5000 rows in 16 kB were sorted by %s", so.method)
		}
	}
}

// Whatever is written to a temporary file must come back identical.
func TestSpillRowsRoundTrip(t *testing.T) {
	rows := [][]any{
		{},
		{nil},
		{int64(0), int64(-1), int64(1) << 62, int64(-1) << 63},
		{true, false, nil, "", "plain", "with\x00zero and é and \n"},
		{1.5, -0.0, 1e308, strings.Repeat("long", 5000)},
	}
	var buf []byte
	for _, r := range rows {
		buf = appendRow(buf, r)
	}
	rd := bufio.NewReader(bytes.NewReader(buf))
	for i, want := range rows {
		got, err := readRow(rd)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", want) {
			t.Errorf("row %d: got %#v, want %#v", i, got, want)
		}
	}
	if _, err := readRow(rd); err != io.EOF {
		t.Errorf("after the last row: %v", err)
	}
	// A file cut short is an error, not a short result.
	rd = bufio.NewReader(bytes.NewReader(buf[:len(buf)-3]))
	var err error
	for err == nil {
		_, err = readRow(rd)
	}
	if err == io.EOF {
		t.Error("a truncated file read as if it were complete")
	}
}

func TestWorkMemSetting(t *testing.T) {
	h := newHarness(t, New())
	h.expect("show work_mem", "4MB")
	h.mustRun("set work_mem = '64kB'; set work_mem = 128; set work_mem = '2GB'")
	h.expect("show work_mem", "2GB")
	h.expectError("set work_mem = '10kB'", pgerr.InvalidParameterValue)
	h.expectError("set work_mem = 'lots'", pgerr.InvalidParameterValue)
	h.expect("show work_mem", "2GB")
}

// Plans of the new operators, pinned.
func TestExecutorPlans(t *testing.T) {
	h := shop(t, New())
	h.mustRun("set enable_indexscan = off")
	// No index to look anything up with: hash the smaller side.
	h.expectPlan("select * from orders o join customer c on c.id = o.customer_id", `
		Hash Join
		  Hash Cond: (c.id = o.customer_id)
		  ->  Seq Scan on orders o
		  ->  Hash
		        ->  Seq Scan on customer c`)
	// Two tables joined on their primary keys are both already in key
	// order: merging them needs neither a hash table nor a sort.
	h.expectPlan("select * from customer c join product p on p.id = c.id", `
		Merge Join
		  Merge Cond: (p.id = c.id)
		  ->  Seq Scan on customer c
		  ->  Seq Scan on product p`)
	// An inequality can be neither hashed nor merged.
	h.expectPlan("select * from customer c join product p on p.id < c.id", `
		Nested Loop
		  Join Filter: (p.id < c.id)
		  ->  Seq Scan on customer c
		  ->  Seq Scan on product p`)
	h.expectPlan("select * from customer c full join orders o on o.customer_id = c.id and o.qty > 3", `
		Hash Full Join
		  Hash Cond: (o.customer_id = c.id)
		  Join Filter: (o.qty > 3)
		  ->  Seq Scan on customer c
		  ->  Hash
		        ->  Seq Scan on orders o`)
	// RIGHT JOIN is a left join read from the other side.
	h.expectPlan("select * from orders o right join customer c on o.customer_id = c.id", `
		Hash Left Join
		  Hash Cond: (o.customer_id = c.id)
		  ->  Seq Scan on customer c
		  ->  Hash
		        ->  Seq Scan on orders o`)
	h.expectPlan("select id from customer union select id from product order by 1 limit 3", `
		Limit
		  ->  Sort
		        Sort Key: 1
		        ->  Unique
		              ->  Append
		                    ->  Seq Scan on customer
		                    ->  Seq Scan on product`)
	h.expectPlan("select id from customer except all select customer_id from orders", `
		HashSetOp Except All
		  ->  Seq Scan on customer
		  ->  Seq Scan on orders`)
}

// heapDuring runs a query through a cursor and returns the largest amount of
// live heap seen while its rows were being read, beyond what was live
// before it started.
func heapDuring(t *testing.T, h *harness, query string) (peak int64, rows int) {
	t.Helper()
	live := func() int64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return int64(m.HeapAlloc)
	}
	base := live()
	cur := cursor(t, h, query)
	defer cur.Close()
	for {
		_, err := cur.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if rows++; rows%4000 == 1 {
			peak = max(peak, live()-base)
		}
	}
	return peak, rows
}

// The point of the whole executor: a query's memory is bounded by work_mem,
// not by the size of what it reads. The same sort and the same join are
// measured with a work_mem that holds everything and with one that holds
// almost nothing.
func TestMemoryIsBoundedByWorkMem(t *testing.T) {
	if testing.Short() {
		t.Skip("measures the heap; slow")
	}
	h := newHarness(t, New())
	big(t, h, "big", 40000)
	h.mustRun("set enable_indexscan = off; set enable_mergejoin = off")
	for _, q := range []struct{ name, query string }{
		{"scan", "select id, pad from big"},
		{"sort", "select id, pad from big order by k"},
		{"hash join", "select a.id, b.pad from big a join big b on a.k = b.id"},
	} {
		h.mustRun("set work_mem = '1GB'")
		roomy, n := heapDuring(t, h, q.query)
		h.mustRun("set work_mem = '256kB'")
		tight, m := heapDuring(t, h, q.query)
		if n != 40000 || m != 40000 {
			t.Fatalf("%s: %d and %d rows", q.name, n, m)
		}
		t.Logf("%-9s 40000 rows: %5d kB of heap with work_mem = 1GB, %5d kB with work_mem = 256kB", q.name, roomy>>10, tight>>10)
		// What a tight work_mem may still hold: its own allowance several
		// times over (run buffers, a batch of rows, one partition).
		if tight > 5<<20 {
			t.Errorf("%s: %d kB of heap in use with work_mem = 256kB", q.name, tight>>10)
		}
		if q.name != "scan" && roomy < 2*tight {
			t.Errorf("%s: %d kB with room and %d kB without; the comparison shows nothing", q.name, roomy>>10, tight>>10)
		}
	}
}

// Each way of joining on an equality must agree with "=" about what is
// equal: an integer equals the double precision of the same value, NULL
// equals nothing, and a key that occurs several times on both sides gives
// every pair.
func TestJoinKeysByEveryMethod(t *testing.T) {
	h := newHarness(t, New())
	h.mustRun(`
		create table i (k int, tag text);
		create table f (k double precision, tag text);
		insert into i values (1, 'i1'), (2, 'i2'), (3, 'i3a'), (3, 'i3b'), (null, 'in'), (0, 'i0');
		insert into f values (1.0, 'f1'), (2.5, 'f2'), (3.0, 'f3a'), (3.0, 'f3b'), (3.0, 'f3c'), (null, 'fn'), (-0.0, 'f0')`)
	const query = "select i.tag, f.tag from i join f on i.k = f.k order by 1, 2"
	const want = "i0|f0;i1|f1;i3a|f3a;i3a|f3b;i3a|f3c;i3b|f3a;i3b|f3b;i3b|f3c"
	for _, m := range []struct{ setting, node string }{
		{"set enable_nestloop = on; set enable_hashjoin = off; set enable_mergejoin = off", "Nested Loop"},
		{"set enable_nestloop = off; set enable_hashjoin = on; set enable_mergejoin = off", "Hash Join"},
		{"set enable_nestloop = off; set enable_hashjoin = off; set enable_mergejoin = on", "Merge Join"},
	} {
		h.mustRun(m.setting)
		if plan := h.plan(query); !strings.Contains(plan, m.node) {
			t.Fatalf("expected a %s:\n%s", m.node, plan)
		}
		h.expect(query, want)
		// Text keys, and a key that is an expression.
		h.expect("select count(*) from i a join i b on a.tag = b.tag", "6")
		h.expect("select a.tag, b.tag from i a join i b on a.k + 1 = b.k order by 1, 2", "i0|i1;i1|i2;i2|i3a;i2|i3b")
	}
}
