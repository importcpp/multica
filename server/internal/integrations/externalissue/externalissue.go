// Package externalissue is the provider abstraction for pulling issues from an
// external tracker (GitHub today, GitLab next) into Multica. It is deliberately
// separate from the inbound `vcs` package: `vcs` mirrors pull requests and CI
// status from webhooks, whereas this package makes OUTBOUND, read-oriented API
// calls to enumerate and fetch issues. Forgejo/Gitea do not all expose the same
// issue API, so issue capability is NOT bolted onto vcs.Provider — a provider
// opts in by implementing this contract instead.
//
// The contract is intentionally provider-neutral. It never exposes GitHub's
// owner/repo pair, its installation IDs, integer page numbers, or GitLab's
// project id/iid to the core: repositories are identified by a stable
// (InstanceKey, ExternalID) with a mutable FullPath for display, pagination is
// an opaque Cursor the provider mints and consumes, and errors collapse onto a
// small normalized taxonomy. A new provider is a new file that implements
// Provider and calls register in init — the core, schema, and API DTOs do not
// move.
package externalissue

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Kind identifies a provider. The string value is persisted on the sync
// source/link rows and used as the registry key.
type Kind string

const (
	KindGitHub Kind = "github"
	// KindGitLab Kind = "gitlab" // added by the GitLab adapter (PR7).
)

// Valid reports whether k is a registered provider kind.
func (k Kind) Valid() bool {
	_, ok := registry[k]
	return ok
}

// State is the normalized open/closed state of a remote issue. It is surfaced
// as read-only source information; the P0 sync never derives Multica workflow
// status from it.
type State string

const (
	StateOpen   State = "open"
	StateClosed State = "closed"
)

// Capabilities describes what a provider can do so the sync engine can gate
// features (e.g. only offer "keep in sync" when webhooks are supported) without
// hard-coding provider identity.
type Capabilities struct {
	// FilterByLabel reports whether ListIssues honors IssueFilter.Labels.
	FilterByLabel bool
	// FilterByUpdatedAfter reports whether ListIssues honors
	// IssueFilter.UpdatedAfter (GitHub `since`, GitLab `updated_after`).
	FilterByUpdatedAfter bool
	// Webhooks reports whether the provider can push low-latency updates. When
	// false, continuous sync must rely on scheduled reconcile alone.
	Webhooks bool
}

// Credentials is the resolved authentication for one API call. A CredentialRef
// (an internal UUID, not modeled here) is resolved to these values by the
// caller BEFORE reaching the provider, so no provider ever mints or caches a
// token itself. An empty Token means unauthenticated access, which only works
// for public repositories.
type Credentials struct {
	// Token is a resolved bearer token (a GitHub installation token, a GitLab
	// PAT, etc.). Empty => unauthenticated.
	Token string
	// InstanceBaseURL overrides the provider's default API base (used by
	// self-hosted GitLab). Empty => the provider default.
	InstanceBaseURL string
}

// RepositoryRef is the user-supplied handle used once, at configuration time,
// to resolve a stable Repository. FullPath is "owner/name" for GitHub or a
// GitLab group/subgroup/project path.
type RepositoryRef struct {
	FullPath string
}

// Repository is the resolved, stable identity of a remote repository. ExternalID
// survives rename/transfer; FullPath is display-only and may change. InstanceKey
// is the normalized, validated platform origin ("github.com", or a validated
// GitLab instance URL) — never a raw user-supplied string.
type Repository struct {
	InstanceKey string
	ExternalID  string
	FullPath    string
}

// IssueRef points at a single remote issue. ExternalID is the stable id used as
// the dedup key; Number is the human-facing "#N"/iid a provider needs to
// address the issue over its REST API.
type IssueRef struct {
	ExternalID string
	Number     int64
}

// IssueFilter narrows a ListIssues enumeration. Zero values mean "no filter"
// except State, which defaults to StateOpen (the P0 default is open-only).
type IssueFilter struct {
	State        string // "open" | "closed" | "all"; "" => "open"
	Labels       []string
	UpdatedAfter time.Time // zero => no lower bound
}

// Issue is the normalized remote issue. Timestamps are RFC3339 strings ("" when
// absent) so the sync engine can store them verbatim and compare monotonically.
type Issue struct {
	ExternalID      string
	Number          int64
	Title           string
	Body            string
	State           State
	HTMLURL         string
	AuthorLogin     string
	Labels          []string
	CreatedAt       string
	RemoteUpdatedAt string
	ClosedAt        string
}

// Cursor is an opaque, provider-minted pagination token. The core never parses
// it; it round-trips whatever the provider returned. An empty NextCursor from
// IssuePage means enumeration is complete.
type Cursor string

// IssuePage is one page of ListIssues output.
type IssuePage struct {
	Issues     []Issue
	NextCursor Cursor
	// IncompleteBucket is set when a full page was entirely within ONE
	// updated_at second: more issues may share that second than a page holds, and
	// GitHub's second-granular `since` cannot enumerate the overflow without an
	// offset walk that a concurrent delete would corrupt. The sync engine advances
	// past the second (guaranteeing progress + delete-safety) and marks the run
	// PARTIAL rather than reporting a possibly-incomplete scan as succeeded.
	IncompleteBucket bool
}

// ErrorKind is the normalized failure taxonomy every provider maps onto, so the
// sync engine can decide retry/backoff/surfacing without provider-specific code.
type ErrorKind int

const (
	// ErrPermanent is the zero value: a non-retryable failure with no more
	// specific classification.
	ErrPermanent ErrorKind = iota
	ErrUnauthorized
	ErrForbidden
	ErrNotFound
	ErrRateLimited
	ErrTransient
)

func (k ErrorKind) String() string {
	switch k {
	case ErrUnauthorized:
		return "unauthorized"
	case ErrForbidden:
		return "forbidden"
	case ErrNotFound:
		return "not_found"
	case ErrRateLimited:
		return "rate_limited"
	case ErrTransient:
		return "transient"
	default:
		return "permanent"
	}
}

// Error is the normalized provider error. RetryAt is set only for ErrRateLimited
// (derived from Retry-After / X-RateLimit-Reset); it tells the worker when to
// resume rather than hammering the API.
type Error struct {
	Kind    ErrorKind
	RetryAt time.Time
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("externalissue: %s: %v", e.Kind, e.Err)
	}
	return "externalissue: " + e.Kind.String()
}

func (e *Error) Unwrap() error { return e.Err }

// Is lets callers match on the kind with errors.Is(err, &Error{Kind: ...}).
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Kind == e.Kind
}

// wrapf builds a normalized *Error.
func wrapf(kind ErrorKind, err error, format string, args ...any) *Error {
	return &Error{Kind: kind, Err: fmt.Errorf(format+": %w", append(args, errOrEmpty(err))...)}
}

func errOrEmpty(err error) error {
	if err == nil {
		return errors.New("")
	}
	return err
}

// Provider is the per-provider adapter. Implementations are stateless and cheap
// to construct; the registry holds one instance per kind.
type Provider interface {
	Kind() Kind
	Capabilities() Capabilities
	// ResolveRepository turns a user-supplied ref into a stable Repository,
	// verifying the repo exists and is visible to creds.
	ResolveRepository(ctx context.Context, creds Credentials, ref RepositoryRef) (Repository, error)
	// ListIssues returns one page. Pass an empty Cursor to start; feed the
	// returned NextCursor back in to advance. Providers MUST drop pull requests
	// and MUST validate any URL embedded in a cursor stays on the approved
	// origin before following it.
	ListIssues(ctx context.Context, creds Credentials, repo Repository, filter IssueFilter, cursor Cursor) (IssuePage, error)
	// GetIssue fetches the current authoritative state of one issue. The
	// webhook path uses this to avoid trusting a possibly out-of-order payload.
	GetIssue(ctx context.Context, creds Credentials, repo Repository, ref IssueRef) (Issue, error)
}

// registry maps a Kind to its Provider. Populated by package init in the adapter
// files (github.go, and later gitlab.go).
var registry = map[Kind]Provider{}

func register(p Provider) { registry[p.Kind()] = p }

// For returns the provider for kind, or (nil, false) if unknown.
func For(kind string) (Provider, bool) {
	p, ok := registry[Kind(kind)]
	return p, ok
}

// SetGitHubAPIBaseForTest points the GitHub adapter at base (e.g. an httptest
// server) and returns a restore func. Test-only seam for callers in other
// packages; production always uses the real api.github.com default.
func SetGitHubAPIBaseForTest(base string) (restore func()) {
	prev := apiBase
	apiBase = base
	return func() { apiBase = prev }
}

// CredentialRef identifies the stored credential a run should authenticate with,
// without leaking provider-specific shape into the worker. The worker passes the
// run's provider + this ref to a CredentialResolver, which returns ready-to-use
// Credentials. For GitHub the ID is the github_installation UUID; for GitLab it
// will be the vcs_connection UUID. The worker never constructs a token itself.
type CredentialRef struct {
	// Provider is the externalissue provider kind ("github", later "gitlab").
	Provider string
	// WorkspaceID scopes the lookup so a resolver can enforce tenancy.
	WorkspaceID string
	// ID is the internal credential UUID (installation / connection).
	ID string
}

// ErrCredentialUnavailable means the credential no longer exists or the account
// must be re-authorized (e.g. installation removed, token revoked). The worker
// maps it to a needs_reauth run outcome rather than a transient retry.
var ErrCredentialUnavailable = errors.New("externalissue: credential unavailable")

// CredentialResolver turns a CredentialRef into live Credentials for a call.
// One implementation per provider keeps auth wiring out of the worker: adding
// GitLab is a new resolver, not a worker edit. Implementations must return
// ErrCredentialUnavailable when the credential is gone/needs reauth, and may
// return a *RateLimitError-style transient error otherwise (the worker requeues
// on any non-ErrCredentialUnavailable error).
type CredentialResolver interface {
	Resolve(ctx context.Context, ref CredentialRef) (Credentials, error)
}
