package runtimedb

import (
	"os"
	"strings"
	"testing"
)

func TestZZCols(t *testing.T) {
	want := os.Getenv("COLS")
	if want == "" {
		t.Skip("")
	}
	db := open(t)
	for _, tbl := range strings.Split(want, ",") {
		rows, err := db.sql.Query(`SELECT name, type, "notnull", dflt_value FROM pragma_table_info(?)`, tbl)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for rows.Next() {
			var n, ty string
			var nn int
			var dflt *string
			rows.Scan(&n, &ty, &nn, &dflt)
			s := n + " " + ty
			if nn == 1 {
				s += " NOT NULL"
			}
			if dflt != nil {
				s += " DEFAULT " + *dflt
			}
			out = append(out, s)
		}
		rows.Close()
		t.Logf("%s:\n  %s", tbl, strings.Join(out, "\n  "))
	}
}
