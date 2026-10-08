//go:build pgx

package main

// Production DB support is built with -tags pgx. Isolating the driver lets the
// deterministic engine be tested without network access or a local database.
import _ "github.com/jackc/pgx/v5/stdlib"
