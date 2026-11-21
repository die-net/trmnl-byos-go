//go:build !cgo

package db

import (
	"fmt"
	"time"

	_ "modernc.org/sqlite" // Registers itself as "sqlite".
)

func driverName() string {
	return "sqlite"
}

// writerDSN opens in the given SQLite mode: "rwc" to create the file, or "rw"
// to require it. It takes the write lock at BEGIN, so a transaction never
// fails upgrading from a read lock, and syncs only at checkpoints, which WAL
// makes safe against corruption.
func writerDSN(path, mode string, busyTimeout time.Duration) string {
	return fmt.Sprintf(
		"file:%s?mode=%s&_txlock=immediate&_pragma=foreign_keys(1)&_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)",
		path,
		mode,
		int(busyTimeout/time.Millisecond),
	)
}

func readerDSN(path string, busyTimeout time.Duration) string {
	return fmt.Sprintf(
		"file:%s?mode=rw&_query_only=1&_pragma=busy_timeout(%d)",
		path,
		int(busyTimeout/time.Millisecond),
	)
}
