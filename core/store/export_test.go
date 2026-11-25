package store

// The database helpers of this package's tests, for the external test
// package of this directory (restart_test.go). Tests against the database
// clear the tables, so they all live in this one test binary and never run
// beside each other.
var (
	TestDatabase = testConfig  // the test database's pool configuration, or skip
	ClearTables  = clearTables // deletes every row of every table but process_run
)
