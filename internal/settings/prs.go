package settings

// PullRequests configures how FrIDE follows the pull (merge) requests of
// task branches (CONFIG.md, pullRequests). Both are on unless turned off:
// without gh or glab, or for a branch with no pull request, there is nothing
// to follow anyway, and nothing changes.
type PullRequests struct {
	// Watch looks each pushed task branch's pull request up with gh or glab,
	// and shows its state on the board and to the orchestrator. Default true.
	Watch *bool `json:"watch,omitempty"`
	// Feedback types failed CI, review comments and merge conflicts into the
	// agent of a task the orchestrator supervises or an inbox thread
	// follows. Default true; nothing is typed while Watch is off.
	Feedback *bool `json:"feedback,omitempty"`
}

// PRWatch reports whether pull requests are looked up at all.
func (s *Settings) PRWatch() bool {
	return s == nil || s.PullRequests == nil || s.PullRequests.Watch == nil || *s.PullRequests.Watch
}

// PRFeedback reports whether what a pull request says is typed into its
// task's agent.
func (s *Settings) PRFeedback() bool {
	if !s.PRWatch() {
		return false
	}
	return s == nil || s.PullRequests == nil || s.PullRequests.Feedback == nil || *s.PullRequests.Feedback
}
