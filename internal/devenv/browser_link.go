package devenv

import (
	"context"
	"errors"

	"github.com/GetDuranta/tools/internal/devaccess"
)

type BootstrapCloud struct {
	Issuer   devaccess.BootstrapIssuer
	Uploader SourceUploader
}

func (c BootstrapCloud) SourceUpload(ctx context.Context, identity Identity) (SourceUpload, error) {
	if c.Uploader == nil {
		return SourceUpload{}, ErrNotReady
	}
	return c.Uploader.Create(ctx, identity)
}

func (c BootstrapCloud) BrowserLink(ctx context.Context, identity Identity,
	env Environment) (BrowserLink, error) {
	if env.State != StateRunning || env.Host == "" || env.ACLVersion < 1 {
		return BrowserLink{}, ErrNotReady
	}
	link, err := c.Issuer.Issue(ctx, devaccess.IssueRequest{
		WorkspaceID: env.ID, Host: env.Host, Subject: identity.PrincipalID,
		Principals: []string{identity.PrincipalID}, ACLVersion: env.ACLVersion,
		ReturnPath: "/a/",
	})
	if err != nil {
		return BrowserLink{}, errors.Join(errors.New("issue browser link"), err)
	}
	return BrowserLink{URL: link.URL, ExpiresAt: &link.ExpiresAt}, nil
}
