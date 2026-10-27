package collector

import (
	"database/sql"
	"errors"
	"strings"

	"modernc.org/sqlite"
)

func init() {
	driver := &sqlite.Driver{}
	driver.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, _ string) error {
		register, ok := conn.(sqlite.AuthorizerRegisterer)
		if !ok {
			return errors.New("SQLite read-only authorizer unavailable")
		}
		return register.RegisterAuthorizer(func(action sqlite.AuthorizerActionCode, arg1, arg2, _, _ string) sqlite.AuthorizerReturnCode {
			switch action {
			case sqlite.AuthRead, sqlite.AuthSelect, sqlite.AuthRecursive, sqlite.AuthTransaction, sqlite.AuthSavepoint:
				return sqlite.AuthorizerOK
			case sqlite.AuthFunction:
				switch strings.ToLower(arg2) {
				case "load_extension", "writefile", "readfile":
					return sqlite.AuthorizerDeny
				}
				return sqlite.AuthorizerOK
			case sqlite.AuthPragma:
				switch strings.ToLower(arg1) {
				case "table_info", "table_xinfo", "index_info", "index_xinfo", "index_list", "foreign_key_list":
					return sqlite.AuthorizerOK
				case "compile_options", "database_list", "user_version", "application_id", "journal_mode", "synchronous", "page_size", "page_count", "freelist_count", "data_version", "query_only":
					if arg2 == "" {
						return sqlite.AuthorizerOK
					}
				}
			}
			return sqlite.AuthorizerDeny
		})
	})
	sql.Register("subyard_telemetry_readonly", driver)
}
