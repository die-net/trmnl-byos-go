//go:build cgo

package db

import (
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3" // Registers itself as "sqlite3".
)

func driverName() string {
	return "sqlite3"
}

// writerDSN takes the write lock at BEGIN, so a transaction never fails
// upgrading from a read lock, and syncs only at checkpoints, which WAL makes
// safe against corruption.
func writerDSN(path string, busyTimeout time.Duration) string {
	return fmt.Sprintf("file:%s?mode=rwc&_txlock=immediate&_fk=1&_busy_timeout=%d&_journal_mode=WAL&_synchronous=NORMAL", path, int(busyTimeout/time.Millisecond))
}

func readerDSN(path string, busyTimeout time.Duration) string {
	return fmt.Sprintf("file:%s?mode=rw&_query_only=1&_busy_timeout=%d", path, int(busyTimeout/time.Millisecond))
}
