package handler

import (
	"context"
	"errors"

	"github.com/multica-ai/multica/server/internal/integrations/externalissue"
	"github.com/multica-ai/multica/server/internal/integrations/githubapi"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// credentialResolvers maps an externalissue provider kind to the resolver that
// turns a stored credential ref into live Credentials. The worker looks up by
// the run's provider and never constructs a token itself, so adding GitLab is a
// new resolver entry here — not a worker edit.
func (h *Handler) credentialResolver(provider string) (externalissue.CredentialResolver, bool) {
	switch provider {
	case "github":
		return &githubCredentialResolver{queries: h.Queries, app: h.GHApp}, true
	// case "gitlab": return &gitlabCredentialResolver{...}, true  // PR7
	default:
		return nil, false
	}
}

// githubCredentialResolver resolves a github_installation UUID to a cached,
// least-privilege issues:read installation token via the shared githubapi
// client. It enforces workspace tenancy on the installation row.
type githubCredentialResolver struct {
	queries *db.Queries
	app     *githubapi.Client
}

func (r *githubCredentialResolver) Resolve(ctx context.Context, ref externalissue.CredentialRef) (externalissue.Credentials, error) {
	credID, err := util.ParseUUID(ref.ID)
	if err != nil {
		return externalissue.Credentials{}, externalissue.ErrCredentialUnavailable
	}
	wsID, err := util.ParseUUID(ref.WorkspaceID)
	if err != nil {
		return externalissue.Credentials{}, externalissue.ErrCredentialUnavailable
	}
	installation, err := r.queries.GetGitHubInstallationByID(ctx, credID)
	if err != nil || installation.WorkspaceID != wsID {
		return externalissue.Credentials{}, externalissue.ErrCredentialUnavailable
	}
	token, _, err := mintGitHubIssuesReadToken(ctx, r.app, installation.InstallationID)
	if err != nil {
		if errors.Is(err, errGitHubIssuesPermission) {
			return externalissue.Credentials{}, externalissue.ErrCredentialUnavailable
		}
		// Transient (rate limit / network): surface so the worker requeues
		// instead of marking needs_reauth.
		return externalissue.Credentials{}, err
	}
	return externalissue.Credentials{Token: token}, nil
}
