package engine

import (
	"fmt"
	"path/filepath"
	"testing"
)

// These benchmarks measure what durability costs: the same inserts with the
// log on an in-memory file, on a real file without fsync, and on a real
// file with an fsync per commit. docs/benchmarks.md explains the numbers.

func benchDB(b *testing.B, mode string) *DB {
	b.Helper()
	var db *DB
	var err error
	switch mode {
	case "memory":
		db = New()
	case "file, no fsync":
		db, err = Open(filepath.Join(b.TempDir(), "bench.cdb"), Options{NoSync: true})
	default:
		db, err = Open(filepath.Join(b.TempDir(), "bench.cdb"), Options{})
	}
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	return db
}

var benchModes = []string{"memory", "file, no fsync", "file, fsync"}

func BenchmarkInsertOnePerTransaction(b *testing.B) {
	for _, mode := range benchModes {
		b.Run(mode, func(b *testing.B) {
			db := benchDB(b, mode)
			sess, _ := db.NewSession(map[string]string{"user": "bench"})
			if err := execSQL(sess, "create table t (id int primary key, name text, n bigint)"); err != nil {
				b.Fatal(err)
			}
			stmts, _ := sess.Parse("insert into t values ($1, $2, $3)")
			p, err := sess.Prepare(stmts[0], nil)
			if err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := p.Execute(b.Context(), []any{int64(i), "capivara", int64(i)}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkInsertHundredPerTransaction(b *testing.B) {
	for _, mode := range benchModes {
		b.Run(mode, func(b *testing.B) {
			db := benchDB(b, mode)
			sess, _ := db.NewSession(map[string]string{"user": "bench"})
			if err := execSQL(sess, "create table t (id int primary key, name text, n bigint)"); err != nil {
				b.Fatal(err)
			}
			stmts, _ := sess.Parse("insert into t values ($1, $2, $3)")
			p, err := sess.Prepare(stmts[0], nil)
			if err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if i%100 == 0 {
					if err := execSQL(sess, "begin"); err != nil {
						b.Fatal(err)
					}
				}
				if _, err := p.Execute(b.Context(), []any{int64(i), "capivara", int64(i)}); err != nil {
					b.Fatal(err)
				}
				if i%100 == 99 || i == b.N-1 {
					if err := execSQL(sess, "commit"); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "rows/s")
		})
	}
}

// The benchmarks below compare the plan the planner chooses with the plan
// it rejected, on the same data and the same query, by turning its switches
// off. That is the only fair way to say what a planner is worth.

const benchOrders = 20000

func benchShop(b *testing.B) (*DB, *harness) {
	b.Helper()
	db := New()
	sess, _ := db.NewSession(map[string]string{"user": "bench"})
	run := func(q string) {
		if err := execSQL(sess, q); err != nil {
			b.Fatal(err)
		}
	}
	run(`create table customer (id int primary key, name text not null);
	     create table product (id int primary key, name text not null);
	     create table orders (id int primary key, customer_id int not null, product_id int not null, qty int not null);
	     create index orders_customer on orders (customer_id);
	     create unique index customer_name on customer (name)`)
	insert := func(table string, n int, row func(i int) string) {
		for start := 0; start < n; start += 500 {
			q := "insert into " + table + " values "
			for i := start; i < start+500 && i < n; i++ {
				if i > start {
					q += ","
				}
				q += row(i)
			}
			run(q)
		}
	}
	insert("customer", 1000, func(i int) string { return fmt.Sprintf("(%d, 'c%d')", i, i) })
	insert("product", 100, func(i int) string { return fmt.Sprintf("(%d, 'p%d')", i, i) })
	insert("orders", benchOrders, func(i int) string {
		return fmt.Sprintf("(%d, %d, %d, %d)", i, (i*7)%1000, (i*3)%100, 1+i%5)
	})
	run("analyze")
	b.Cleanup(func() { sess.Close() })
	return db, &harness{sess: sess}
}

// benchQuery prepares a statement under the given planner settings and runs
// it b.N times, with a different parameter each time.
func benchQuery(b *testing.B, h *harness, settings, query string, param func(i int) any) {
	b.Helper()
	if err := execSQL(h.sess, settings); err != nil {
		b.Fatal(err)
	}
	stmts, _ := h.sess.Parse(query)
	p, err := h.sess.Prepare(stmts[0], nil)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := p.Execute(b.Context(), []any{param(i)})
		if err != nil {
			b.Fatal(err)
		}
		for {
			if _, err := rows.Next(b.Context()); err != nil {
				break
			}
		}
	}
}

const (
	everything = "set enable_indexscan = on; set join_collapse_limit = 8; set enable_hashjoin = on; set enable_mergejoin = on; set work_mem = '64MB'; "
	planned    = everything
	noIndexes  = everything + "set enable_indexscan = off"
	// naive is the executor with nothing to choose from: no indexes, joins
	// in the order written, by nested loops.
	naive     = noIndexes + "; set join_collapse_limit = 1; set enable_hashjoin = off; set enable_mergejoin = off"
	hashOnly  = noIndexes + "; set enable_mergejoin = off"
	mergeOnly = noIndexes + "; set enable_hashjoin = off"
	loopOnly  = noIndexes + "; set enable_hashjoin = off; set enable_mergejoin = off"
)

func BenchmarkPointLookup(b *testing.B) {
	_, h := benchShop(b)
	for _, mode := range []struct{ name, settings string }{{"index scan", planned}, {"sequential scan", noIndexes}} {
		b.Run(mode.name, func(b *testing.B) {
			benchQuery(b, h, mode.settings, "select * from orders where id = $1",
				func(i int) any { return int64(i * 7919 % benchOrders) })
		})
	}
}

func BenchmarkSecondaryIndex(b *testing.B) {
	_, h := benchShop(b)
	for _, mode := range []struct{ name, settings string }{{"index scan", planned}, {"sequential scan", noIndexes}} {
		b.Run(mode.name, func(b *testing.B) {
			benchQuery(b, h, mode.settings, "select * from orders where customer_id = $1",
				func(i int) any { return int64(i % 1000) })
		})
	}
}

func BenchmarkUpdateByKey(b *testing.B) {
	_, h := benchShop(b)
	for _, mode := range []struct{ name, settings string }{{"index scan", planned}, {"sequential scan", noIndexes}} {
		b.Run(mode.name, func(b *testing.B) {
			benchQuery(b, h, mode.settings, "update orders set qty = qty + 1 where id = $1",
				func(i int) any { return int64(i * 7919 % benchOrders) })
		})
	}
}

// One customer's orders with their products: three tables, written with the
// largest first.
func BenchmarkThreeTableJoin(b *testing.B) {
	_, h := benchShop(b)
	query := `select o.id, p.name from orders o, customer c, product p
	          where o.customer_id = c.id and o.product_id = p.id and c.name = $1`
	for _, mode := range []struct{ name, settings string }{
		{"planned", planned},
		{"no indexes", noIndexes},
		{"no indexes, nested loops as written", naive},
	} {
		b.Run(mode.name, func(b *testing.B) {
			benchQuery(b, h, mode.settings, query, func(i int) any { return fmt.Sprintf("c%d", i%1000) })
		})
	}
}

// Every order with its customer: 20 000 rows against 1 000, with no index
// to help. What the join method alone is worth.
func BenchmarkJoinMethod(b *testing.B) {
	_, h := benchShop(b)
	query := "select o.id, c.name from orders o join customer c on c.id = o.customer_id where o.qty > $1"
	for _, mode := range []struct{ name, settings string }{
		{"hash join", hashOnly},
		{"merge join", mergeOnly},
		{"nested loop", loopOnly},
		{"hash join on disk", hashOnly + "; set work_mem = '64kB'"},
	} {
		b.Run(mode.name, func(b *testing.B) {
			benchQuery(b, h, mode.settings, query, func(i int) any { return int64(0) })
		})
	}
}

// Sorting 20 000 rows: in memory, on disk, and when only the first ten are
// wanted.
func BenchmarkSort(b *testing.B) {
	_, h := benchShop(b)
	for _, mode := range []struct{ name, settings, query string }{
		{"in memory", everything, "select id from orders where qty > $1 order by product_id, id desc"},
		{"external, work_mem 64kB", everything + "set work_mem = '64kB'", "select id from orders where qty > $1 order by product_id, id desc"},
		{"top 10", everything, "select id from orders where qty > $1 order by product_id, id desc limit 10"},
	} {
		b.Run(mode.name, func(b *testing.B) {
			benchQuery(b, h, mode.settings, mode.query, func(i int) any { return int64(0) })
		})
	}
}

// What not computing the rows nobody asked for is worth.
func BenchmarkFirstRows(b *testing.B) {
	_, h := benchShop(b)
	for _, mode := range []struct{ name, query string }{
		{"all 20000 rows", "select * from orders where qty > $1"},
		{"limit 10", "select * from orders where qty > $1 limit 10"},
		{"exists", "select exists (select 1 from orders where qty > $1)"},
	} {
		b.Run(mode.name, func(b *testing.B) {
			benchQuery(b, h, planned, mode.query, func(i int) any { return int64(0) })
		})
	}
}

// A set operation over the two halves of a table.
func BenchmarkSetOperation(b *testing.B) {
	_, h := benchShop(b)
	for _, op := range []string{"union all", "union", "intersect", "except"} {
		b.Run(op, func(b *testing.B) {
			benchQuery(b, h, planned,
				"select customer_id from orders where id < 10000 and qty > $1 "+op+" select customer_id from orders where id >= 5000",
				func(i int) any { return int64(0) })
		})
	}
}

// The first ten rows in some order: read off a key that is already in that
// order, or found by going through the whole table.
func BenchmarkOrderByLimit(b *testing.B) {
	_, h := benchShop(b)
	for _, mode := range []struct{ name, query string }{
		{"primary key order", "select * from orders where qty > $1 order by id limit 10"},
		{"index order", "select * from orders where qty > $1 order by customer_id limit 10"},
		{"sorted (the same, defeated by an expression)", "select * from orders where qty > $1 order by customer_id + 0 limit 10"},
	} {
		b.Run(mode.name, func(b *testing.B) {
			benchQuery(b, h, planned, mode.query, func(i int) any { return int64(0) })
		})
	}
}

// One group per order: 20 000 groups, in memory and with a work_mem that
// holds a few hundred of them.
func BenchmarkGroupBy(b *testing.B) {
	_, h := benchShop(b)
	for _, mode := range []struct{ name, settings, query string }{
		{"20000 groups in memory", everything, "select id, count(*) from orders where qty > $1 group by id"},
		{"20000 groups, work_mem 64kB", everything + "set work_mem = '64kB'", "select id, count(*) from orders where qty > $1 group by id"},
		{"1000 groups, work_mem 64kB", everything + "set work_mem = '64kB'", "select customer_id, count(*) from orders where qty > $1 group by customer_id"},
		{"distinct, work_mem 64kB", everything + "set work_mem = '64kB'", "select distinct id from orders where qty > $1"},
	} {
		b.Run(mode.name, func(b *testing.B) {
			benchQuery(b, h, mode.settings, mode.query, func(i int) any { return int64(0) })
		})
	}
}
