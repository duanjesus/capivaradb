package engine

import (
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
