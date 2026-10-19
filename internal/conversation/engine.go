package conversation

import "context"

// RunScheduled runs one turn nobody is watching; see turn.Engine.RunScheduled.
func (s *Service) RunScheduled(ctx context.Context, principalID, turnKey, message string) (ScheduledReply, error) {
	return s.engine.RunScheduled(ctx, principalID, turnKey, message)
}

// RunCheck runs one check of an agent watch; see turn.Engine.RunCheck.
func (s *Service) RunCheck(ctx context.Context, principalID, message string) (ScheduledReply, error) {
	return s.engine.RunCheck(ctx, principalID, message)
}

// RunTask runs one stretch of a background task; see turn.Engine.RunTask.
func (s *Service) RunTask(ctx context.Context, run TaskRun) (TaskReply, error) {
	return s.engine.RunTask(ctx, run)
}

// RewriteAlert rewrites a message the application wrote; see turn.Engine.RewriteAlert.
func (s *Service) RewriteAlert(ctx context.Context, principalID, language, text string) (string, error) {
	return s.engine.RewriteAlert(ctx, principalID, language, text)
}

// NameTab chooses a short name for a tab; see turn.Engine.NameTab.
func (s *Service) NameTab(ctx context.Context, principalID, message, reply string, icons []string) (TabName, error) {
	return s.engine.NameTab(ctx, principalID, message, reply, icons)
}
