package upkeep

import (
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/internal/dbtemplate"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// migrated is the schema, built once for this whole test binary rather
// than migrated into every database these tests open (#231). Each test
// still gets its own file, at the path it chose.
var migrated = dbtemplate.New(func(path string) error {
	db, err := runtimedb.Open(path)
	if err != nil {
		return err
	}
	return db.Close()
})
