// Package dbtest is for the tests that use the development database. go
// test runs the packages as parallel processes on that one database, and
// those tests delete rows and read back what they find: each takes Lock for
// its whole run, so that they run one at a time across all packages, and
// clears the rows it would read with Clear while it holds the lock.
package dbtest

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// lockKey is the advisory lock the database tests share; it is not the
// store's schema lock.
const lockKey int64 = 0x65746670_74657374

// The lock is held by one connection of this test binary, for as long as
// any test of it holds Lock.
var held struct {
	mu      sync.Mutex
	conn    *pgx.Conn
	holders int
}

// DSN returns the test database's connection string, or skips the test.
// The production port is refused: these tests write and delete rows.
func DSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv("ETFPAIR_TRADE_TEST_DSN")
	if dsn == "" {
		t.Skip("ETFPAIR_TRADE_TEST_DSN is not set: database test skipped (scripts/dev_db.sh start prints the value)")
	}
	pc, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("ETFPAIR_TRADE_TEST_DSN cannot be parsed")
	}
	if pc.Port == 5432 {
		t.Fatal("ETFPAIR_TRADE_TEST_DSN points at port 5432: database tests write and delete rows and run only against a development database")
	}
	return dsn
}

// Lock holds the database for the test until it ends, waiting while a test
// of another package holds it. It may be taken again by the same test or
// its subtests.
func Lock(t testing.TB) {
	t.Helper()
	dsn := DSN(t)
	held.mu.Lock()
	defer held.mu.Unlock()
	if held.holders == 0 {
		ctx := context.Background()
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal("dbtest: the test database cannot be reached")
		}
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
			conn.Close(ctx)
			t.Fatalf("dbtest: lock: %v", err)
		}
		held.conn = conn
	}
	held.holders++
	t.Cleanup(func() {
		held.mu.Lock()
		defer held.mu.Unlock()
		if held.holders--; held.holders == 0 {
			ctx := context.Background()
			held.conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey)
			held.conn.Close(ctx)
			held.conn = nil
		}
	})
}

// Clear deletes every row of schema trade except the runs (process_run),
// on the lock's connection: the test that holds the lock starts from
// nothing another test left.
func Clear(t testing.TB) {
	t.Helper()
	held.mu.Lock()
	defer held.mu.Unlock()
	if held.conn == nil {
		t.Fatal("dbtest: Clear without Lock")
	}
	ctx := context.Background()
	rows, err := held.conn.Query(ctx, "SELECT table_name FROM information_schema.tables WHERE table_schema = 'trade' AND table_type = 'BASE TABLE' AND table_name <> 'process_run'")
	if err != nil {
		t.Fatalf("dbtest: tables: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("dbtest: tables: %v", err)
	}
	for _, table := range tables {
		if _, err := held.conn.Exec(ctx, "DELETE FROM trade."+pgx.Identifier{table}.Sanitize()); err != nil {
			t.Fatalf("dbtest: clear %s: %v", table, err)
		}
	}
}
