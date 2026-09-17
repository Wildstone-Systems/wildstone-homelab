// auditpeek dumps the tail of the API audit table, read-only.
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	db, err := sql.Open("sqlite", "file:"+os.Args[1]+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT time, principal, acting_for, method, path, status, remote
	                         FROM audit
	                        WHERE principal = 'portal' OR path LIKE '%filedrop%' OR path LIKE '%assistant%'
	                        ORDER BY seq DESC LIMIT 30`)
	if err != nil {
		panic(err)
	}
	defer rows.Close()
	for rows.Next() {
		var at, principal, acting, method, path, remote string
		var status int
		if err := rows.Scan(&at, &principal, &acting, &method, &path, &status, &remote); err != nil {
			panic(err)
		}
		fmt.Printf("%s  %-8s acting=%-10s %-4s %-46s %d  from %s\n", at, principal, acting, method, path, status, remote)
	}
}
