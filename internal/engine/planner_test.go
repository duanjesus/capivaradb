package engine

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// shop loads a schema large enough for the planner's choices to matter: a
// thousand orders, a hundred customers, twenty products.
func shop(t *testing.T, db *DB) *harness {
	t.Helper()
	h := newHarness(t, db)
	h.mustRun(`
		create table customer (id int primary key, name text not null, city text);
		create table product (id int primary key, name text not null, price int not null);
		create table orders (id int primary key, customer_id int not null, product_id int not null, qty int not null, note text);
		create index orders_customer on orders (customer_id);
		create index orders_product_qty on orders (product_id, qty);
		create unique index customer_name on customer (name);
	`)
	var sb strings.Builder
	sb.WriteString("insert into customer values ")
	for i := 0; i < 100; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "(%d, 'c%d', 'city%d')", i, i, i%10)
	}
	sb.WriteString("; insert into product values ")
	for i := 0; i < 20; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "(%d, 'p%d', %d)", i, i, 10+i*5)
	}
	sb.WriteString("; insert into orders values ")
	for i := 0; i < 1000; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "(%d, %d, %d, %d, null)", i, (i*7)%100, (i*3)%20, 1+i%5)
	}
	h.mustRun(sb.String())
	h.mustRun("analyze")
	return h
}

// plan returns the plan of a query as EXPLAIN (COSTS OFF) prints it.
func (h *harness) plan(query string) string {
	h.t.Helper()
	out := h.mustRun("explain (costs off) " + query)
	return strings.ReplaceAll(out, ";", "\n")
}
func (h *harness) expectPlan(query, want string) {
	h.t.Helper()
	want = strings.TrimSpace(strings.ReplaceAll(want, "\n\t\t", "\n"))
	if got := h.plan(query); got != want {
		h.t.Errorf("plan of %s\n--- got:\n%s\n--- want:\n%s", query, got, want)
	}
}
func TestAccessPathChoice(t *testing.T) {
	h := shop(t, New())
	// The primary key picks out one row.
	h.expectPlan("select * from orders where id = 42", `
		Index Scan using orders_pkey on orders
		  Index Cond: (id = 42)`)
	// A range of the primary key.
	h.expectPlan("select * from orders where id >= 10 and id < 20", `
		Index Scan using orders_pkey on orders
		  Index Cond: (id >= 10) AND (id < 20)`)
	// A secondary index, with the condition it cannot help with left as a
	// filter.
	h.expectPlan("select * from orders where customer_id = 7 and qty > 2", `
		Index Scan using orders_customer on orders
		  Index Cond: (customer_id = 7)
		  Filter: (qty > 2)`)
	// A two-column index: equality on the first, range on the second.
	h.expectPlan("select * from orders where product_id = 3 and qty between 2 and 4", `
		Index Scan using orders_product_qty on orders
		  Index Cond: (product_id = 3) AND (qty between 2 and 4)`)
	// The condition may be written either way round.
	h.expectPlan("select * from customer where 'c5' = name", `
		Index Scan using customer_name on customer
		  Index Cond: ('c5' = name)`)
	// Parameters work as well as literals.
	h.expectPlan("select * from orders where id = $1", `
		Index Scan using orders_pkey on orders
		  Index Cond: (id = $1)`)
	// No index helps: the second column of an index without the first, an
	// expression over the column, an OR, a column with no index.
	for _, where := range []string{"qty = 3", "id + 1 = 5", "id = 1 or id = 2", "note = 'x'", "id <> 5"} {
		if got := h.plan("select * from orders where " + where); !strings.HasPrefix(got, "Seq Scan on orders") {
			t.Errorf("where %s: expected a sequential scan, got\n%s", where, got)
		}
	}
	// An index that would return most of the table is not worth its
	// per-row cost.
	if got := h.plan("select * from orders where customer_id >= 0"); !strings.HasPrefix(got, "Seq Scan") {
		t.Errorf("unselective range: expected a sequential scan, got\n%s", got)
	}
	// Turning index scans off gives the plan that was rejected.
	h.mustRun("set enable_indexscan = off")
	h.expectPlan("select * from orders where id = 42", `
		Seq Scan on orders
		  Filter: (id = 42)`)
}
func TestIndexScanResults(t *testing.T) {
	h := shop(t, New())
	// Every way of reaching rows must return the same rows as a scan.
	queries := []string{
		"select id from orders where id = 42",
		"select id from orders where id = 4242",
		"select id from orders where id > 990",
		"select id from orders where id >= 990",
		"select id from orders where id < 3",
		"select id from orders where id <= 3",
		"select id from orders where id between 500 and 505",
		"select id from orders where id > 10 and id < 12",
		"select id from orders where id > 12 and id < 10",
		"select id from orders where customer_id = 7 and qty > 2",
		"select id from orders where product_id = 3 and qty = 2",
		"select id from orders where product_id = 3 and qty >= 4",
		"select id from orders where product_id = 3 and qty > 1 and qty < 4",
		"select id from orders where product_id = 19",
		"select id from orders where product_id = null",
		"select id from orders where id = 2.0",
		"select id from customer where name = 'c50'",
		"select id from customer where name >= 'c98'",
		"select id from customer where name < 'c1'",
	}
	results := make(map[string]string)
	for _, q := range queries {
		results[q] = sortedRows(h.mustRun(q))
	}
	h.mustRun("set enable_indexscan = off")
	for _, q := range queries {
		if got := sortedRows(h.mustRun(q)); got != results[q] {
			t.Errorf("%s:\n with indexes: %s\n     without: %s", q, results[q], got)
		}
	}
	// And the ones that must find something did.
	if results["select id from orders where id = 42"] != "42" || results["select id from customer where name = 'c50'"] != "50" {
		t.Errorf("point lookups: %q, %q", results["select id from orders where id = 42"], results["select id from customer where name = 'c50'"])
	}
}
func sortedRows(result string) string {
	rows := strings.Split(result, ";")
	sort.Strings(rows)
	return strings.Join(rows, ";")
}
func TestPredicatePushdown(t *testing.T) {
	h := shop(t, New())
	h.mustRun("set enable_indexscan = off") // show where filters go, not which index
	// Each condition is checked at the first point where its tables are
	// available: one-table conditions in the scans, the rest in the join.
	h.expectPlan("select * from product p, customer c where p.price > 100 and c.city = 'city3' and p.id = c.id", `
		Nested Loop
		  Join Filter: (p.id = c.id)
		  ->  Seq Scan on product p
		        Filter: (p.price > 100)
		  ->  Seq Scan on customer c
		        Filter: (c.city = 'city3')`)
	// INNER JOIN ... ON is the same thing as a comma and a WHERE.
	a := h.plan("select * from product p join customer c on p.id = c.id where p.price > 100")
	b := h.plan("select * from product p, customer c where p.id = c.id and p.price > 100")
	if a != b {
		t.Errorf("JOIN ON and comma+WHERE planned differently:\n%s\n---\n%s", a, b)
	}
}
func TestJoinOrder(t *testing.T) {
	h := shop(t, New())
	// Written largest table first. The planner starts from the one row of
	// customer the query asks for and reaches the orders through an index.
	h.expectPlan("select * from orders o, customer c, product p where o.customer_id = c.id and o.product_id = p.id and c.name = 'c7'", `
		Nested Loop
		  ->  Nested Loop
		        ->  Index Scan using customer_name on customer c
		              Index Cond: (c.name = 'c7')
		        ->  Index Scan using orders_customer on orders o
		              Index Cond: (o.customer_id = c.id)
		  ->  Index Scan using product_pkey on product p
		        Index Cond: (o.product_id = p.id)`)
	// With reordering off the joins run as written.
	h.mustRun("set join_collapse_limit = 1; set enable_indexscan = off")
	h.expectPlan("select * from orders o, customer c, product p where o.customer_id = c.id and o.product_id = p.id and c.name = 'c7'", `
		Nested Loop
		  Join Filter: (o.product_id = p.id)
		  ->  Nested Loop
		        Join Filter: (o.customer_id = c.id)
		        ->  Seq Scan on orders o
		        ->  Seq Scan on customer c
		              Filter: (c.name = 'c7')
		  ->  Seq Scan on product p`)
}
func TestLeftJoinPlans(t *testing.T) {
	h := shop(t, New())
	// The right side of a left join is looked up through its index; the
	// WHERE condition on the left side is pushed into the left scan.
	h.expectPlan("select * from customer c left join orders o on o.customer_id = c.id where c.id < 3", `
		Nested Loop Left Join
		  Join Filter: (o.customer_id = c.id)
		  ->  Index Scan using customer_pkey on customer c
		        Index Cond: (c.id < 3)
		  ->  Index Scan using orders_customer on orders o
		        Index Cond: (o.customer_id = c.id)`)
	// A WHERE condition on the right side must stay above the join: below
	// it, it would turn "no match" rows into matches that were filtered.
	h.mustRun("set enable_indexscan = off")
	h.expectPlan("select * from customer c left join orders o on o.customer_id = c.id where o.id is null", `
		Nested Loop Left Join
		  Join Filter: (o.customer_id = c.id)
		  Filter: (o.id is null)
		  ->  Seq Scan on customer c
		  ->  Seq Scan on orders o`)
	// An ON condition on the right side goes into the right scan; one on
	// the left side cannot be pushed, since it does not remove left rows.
	h.expectPlan("select * from customer c left join orders o on o.customer_id = c.id and o.qty = 5 and c.city = 'city1'", `
		Nested Loop Left Join
		  Join Filter: (o.customer_id = c.id) AND (c.city = 'city1')
		  ->  Seq Scan on customer c
		  ->  Seq Scan on orders o
		        Filter: (o.qty = 5)`)
}
func TestExplainUpperNodes(t *testing.T) {
	h := shop(t, New())
	h.expectPlan("select customer_id, count(*) from orders where qty > 1 group by customer_id having count(*) > 5 order by 2 desc limit 3", `
		Limit
		  ->  Sort
		        Sort Key: 2
		        ->  HashAggregate
		              Group Key: customer_id
		              Filter: (count(*) > 5)
		              ->  Seq Scan on orders
		                    Filter: (qty > 1)`)
	h.expectPlan("select distinct city from customer", `
		Unique
		  ->  Seq Scan on customer`)
	h.expectPlan("select 1 where 1 = 2", `
		Result
		  One-Time Filter: (1 = 2)`)
	h.expectPlan("select s.n from (select count(*) as n from orders) s where s.n > 0", `
		Subquery Scan on s
		  Filter: (s.n > 0)
		  ->  Aggregate
		        ->  Seq Scan on orders`)
	// Data-modifying statements find their rows the same way.
	h.expectPlan("update orders set qty = 9 where id = 5", `
		Update on orders
		  ->  Index Scan using orders_pkey on orders
		        Index Cond: (id = 5)`)
	h.expectPlan("delete from orders where customer_id = 3 and qty = 1", `
		Delete on orders
		  ->  Index Scan using orders_customer on orders
		        Index Cond: (customer_id = 3)
		        Filter: (qty = 1)`)
	h.expectError("explain create table x (a int)", "42601")
	// Found by the parser fuzzer: this used to print as "explain analyze orders".
	h.expectError("explain (costs on) analyze orders", "42601")
}
func TestExplainAnalyze(t *testing.T) {
	h := shop(t, New())
	out := h.mustRun("explain (analyze, costs off, timing off) select * from customer c join orders o on o.customer_id = c.id where c.id < 3")
	want := strings.Join([]string{
		"Nested Loop (actual rows=30 loops=1)",
		"  ->  Index Scan using customer_pkey on customer c (actual rows=3 loops=1)",
		"        Index Cond: (c.id < 3)",
		"  ->  Index Scan using orders_customer on orders o (actual rows=30 loops=3)",
		"        Index Cond: (o.customer_id = c.id)",
	}, ";")
	if out != want {
		t.Errorf("explain analyze:\n got %s\nwant %s", strings.ReplaceAll(out, ";", "\n     "), strings.ReplaceAll(want, ";", "\n     "))
	}
	// The estimates are shown next to what happened.
	out = h.mustRun("explain analyze select * from orders where customer_id = 7")
	if !strings.Contains(out, "rows=10)") || !strings.Contains(out, "actual rows=10 loops=1") || !strings.Contains(out, "Execution Time:") {
		t.Errorf("explain analyze with costs:\n%s", strings.ReplaceAll(out, ";", "\n"))
	}
	// EXPLAIN ANALYZE really runs the statement.
	h.mustRun("explain analyze delete from orders where id = 0")
	h.expect("select count(*) from orders where id = 0", "0")
	// Plain EXPLAIN does not.
	h.mustRun("explain delete from orders where id = 1")
	h.expect("select count(*) from orders where id = 1", "1")
}
func TestStatistics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.cdb")
	db, err := Open(path, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	h := shop(t, db)
	estimate := func(h *harness, query string) string {
		t.Helper()
		first := strings.Split(h.mustRun("explain "+query), ";")[0]
		return first[strings.Index(first, "rows=")+5 : len(first)-1]
	}
	// Estimates come from what ANALYZE counted: 1000 orders over 100
	// customers is 10 per customer; qty has 5 distinct values.
	for query, want := range map[string]string{
		"select * from orders":                       "1000",
		"select * from orders where customer_id = 7": "10",
		"select * from orders where qty = 3":         "200",
		"select * from orders where id = 5":          "1",
		"select * from orders where id < 100":        "100",
		"select * from customer where city = 'x'":    "10",
		"select * from orders where note is null":    "1000",
	} {
		if got := estimate(h, query); got != want {
			t.Errorf("%s: estimated %s rows, want %s", query, got, want)
		}
	}
	h.sess.Close()
	// Statistics survive a restart.
	db = reopen(t, db, path, 256)
	defer db.Close()
	h = newHarness(t, db)
	if got := estimate(h, "select * from orders where customer_id = 7"); got != "10" {
		t.Errorf("after restart: estimated %s rows, want 10", got)
	}
	// A table that changes enough is analysed again on its own.
	h.mustRun("create table grow (id int primary key, k int)")
	for i := 0; i < 300; i++ {
		h.mustRun(fmt.Sprintf("insert into grow values (%d, %d)", i, i%3))
	}
	if got := estimate(h, "select * from grow where k = 1"); got != "100" {
		// 300 rows, 3 distinct values of k; the exact figure depends on
		// when the last automatic ANALYZE ran, so allow some slack.
		var n int
		fmt.Sscan(got, &n)
		if n < 70 || n > 130 {
			t.Errorf("after automatic analyze: estimated %s rows for k = 1, want about 100", got)
		}
	}
	// Dropping the table drops its statistics; a new table of the same
	// name starts without any.
	h.mustRun("drop table grow; create table grow (id int primary key, k int); insert into grow values (1, 1)")
	if got := estimate(h, "select * from grow"); got != "1" {
		t.Errorf("recreated table: estimated %s rows, want 1", got)
	}
	h.expectError("analyze missing", "42P01")
}

// TestPlansAgree is the planner's safety net. Whatever the planner decides,
// the rows a query returns must be the same; so random queries are run
// under every combination of the planner's switches and the results
// compared. A wrong pushdown, a join condition applied at the wrong step, a
// range scan that misses a boundary: all show up as a difference.
func TestPlansAgree(t *testing.T) {
	h := shop(t, New())
	h.mustRun(`
		insert into orders values (2000, 5, 7, 2, 'gift'), (2001, 5, 19, 5, null), (2002, 99, 0, 1, 'x');
		create table tag (order_id int, label text);
		create index tag_order on tag (order_id);
		insert into tag values (1, 'a'), (1, 'b'), (2, 'a'), (42, 'c'), (2000, 'd'), (null, 'e'), (7777, 'f');
		analyze`)
	rng := rand.New(rand.NewSource(1))
	pick := func(options ...string) string { return options[rng.Intn(len(options))] }
	num := func(n int) string { return fmt.Sprint(rng.Intn(n)) }
	cond := func() string {
		switch rng.Intn(14) {
		case 0:
			return "o.id = " + num(1100)
		case 1:
			return "o.id " + pick("<", "<=", ">", ">=") + " " + num(1100)
		case 2:
			return "o.customer_id = " + num(110)
		case 3:
			return "o.product_id = " + num(22) + " and o.qty " + pick("=", "<", ">=") + " " + num(7)
		case 4:
			return "c.name = 'c" + num(110) + "'"
		case 5:
			return "c.city = 'city" + num(11) + "'"
		case 6:
			return "p.price " + pick("<", ">") + " " + num(120)
		case 7:
			return "o.qty between " + num(4) + " and " + num(7)
		case 8:
			return "(o.id < " + num(50) + " or c.id > " + num(100) + ")"
		case 9:
			return "o.note is " + pick("null", "not null")
		case 10:
			return "c.id in (" + num(100) + ", " + num(100) + ", " + num(100) + ")"
		case 11:
			return "exists (select 1 from tag t2 where t2.order_id = o.id)"
		case 12:
			return "o.qty > (select count(*) from tag t3 where t3.order_id = o.id)"
		}
		return "p.id = " + num(22)
	}
	from := []string{
		"orders o, customer c, product p where o.customer_id = c.id and o.product_id = p.id",
		"product p, orders o, customer c where o.product_id = p.id and c.id = o.customer_id",
		"customer c join orders o on o.customer_id = c.id join product p on p.id = o.product_id where true",
		"orders o join product p on o.product_id = p.id join customer c on c.id = o.customer_id where true",
		"customer c left join orders o on o.customer_id = c.id left join product p on p.id = o.product_id where true",
		"orders o left join tag t on t.order_id = o.id join customer c on c.id = o.customer_id join product p on p.id = o.product_id where true",
		"customer c join orders o on o.customer_id = c.id and o.qty > 2 left join product p on p.id = o.product_id and p.price > 50 where true",
		"(select * from orders where qty < 4) o join customer c on c.id = o.customer_id join product p on p.id = o.product_id where true",
	}
	settings := []string{
		"set enable_indexscan = on; set join_collapse_limit = 8",
		"set enable_indexscan = off; set join_collapse_limit = 8",
		"set enable_indexscan = on; set join_collapse_limit = 1",
		"set enable_indexscan = off; set join_collapse_limit = 1",
	}
	nonEmpty := 0
	for i := 0; i < 400; i++ {
		query := "select o.id, c.id, p.id from " + pick(from...)
		for n := rng.Intn(4); n > 0; n-- {
			query += " and " + cond()
		}
		var reference string
		for j, setting := range settings {
			h.mustRun(setting)
			got := sortedRows(h.mustRun(query))
			if j == 0 {
				reference = got
				if got != "" {
					nonEmpty++
				}
			} else if got != reference {
				t.Fatalf("query %d gives different rows under %q:\n%s\n--- optimised (%d rows):\n%.300s\n--- this setting (%d rows):\n%.300s\n--- optimised plan:\n%s",
					i, setting, query, strings.Count(reference, ";")+1, reference, strings.Count(got, ";")+1, got, func() string {
						h.mustRun(settings[0])
						return h.plan(query)
					}())
			}
		}
	}
	// A test that compares four empty results proves nothing.
	if nonEmpty < 150 {
		t.Errorf("only %d of 400 random queries returned rows; the generator is too restrictive", nonEmpty)
	}
	t.Logf("400 random queries, %d with results, each run under 4 planner settings: all agree", nonEmpty)
}

// A condition containing a subquery may depend on any table of the FROM
// clause without naming one outside the subquery. It must not be evaluated
// until every table is in place, whatever the join order.
func TestSubqueryConditionsInJoins(t *testing.T) {
	h := shop(t, New())
	h.mustRun("create table vip (customer_id int primary key); insert into vip values (3), (5)")
	// Orders of VIP customers for products dearer than 40: those of customer
	// 3, whose orders are all for a product at 45, and not those of customer
	// 5, whose product costs 35.
	want := h.mustRun(`select count(*) from orders o, customer c, product p
		where o.customer_id = c.id and o.product_id = p.id and p.price > 40 and c.id in (3, 5)`)
	if want == "0" {
		t.Fatal("the reference query should find something")
	}
	for _, from := range []string{"orders o, customer c, product p", "product p, customer c, orders o", "customer c, product p, orders o"} {
		h.expect(`select count(*) from `+from+`
			where o.customer_id = c.id and o.product_id = p.id and p.price > 40
			  and exists (select 1 from vip v where v.customer_id = c.id)`, want)
		h.expect(`select count(*) from `+from+`
			where o.customer_id = c.id and o.product_id = p.id
			  and p.price > (select 40 from vip v where v.customer_id = c.id)`, want)
	}
}
