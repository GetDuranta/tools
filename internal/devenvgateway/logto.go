package devenvgateway

import "context"

type LogtoPreviewApp struct {
	EnvironmentID string
	Name          string
	Origin        string
}

// LogtoPreviewApps is implemented by the control plane. Every environment gets
// exact redirect, post-logout, and CORS origins; wildcard redirects are forbidden.
type LogtoPreviewApps interface {
	Ensure(context.Context, LogtoPreviewApp) (clientID string, err error)
	Delete(context.Context, string) error
	Reconcile(context.Context, []LogtoPreviewApp) error
}
