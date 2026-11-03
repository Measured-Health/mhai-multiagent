package platform

// SetLeaderSession replaces the leader's session settings for a test and
// returns the function that puts them back. Not safe while a sampler runs.
func SetLeaderSession(stmt string) (restore func()) {
	old := leaderSession
	leaderSession = stmt
	return func() { leaderSession = old }
}
