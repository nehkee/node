// Package sqldialect identifies the SQL backend behind a database/sql handle
// and adapts portable query text to it.
//
// Node stores write queries once using SQLite-style placeholders: '?' for
// sequential arguments and '?N' for numbered arguments. PostgreSQL receives the
// same text with placeholders rewritten to '$N'. Everything else that differs
// between backends is either written portably or isolated by the caller.
package sqldialect

import (
	"database/sql"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/stdlib"
)

type Dialect uint8

const (
	SQLite Dialect = iota
	PostgreSQL
)

func (d Dialect) String() string {
	if d == PostgreSQL {
		return "postgres"
	}
	return "sqlite"
}

// Of reports the dialect of db's driver. Unknown drivers are treated as SQLite,
// which preserves the behavior of every existing caller.
func Of(db *sql.DB) Dialect {
	if _, ok := db.Driver().(*stdlib.Driver); ok {
		return PostgreSQL
	}
	return SQLite
}

var rebound sync.Map // query text -> PostgreSQL text

// Rebind rewrites placeholders for d. SQLite text is returned unchanged.
func (d Dialect) Rebind(query string) string {
	if d != PostgreSQL || !strings.Contains(query, "?") {
		return query
	}
	if out, ok := rebound.Load(query); ok {
		return out.(string)
	}
	out := rebind(query)
	rebound.Store(query, out)
	return out
}

// rebind skips string literals, quoted identifiers and line comments, so a
// literal question mark is never treated as a placeholder.
func rebind(query string) string {
	var b strings.Builder
	b.Grow(len(query) + 16)
	next := 1
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'' || c == '"':
			end := i + 1
			for end < len(query) {
				if query[end] == c {
					if end+1 < len(query) && query[end+1] == c {
						end += 2
						continue
					}
					break
				}
				end++
			}
			b.WriteString(query[i:min(end+1, len(query))])
			i = end
		case c == '-' && i+1 < len(query) && query[i+1] == '-':
			end := strings.IndexByte(query[i:], '\n')
			if end < 0 {
				end = len(query) - i
			}
			b.WriteString(query[i : i+end])
			i += end - 1
		case c == '?':
			end := i + 1
			for end < len(query) && query[end] >= '0' && query[end] <= '9' {
				end++
			}
			b.WriteByte('$')
			if end > i+1 {
				b.WriteString(query[i+1 : end])
			} else {
				b.WriteString(strconv.Itoa(next))
				next++
			}
			i = end - 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
