package runtimedb

import (
	"crypto/sha256"
	"fmt"
	"os"
)

// A stable deployment id permits crash recovery, but is not a process id.
// Hold an OS lock for this database/holder until Close or process exit. A
// promoted, different holder has a separate lock; the database epoch fences
// its predecessor. Lock files persist so unlink/recreate cannot split a lock.
func (db *DB) acquireCoordinatorProcessLock(holder string) (*os.File, error) {
	if db.path == ":memory:" {
		return nil, nil
	}
	name := fmt.Sprintf("%s.coordinator-%x.lock", db.path, sha256.Sum256([]byte(holder)))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("runtimedb: coordinator process lock: %w", err)
	}
	if err := lockCoordinatorFile(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w: deployment %q process lock: %v", ErrCoordinatorHeld, holder, err)
	}
	return f, nil
}
