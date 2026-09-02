package taskctx

import "context"

type contextKey struct{}
type askUserKey struct{}

// AskUserFunc is a callback the agent can call to pause and ask the user a question.
// It blocks until the user answers or the context is cancelled.
type AskUserFunc func(ctx context.Context, question string) (string, error)

// Credentials holds per-request client credentials.
// Server never stores these — they live only for the duration of a task.
type Credentials struct {
	// Output
	OutputType string // pr | files | print
	OutputDir  string // for output_type=files
	RepoURL    string // for output_type=pr

	// GitHub
	GitHubToken string

	// Atlassian / Jira
	AtlassianToken  string
	AtlassianDomain string // e.g. mycompany.atlassian.net
	AtlassianEmail  string

	// TaskID identifies the logical task this execution belongs to, stable
	// across NATS redelivery/retries of the same submission. CreatePRSkill
	// uses it to derive a deterministic branch name so a redelivered task
	// reuses, rather than duplicates, any branch/PR a prior attempt
	// already created.
	TaskID string
}

// WithCredentials stores credentials in the context.
func WithCredentials(ctx context.Context, creds Credentials) context.Context {
	return context.WithValue(ctx, contextKey{}, creds)
}

// FromContext retrieves credentials from the context.
func FromContext(ctx context.Context) (Credentials, bool) {
	creds, ok := ctx.Value(contextKey{}).(Credentials)
	return creds, ok
}

// WithAskUser stores an AskUserFunc in the context.
func WithAskUser(ctx context.Context, fn AskUserFunc) context.Context {
	return context.WithValue(ctx, askUserKey{}, fn)
}

// AskUser calls the registered AskUserFunc, or returns an error if none is set.
func AskUser(ctx context.Context, question string) (string, error) {
	fn, ok := ctx.Value(askUserKey{}).(AskUserFunc)
	if !ok || fn == nil {
		return "", nil // no-op outside server context
	}
	return fn(ctx, question)
}
