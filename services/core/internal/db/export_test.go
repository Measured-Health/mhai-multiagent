package db

// SetTestPasswordSQL makes EnsureLogin rewrite its password statement with f
// (nil restores it), so a test can make that statement fail on the server.
func SetTestPasswordSQL(f func(string) string) { testPasswordSQL = f }
