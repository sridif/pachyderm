package auth

import (
	"crypto/sha256"
	"fmt"
	"path"
	"time"

	etcd "github.com/coreos/etcd/clientv3"
	"github.com/google/go-github/github"
	"go.pedge.io/proto/rpclog"
	"golang.org/x/net/context"
	"golang.org/x/oauth2"

	"github.com/pachyderm/pachyderm/src/client"
	authclient "github.com/pachyderm/pachyderm/src/client/auth"
	"github.com/pachyderm/pachyderm/src/client/pkg/uuid"
	col "github.com/pachyderm/pachyderm/src/server/pkg/collection"
)

const (
	tokensPrefix = "/pach-tokens"
	aclsPrefix   = "/acls"

	defaultTokenTTLSecs = 24 * 60 * 60
	authnToken          = "authn-token"
)

type apiServer struct {
	protorpclog.Logger
	etcdClient  *etcd.Client
	tokenPrefix string
	// acls is a collection of repoName -> ACL mappings.
	acls col.Collection
}

// NewAuthServer returns an implementation of auth.APIServer.
func NewAuthServer(etcdAddress string, etcdPrefix string) (authclient.APIServer, error) {
	etcdClient, err := etcd.New(etcd.Config{
		Endpoints:   []string{etcdAddress},
		DialOptions: client.EtcdDialOptions(),
	})
	if err != nil {
		return nil, fmt.Errorf("error constructing etcdClient: %v", err)
	}

	return &apiServer{
		Logger:      protorpclog.NewLogger("auth.API"),
		etcdClient:  etcdClient,
		tokenPrefix: path.Join(etcdPrefix, tokensPrefix),
		acls: col.NewCollection(
			etcdClient,
			path.Join(etcdPrefix, aclsPrefix),
			nil,
			&authclient.ACL{},
			nil,
		),
	}, nil
}

func (a *apiServer) Authenticate(ctx context.Context, req *authclient.AuthenticateRequest) (resp *authclient.AuthenticateResponse, retErr error) {
	// We don't want to actually log the request/response since they contain
	// credentials.
	defer func(start time.Time) { a.Log(nil, nil, retErr, time.Since(start)) }(time.Now())

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{
			AccessToken: req.GithubToken,
		},
	)
	tc := oauth2.NewClient(ctx, ts)

	gclient := github.NewClient(tc)

	// Passing the empty string gets us the authenticated user
	user, _, err := gclient.Users.Get(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("error getting the authenticated user: %v", err)
	}

	username := user.GetName()
	pachToken := uuid.NewWithoutDashes()

	lease, err := a.etcdClient.Grant(ctx, defaultTokenTTLSecs)
	if err != nil {
		return nil, fmt.Errorf("error granting token TTL: %v", err)
	}

	_, err = a.etcdClient.Put(ctx, path.Join(a.tokenPrefix, hashToken(pachToken)), username, etcd.WithLease(lease.ID))
	if err != nil {
		return nil, fmt.Errorf("error storing the auth token: %v", err)
	}

	return &authclient.AuthenticateResponse{
		PachToken: pachToken,
	}, nil
}

func (a *apiServer) Authorize(ctx context.Context, req *authclient.AuthorizeRequest) (resp *authclient.AuthorizeResponse, retErr error) {
	func() { a.Log(req, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(req, resp, retErr, time.Since(start)) }(time.Now())

	user, err := a.getAuthorizedUser(ctx)
	if err != nil {
		return nil, err
	}

	var acl authclient.ACL
	if err := a.acls.ReadOnly(ctx).Get(req.Repo.Name, &acl); err != nil {
		if _, ok := err.(col.ErrNotFound); ok {
			return nil, fmt.Errorf("ACL not found for repo %v", req.Repo.Name)
		}
		return nil, fmt.Errorf("error getting ACL for repo %v: %v", req.Repo.Name, err)
	}

	if req.Scope == acl.Entries[user] {
		return &authclient.AuthorizeResponse{
			Authorized: true,
		}, nil
	}

	// If the user cannot authorize via ACL, we check if they are an admin.
	var _u authclient.User
	if err := a.acls.ReadOnly(ctx).Get(user, &_u); err != nil {
		if _, ok := err.(col.ErrNotFound); ok {
			return &authclient.AuthorizeResponse{
				Authorized: false,
			}, nil
		}
		return nil, fmt.Errorf("error checking if user %v is an admin: %v", user, err)
	}

	// Admins always authorize
	return &authclient.AuthorizeResponse{
		Authorized: true,
	}, nil
}

func (a *apiServer) SetScope(ctx context.Context, req *authclient.SetScopeRequest) (resp *authclient.SetScopeResponse, retErr error) {
	func() { a.Log(req, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(req, resp, retErr, time.Since(start)) }(time.Now())

	user, err := a.getAuthorizedUser(ctx)
	if err != nil {
		return nil, err
	}

	_, err = col.NewSTM(ctx, a.etcdClient, func(stm col.STM) error {
		acls := a.acls.ReadWrite(stm)

		var acl authclient.ACL
		if err := acls.Get(req.Repo.Name, &acl); err != nil {
			return fmt.Errorf("ACL not found for repo %v", req.Repo.Name)
		}

		if acl.Entries[user] != authclient.Scope_OWNER {
			return fmt.Errorf("user %v is not authorized to update ACL for repo %v", user, req.Repo.Name)
		}

		acl.Entries[req.Username] = req.Scope
		acls.Put(req.Repo.Name, &acl)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &authclient.SetScopeResponse{}, nil
}

func (a *apiServer) GetScope(ctx context.Context, req *authclient.GetScopeRequest) (resp *authclient.GetScopeResponse, retErr error) {
	func() { a.Log(req, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(req, resp, retErr, time.Since(start)) }(time.Now())
	return nil, fmt.Errorf("TODO")
}

func (a *apiServer) GetACL(ctx context.Context, req *authclient.GetACLRequest) (resp *authclient.GetACLResponse, retErr error) {
	func() { a.Log(req, nil, nil, 0) }()
	defer func(start time.Time) { a.Log(req, resp, retErr, time.Since(start)) }(time.Now())
	return nil, fmt.Errorf("TODO")
}

// hashToken converts a token to a cryptographic hash.
// We don't want to store tokens verbatim in the database, as then whoever
// that has access to the database has access to all tokens.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum)
}

func (a *apiServer) getAuthorizedUser(ctx context.Context) (string, error) {
	token := ctx.Value(authnToken)
	if token == nil {
		return "", fmt.Errorf("auth token not found in context")
	}

	tokenStr, ok := token.(string)
	if !ok {
		return "", fmt.Errorf("auth token found in context is malformed")
	}

	resp, err := a.etcdClient.Get(ctx, path.Join(a.tokenPrefix, hashToken(tokenStr)))
	if err != nil {
		return "", fmt.Errorf("auth token not found: %v", err)
	}

	return string(resp.Kvs[0].Value), nil
}
