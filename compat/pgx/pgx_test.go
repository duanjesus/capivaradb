// Package pgxcompat checks CapivaraDB against pgx, the most widely used Go
// PostgreSQL driver. It lives in its own module so that the driver never
// becomes a dependency of the database itself.
package pgxcompat

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/duanjesus/capivaradb/internal/engine"
	"github.com/duanjesus/capivaradb/internal/pgwire"
)

func startServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &pgwire.Server{Handler: engine.New()}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return fmt.Sprintf("postgres://ana@%s/capi", ln.Addr())
}

func connect(t *testing.T, url string) (*pgx.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn, ctx
}

func seed(t *testing.T, conn *pgx.Conn, ctx context.Context) {
	t.Helper()
	_, err := conn.Exec(ctx, `create table users (id int primary key, name text not null, age bigint, score float8, active bool)`)
	if err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		id     int32
		name   string
		age    *int64
		score  float64
		active bool
	}{
		{1, "ana", ptr(int64(30)), 9.5, true},
		{2, "bia", nil, 7.25, false},
		{3, "caio", ptr(int64(41)), 0, true},
	}
	for _, r := range rows {
		tag, err := conn.Exec(ctx, "insert into users values ($1, $2, $3, $4, $5)", r.id, r.name, r.age, r.score, r.active)
		if err != nil {
			t.Fatal(err)
		}
		if tag.RowsAffected() != 1 || !tag.Insert() {
			t.Fatalf("command tag: %s", tag)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestConnectAndPing(t *testing.T) {
	conn, ctx := connect(t, startServer(t))
	if err := conn.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if v := conn.PgConn().ParameterStatus("server_version"); v != "16.0" {
		t.Errorf("server_version: %q", v)
	}
	var version string
	if err := conn.QueryRow(ctx, "select version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Log(version)
}

// The default mode: statements are prepared and cached, parameters and
// results travel in binary where pgx knows the type.
func TestQueryWithParameters(t *testing.T) {
	conn, ctx := connect(t, startServer(t))
	seed(t, conn, ctx)

	var (
		id     int32
		name   string
		age    *int64
		score  float64
		active bool
	)
	err := conn.QueryRow(ctx, "select id, name, age, score, active from users where id = $1", 1).
		Scan(&id, &name, &age, &score, &active)
	if err != nil {
		t.Fatal(err)
	}
	if id != 1 || name != "ana" || age == nil || *age != 30 || score != 9.5 || !active {
		t.Errorf("row: %d %q %v %v %v", id, name, age, score, active)
	}

	// NULL comes back as a nil pointer.
	if err := conn.QueryRow(ctx, "select age from users where name = $1", "bia").Scan(&age); err != nil {
		t.Fatal(err)
	}
	if age != nil {
		t.Errorf("expected NULL, got %d", *age)
	}

	rows, err := conn.Query(ctx, "select name, age + 1 from users where age > $1 and active = $2", 20, true)
	if err != nil {
		t.Fatal(err)
	}
	type rec struct {
		Name string
		Age  int64
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[rec])
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[{ana 31} {caio 42}]" {
		t.Errorf("rows: %v", got)
	}

	if err := conn.QueryRow(ctx, "select id from users where id = $1", 99).Scan(&id); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("expected ErrNoRows, got %v", err)
	}
}

// Every way pgx can run a query maps to a different message sequence.
func TestQueryExecModes(t *testing.T) {
	url := startServer(t)
	seedConn, ctx := connect(t, url)
	seed(t, seedConn, ctx)

	modes := []pgx.QueryExecMode{
		pgx.QueryExecModeCacheStatement,
		pgx.QueryExecModeCacheDescribe,
		pgx.QueryExecModeDescribeExec,
		pgx.QueryExecModeExec,
		pgx.QueryExecModeSimpleProtocol,
	}
	for _, mode := range modes {
		t.Run(mode.String(), func(t *testing.T) {
			cfg, err := pgx.ParseConfig(url)
			if err != nil {
				t.Fatal(err)
			}
			cfg.DefaultQueryExecMode = mode
			conn, err := pgx.ConnectConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close(ctx)

			for i := 0; i < 3; i++ { // repeated, to hit the statement cache
				var name string
				var score float64
				err := conn.QueryRow(ctx, "select name, score from users where id = $1 and active = $2", int32(1), true).Scan(&name, &score)
				if err != nil {
					t.Fatal(err)
				}
				if name != "ana" || score != 9.5 {
					t.Errorf("got %q %v", name, score)
				}
			}
		})
	}
}

func TestPreparedStatement(t *testing.T) {
	conn, ctx := connect(t, startServer(t))
	seed(t, conn, ctx)

	sd, err := conn.Prepare(ctx, "by_name", "select id, age from users where name = $1")
	if err != nil {
		t.Fatal(err)
	}
	// 25 = text for the parameter; 23 and 20 = int4 and int8 for the columns.
	if len(sd.ParamOIDs) != 1 || sd.ParamOIDs[0] != 25 {
		t.Errorf("parameter OIDs: %v", sd.ParamOIDs)
	}
	if len(sd.Fields) != 2 || sd.Fields[0].DataTypeOID != 23 || sd.Fields[1].DataTypeOID != 20 {
		t.Errorf("fields: %+v", sd.Fields)
	}
	var id int32
	var age int64
	if err := conn.QueryRow(ctx, "by_name", "caio").Scan(&id, &age); err != nil {
		t.Fatal(err)
	}
	if id != 3 || age != 41 {
		t.Errorf("got %d %d", id, age)
	}
	if err := conn.Deallocate(ctx, "by_name"); err != nil {
		t.Fatal(err)
	}
}

func TestErrorsCarrySQLState(t *testing.T) {
	conn, ctx := connect(t, startServer(t))
	seed(t, conn, ctx)

	cases := []struct{ query, code string }{
		{"select * from missing", "42P01"},
		{"select nope from users", "42703"},
		{"selec 1", "42601"},
		{"insert into users values (1, 'dup', 1, 1, true)", "23505"},
		{"insert into users values (9, null, 1, 1, true)", "23502"},
		{"select 1 / 0", "22012"},
	}
	for _, tc := range cases {
		_, err := conn.Exec(ctx, tc.query)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Errorf("%s: expected a PgError, got %v", tc.query, err)
			continue
		}
		if pgErr.Code != tc.code || pgErr.Severity != "ERROR" {
			t.Errorf("%s: got %s %s (%s), want %s", tc.query, pgErr.Severity, pgErr.Code, pgErr.Message, tc.code)
		}
	}
	// The connection survives all of that.
	if err := conn.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestTransactions(t *testing.T) {
	conn, ctx := connect(t, startServer(t))
	seed(t, conn, ctx)
	count := func() (n int) {
		t.Helper()
		rows, err := conn.Query(ctx, "select id from users")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return n
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "delete from users where id > $1", 1); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 1 {
		t.Errorf("inside the transaction: %d rows", got)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 3 {
		t.Errorf("after rollback: %d rows", got)
	}

	tx, _ = conn.Begin(ctx)
	if _, err := tx.Exec(ctx, "update users set age = $1 where id = $2", int64(99), 2); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var age int64
	if err := conn.QueryRow(ctx, "select age from users where id = 2").Scan(&age); err != nil || age != 99 {
		t.Errorf("after commit: %d, %v", age, err)
	}

	// A failed statement aborts the transaction; pgx reports the failed
	// commit as a rollback.
	tx, _ = conn.Begin(ctx)
	if _, err := tx.Exec(ctx, "select 1 / 0"); err == nil {
		t.Fatal("expected an error")
	}
	if err := tx.Commit(ctx); !errors.Is(err, pgx.ErrTxCommitRollback) {
		t.Errorf("commit of a failed transaction: %v", err)
	}
}

// A batch is pipelined: many Bind/Execute pairs and a single Sync.
func TestBatch(t *testing.T) {
	conn, ctx := connect(t, startServer(t))
	seed(t, conn, ctx)

	batch := &pgx.Batch{}
	for i := 10; i < 15; i++ {
		batch.Queue("insert into users values ($1, $2, $3, $4, $5)", i, fmt.Sprint("user", i), int64(i), float64(i), i%2 == 0)
	}
	batch.Queue("select name from users where id = $1", 12)
	res := conn.SendBatch(ctx, batch)
	for i := 0; i < 5; i++ {
		if _, err := res.Exec(); err != nil {
			t.Fatal(err)
		}
	}
	var name string
	if err := res.QueryRow().Scan(&name); err != nil || name != "user12" {
		t.Errorf("got %q, %v", name, err)
	}
	if err := res.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestContextCancellation(t *testing.T) {
	conn, ctx := connect(t, startServer(t))

	// pgx sends the CancelRequest on a second connection, using the key
	// the server handed out in BackendKeyData.
	go func() {
		time.Sleep(200 * time.Millisecond)
		if err := conn.PgConn().CancelRequest(ctx); err != nil {
			t.Errorf("CancelRequest: %v", err)
		}
	}()
	start := time.Now()
	_, err := conn.Exec(ctx, "select pg_sleep(30)")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("expected query_canceled, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v", elapsed)
	}
	// The connection is still good after a cancelled statement.
	if err := conn.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

// database/sql on top of pgx: what most Go applications actually use.
func TestDatabaseSQL(t *testing.T) {
	db, err := sql.Open("pgx", startServer(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec("create table kv (k text primary key, v bigint)"); err != nil {
		t.Fatal(err)
	}
	stmt, err := db.Prepare("insert into kv values ($1, $2)")
	if err != nil {
		t.Fatal(err)
	}
	for i, k := range []string{"a", "b", "c"} {
		if _, err := stmt.Exec(k, i); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("delete from kv"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Query("select k, v from kv where v >= $1", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got string
	for rows.Next() {
		var k string
		var v sql.NullInt64
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		got += fmt.Sprintf("%s=%d ", k, v.Int64)
	}
	if got != "b=1 c=2 " {
		t.Errorf("got %q", got)
	}
}

// Joins, aggregates and subqueries through the extended protocol: result
// types must be described correctly before any row exists.
func TestAnalyticalQueries(t *testing.T) {
	conn, ctx := connect(t, startServer(t))
	seed(t, conn, ctx)
	if _, err := conn.Exec(ctx, `create table orders (id int primary key, user_id int, total float8);
		insert into orders values (1, 1, 10.5), (2, 1, 4.5), (3, 3, 100)`); err != nil {
		t.Fatal(err)
	}

	rows, err := conn.Query(ctx, `
		select u.name, count(o.id), coalesce(sum(o.total), 0), max(o.total)
		from users u left join orders o on o.user_id = u.id
		where u.id <= $1
		group by u.name
		having count(o.id) >= $2
		order by 2 desc, u.name
		limit $3`, 10, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	type rec struct {
		Name  string
		N     int64
		Total float64
		Max   *float64
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[rec])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Name != "ana" || got[0].N != 2 || got[0].Total != 15 ||
		got[1].Name != "caio" || *got[1].Max != 100 || got[2].Name != "bia" || got[2].Max != nil {
		t.Errorf("rows: %+v", got)
	}

	var name string
	err = conn.QueryRow(ctx, `select name from users u
		where exists (select 1 from orders o where o.user_id = u.id and o.total > $1)
		  and name like $2`, 50.0, "c%").Scan(&name)
	if err != nil || name != "caio" {
		t.Errorf("got %q, %v", name, err)
	}
}
