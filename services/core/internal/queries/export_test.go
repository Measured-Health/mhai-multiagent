package queries

// SetAfterCommit runs f between the submit's commit and the publish (a test
// hook for the deactivation race); the returned func restores the old one.
func SetAfterCommit(f func()) (restore func()) {
	old := afterCommit
	afterCommit = f
	return func() { afterCommit = old }
}
