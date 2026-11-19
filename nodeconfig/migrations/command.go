package migrations

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"

	"github.com/livepeer/node/internal/sqldialect"

	"github.com/j0sh/boa/pkg/boa"
	"github.com/spf13/cobra"
)

// Command creates migration subcommands for files containing migrations/*.sql.
// When open returns a PostgreSQL handle, the catalog is postgres/migrations/*.sql.
// Config parameters used by subcommands must be persistent.
// open must not run migrations. Only up requests creation; Command closes the database.
func Command[P any](config boa.Cmd[P], path *string, files fs.FS, open func(context.Context, string, bool) (*sql.DB, error)) *cobra.Command {
	run := func(_ *boa.NoParams, cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		db, err := open(ctx, *path, cmd.Name() == "up")
		if err != nil {
			return err
		}
		defer db.Close()
		catalog, err := fs.Sub(files, Dir(db))
		if err != nil {
			return err
		}
		switch cmd.Name() {
		case "up":
			err = Up(ctx, db, catalog)
		case "down":
			err = Down(ctx, db, catalog)
		}
		if err != nil {
			return err
		}
		status, err := List(ctx, db, catalog)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}
	config.Use, config.Short, config.Args = "migrate", "Manage embedded SQL migrations", cobra.NoArgs
	config.SubCmds = boa.SubCmds(
		boa.Cmd[boa.NoParams]{Use: "up", Short: "Apply pending migrations", Args: cobra.NoArgs, RunFuncE: run},
		boa.Cmd[boa.NoParams]{Use: "down", Short: "Roll back the latest migration; destroys data", Args: cobra.NoArgs, RunFuncE: run},
		boa.Cmd[boa.NoParams]{Use: "status", Short: "Inspect migration status", Args: cobra.NoArgs, RunFuncE: run},
	)
	return config.ToCobra()
}

// Dir names the catalog directory for db's backend within an embedded tree.
func Dir(db *sql.DB) string {
	if sqldialect.Of(db) == sqldialect.PostgreSQL {
		return "postgres/migrations"
	}
	return "migrations"
}
