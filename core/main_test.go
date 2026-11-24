package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"etf-pair-arbitrage/core/config"
	"etf-pair-arbitrage/core/store"
)

// fakeSecrets are the values the test secrets file holds; none is real.
var fakeSecrets = []string{"FAKE-KEY-111", "FAKE-SECRET-222", "FAKE-PASS-333", "FAKE-DIREXION-444"}

// loadLive loads the example configuration switched to live mode, so the
// exchange key is kept in memory, with a secrets file holding fakeSecrets and
// dsn.
func loadLive(t *testing.T, dsn string) *config.Config {
	t.Helper()
	example, err := os.ReadFile("config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(example), "live: false") {
		t.Fatal("config.example.yaml no longer contains live: false")
	}
	dir := t.TempDir()
	configPath, envPath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "env.json")
	env := fmt.Sprintf(`{"bitget":{"key":"FAKE-KEY-111","secret":"FAKE-SECRET-222","passphrase":"FAKE-PASS-333"},"direxion":{"site_api_key":"FAKE-DIREXION-444"},"store":{"dsn":%q}}`, dsn)
	if err := os.WriteFile(configPath, []byte(strings.Replace(string(example), "live: false", "live: true", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath, envPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// assertNoSecrets fails when text carries anything of the secrets file.
func assertNoSecrets(t *testing.T, what, text, dsn string) {
	t.Helper()
	for _, secret := range append(fakeSecrets, dsn) {
		if strings.Contains(text, secret) {
			t.Errorf("%s carries %q from the secrets file:\n%s", what, secret, text)
		}
	}
	if !strings.Contains(text, "d_in") || !strings.Contains(text, "SOXLUSDT") {
		t.Errorf("%s lacks the configuration:\n%s", what, text)
	}
}

// The process_run row main builds carries the configuration and nothing from
// the secrets file.
func TestProcessRunRowHoldsNoSecrets(t *testing.T) {
	const dsn = "postgres://etfpair_trade:FAKE-PW-555@127.0.0.1:5441/etfpair"
	cfg := loadLive(t, dsn)
	row := processRunRow(cfg, "host-1", time.UnixMilli(1_795_000_000_000))
	assertNoSecrets(t, "the process_run row", string(row.Config), dsn)
	assertNoSecrets(t, "the -check-config line", configCheckLine("config.yaml", ".env", cfg), dsn)
	if row.StartedMs != 1_795_000_000_000 || row.Host != "host-1" || row.StoppedMs != 0 {
		t.Errorf("row = %+v", row)
	}
}

// The same, as stored: the row is written to the test database the way run
// writes it and its config column is read back.
func TestStoredProcessRunConfigHoldsNoSecrets(t *testing.T) {
	dsn := testDSN(t)
	cfg := loadLive(t, dsn)
	runID := store.RunID(fmt.Sprintf("test-%d-process-run", time.Now().UnixNano()))
	st, err := store.New(cfg.Store, runID, cfg.Env.Store.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	go st.Run(context.Background())
	if err := st.WriteSync(processRunRow(cfg, "host-1", time.Now())); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var stored string
	var live bool
	if err := conn.QueryRow(ctx, "SELECT config::text, live FROM trade.process_run WHERE run_id = $1", string(runID)).Scan(&stored, &live); err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, "the stored process_run.config", stored, dsn)
	if !live {
		t.Error("process_run.live must follow exec.live, which is true here")
	}
}

// testDSN returns the test database's connection string, or skips. These
// tests write rows: like the store's tests they run only against a
// development database, never the production port.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("ETFPAIR_TRADE_TEST_DSN")
	if dsn == "" {
		t.Skip("ETFPAIR_TRADE_TEST_DSN is not set: database test skipped (scripts/dev_db.sh start prints the value)")
	}
	if pc, err := pgx.ParseConfig(dsn); err != nil {
		t.Fatal("ETFPAIR_TRADE_TEST_DSN cannot be parsed")
	} else if pc.Port == 5432 {
		t.Fatal("ETFPAIR_TRADE_TEST_DSN points at port 5432: this test writes and runs only against a development database")
	}
	return dsn
}

// schemaLockKey is the advisory lock the store takes while it applies the
// schema (store.go). The test below holds it to keep a starting process
// waiting; it checks that the process really waits, so a changed key fails
// the test instead of emptying it.
const schemaLockKey int64 = 0x65746670_74726164

// A stop signal that arrives while the process is still starting (here: it
// waits for the schema lock, before any manager exists) does not kill it. The
// process finishes its start-up, leaves through the execution manager with
// exit code 0 and writes the stop time of its run.
func TestStopSignalDuringStartupIsHonoured(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	dir := t.TempDir()

	// The real binary, with the example configuration in paper mode. Its
	// connections carry an application name, so its lock wait can be told
	// from other tests'.
	bin := buildCore(t)
	example, err := os.ReadFile("config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	appName := fmt.Sprintf("core-under-test-%d", time.Now().UnixNano())
	childDSN := dsn + " application_name=" + appName
	if strings.Contains(dsn, "://") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		childDSN = dsn + sep + "application_name=" + appName
	}
	configPath, envPath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "env.json")
	if err := os.WriteFile(configPath, example, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte(fmt.Sprintf(`{"store":{"dsn":%q}}`, childDSN)), 0o600); err != nil {
		t.Fatal(err)
	}

	// Hold the schema lock, start the process and wait until it waits.
	locker, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close(ctx)
	if _, err := locker.Exec(ctx, "SELECT pg_advisory_lock($1)", schemaLockKey); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	cmd := osexec.Command(bin, "-config", configPath, "-env", envPath)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, &output, &output
	began := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer cmd.Process.Kill()
	const waiting = "SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a USING (pid) WHERE l.locktype = 'advisory' AND NOT l.granted AND a.application_name = $1"
	for deadline := time.Now().Add(8 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var n int
		if err := locker.QueryRow(ctx, waiting, appName).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the process never waited for the schema lock:\n%s", output.String())
		}
	}

	// The stop signal arrives now; the process must still be there after it.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		t.Fatalf("the process left while its start-up was blocked: %v\n%s", err, output.String())
	case <-time.After(300 * time.Millisecond):
	}

	// Start-up may go on: the process finishes it and then stops in order.
	if _, err := locker.Exec(ctx, "SELECT pg_advisory_unlock($1)", schemaLockKey); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("the process did not exit with code 0: %v\n%s", err, output.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("the process did not stop after its start-up:\n%s", output.String())
	}
	host, _ := os.Hostname()
	var stopped int
	if err := locker.QueryRow(ctx, "SELECT count(*) FROM trade.process_run WHERE host = $1 AND NOT live AND started_ms >= $2 AND stopped_ms >= started_ms",
		host, began.UnixMilli()).Scan(&stopped); err != nil {
		t.Fatal(err)
	}
	if stopped != 1 {
		t.Errorf("%d process_run rows of this start with a stop time, want 1:\n%s", stopped, output.String())
	}
}

// coreBuild is the process's binary, built once for the tests that run it and
// removed by TestMain.
var coreBuild struct {
	once sync.Once
	dir  string
	err  error
}

func buildCore(t *testing.T) string {
	t.Helper()
	coreBuild.once.Do(func() {
		if coreBuild.dir, coreBuild.err = os.MkdirTemp("", "core-under-test"); coreBuild.err != nil {
			return
		}
		if out, err := osexec.Command("go", "build", "-o", filepath.Join(coreBuild.dir, "core"), ".").CombinedOutput(); err != nil {
			coreBuild.err = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if coreBuild.err != nil {
		t.Fatal(coreBuild.err)
	}
	return filepath.Join(coreBuild.dir, "core")
}

func TestMain(m *testing.M) {
	code := m.Run()
	if coreBuild.dir != "" {
		os.RemoveAll(coreBuild.dir)
	}
	os.Exit(code)
}

// The -env flag as the binary takes it, checked with -check-config (which
// opens no database). Paper: the default .env of the working directory is
// read, and a bitget section holding anything does not matter. Live: the
// default is refused, a named file with the full key is accepted.
func TestSecretsFileFlag(t *testing.T) {
	bin := buildCore(t)
	example, err := os.ReadFile("config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir() // the child's working directory, with its own .env
	write := func(name, text string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	const dsn = "postgres://etfpair_trade:FAKE-PW-555@127.0.0.1:5441/etfpair"
	paper := write("paper.yaml", string(example))
	live := write("live.yaml", strings.Replace(string(example), "live: false", "live: true", 1))
	write(".env", `{"bitget":"garbage FAKE-KEY-111","store":{"dsn":"`+dsn+`"}}`)
	full := write("service.json", `{"bitget":{"key":"FAKE-KEY-111","secret":"FAKE-SECRET-222","passphrase":"FAKE-PASS-333"},"store":{"dsn":"`+dsn+`"}}`)

	cases := []struct {
		name   string
		args   []string
		wantOK bool
		want   string
	}{
		{"paper, default .env with a garbage bitget section", []string{"-check-config", "-config", paper}, true, "live=false"},
		{"paper, named file", []string{"-check-config", "-config", paper, "-env", full}, true, "live=false"},
		{"live without -env", []string{"-check-config", "-config", live}, false, "-env must be given"},
		{"live with -env and the full key", []string{"-check-config", "-config", live, "-env", full}, true, "live=true"},
		{"live with -env naming a file without a usable key", []string{"-check-config", "-config", live, "-env", filepath.Join(dir, ".env")}, false, "bitget must be an object"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := osexec.Command(bin, c.args...)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if (err == nil) != c.wantOK || !strings.Contains(string(out), c.want) {
				t.Errorf("exit error %v, output %q; want success=%t and %q in the output", err, out, c.wantOK, c.want)
			}
			for _, secret := range append(fakeSecrets, "FAKE-PW-555") {
				if strings.Contains(string(out), secret) {
					t.Errorf("the output carries the secret %s: %s", secret, out)
				}
			}
		})
	}
}
