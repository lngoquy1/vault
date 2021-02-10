package cache

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/errwrap"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/vault/api"
	cachememdb "github.com/hashicorp/vault/command/agent/cache/cachememdb"
	"github.com/hashicorp/vault/command/agent/cache/persistcache"
	"github.com/hashicorp/vault/helper/namespace"
	nshelper "github.com/hashicorp/vault/helper/namespace"
	"github.com/hashicorp/vault/sdk/helper/base62"
	"github.com/hashicorp/vault/sdk/helper/consts"
	"github.com/hashicorp/vault/sdk/helper/cryptoutil"
	"github.com/hashicorp/vault/sdk/helper/jsonutil"
	"github.com/hashicorp/vault/sdk/helper/locksutil"
	"github.com/hashicorp/vault/sdk/logical"
	gocache "github.com/patrickmn/go-cache"
	"github.com/ryboe/q"
	"go.uber.org/atomic"
)

const (
	vaultPathTokenCreate         = "/v1/auth/token/create"
	vaultPathTokenRevoke         = "/v1/auth/token/revoke"
	vaultPathTokenRevokeSelf     = "/v1/auth/token/revoke-self"
	vaultPathTokenRevokeAccessor = "/v1/auth/token/revoke-accessor"
	vaultPathTokenRevokeOrphan   = "/v1/auth/token/revoke-orphan"
	vaultPathTokenLookup         = "/v1/auth/token/lookup"
	vaultPathTokenLookupSelf     = "/v1/auth/token/lookup-self"
	vaultPathLeaseRevoke         = "/v1/sys/leases/revoke"
	vaultPathLeaseRevokeForce    = "/v1/sys/leases/revoke-force"
	vaultPathLeaseRevokePrefix   = "/v1/sys/leases/revoke-prefix"
)

var (
	contextIndexID  = contextIndex{}
	errInvalidType  = errors.New("invalid type provided")
	revocationPaths = []string{
		strings.TrimPrefix(vaultPathTokenRevoke, "/v1"),
		strings.TrimPrefix(vaultPathTokenRevokeSelf, "/v1"),
		strings.TrimPrefix(vaultPathTokenRevokeAccessor, "/v1"),
		strings.TrimPrefix(vaultPathTokenRevokeOrphan, "/v1"),
		strings.TrimPrefix(vaultPathLeaseRevoke, "/v1"),
		strings.TrimPrefix(vaultPathLeaseRevokeForce, "/v1"),
		strings.TrimPrefix(vaultPathLeaseRevokePrefix, "/v1"),
	}
)

type contextIndex struct{}

type cacheClearRequest struct {
	Type      string `json:"type"`
	Value     string `json:"value"`
	Namespace string `json:"namespace"`
}

// LeaseCache is an implementation of Proxier that handles
// the caching of responses. It passes the incoming request
// to an underlying Proxier implementation.
type LeaseCache struct {
	client      *api.Client
	proxier     Proxier
	logger      hclog.Logger
	db          *cachememdb.CacheMemDB
	baseCtxInfo *cachememdb.ContextInfo
	l           *sync.RWMutex

	// idLocks is used during cache lookup to ensure that identical requests made
	// in parallel won't trigger multiple renewal goroutines.
	idLocks []*locksutil.LockEntry

	// inflightCache keeps track of inflight requests
	inflightCache *gocache.Cache

	ps persistcache.Storage
}

// LeaseCacheConfig is the configuration for initializing a new
// Lease.
type LeaseCacheConfig struct {
	Client      *api.Client
	BaseContext context.Context
	Proxier     Proxier
	Logger      hclog.Logger
	Storage     persistcache.Storage
}

type inflightRequest struct {
	// ch is closed by the request that ends up processing the set of
	// parallel request
	ch chan struct{}

	// remaining is the number of remaining inflight request that needs to
	// be processed before this object can be cleaned up
	remaining atomic.Uint64
}

func newInflightRequest() *inflightRequest {
	return &inflightRequest{
		ch: make(chan struct{}),
	}
}

// NewLeaseCache creates a new instance of a LeaseCache.
func NewLeaseCache(conf *LeaseCacheConfig) (*LeaseCache, error) {
	if conf == nil {
		return nil, errors.New("nil configuration provided")
	}

	if conf.Proxier == nil || conf.Logger == nil {
		return nil, fmt.Errorf("missing configuration required params: %v", conf)
	}

	if conf.Client == nil {
		return nil, fmt.Errorf("nil API client")
	}

	db, err := cachememdb.New()
	if err != nil {
		return nil, err
	}

	// Create a base context for the lease cache layer
	baseCtxInfo := cachememdb.NewContextInfo(conf.BaseContext)

	return &LeaseCache{
		client:        conf.Client,
		proxier:       conf.Proxier,
		logger:        conf.Logger,
		db:            db,
		baseCtxInfo:   baseCtxInfo,
		l:             &sync.RWMutex{},
		idLocks:       locksutil.CreateLocks(),
		inflightCache: gocache.New(gocache.NoExpiration, gocache.NoExpiration),
		ps:            conf.Storage,
	}, nil
}

// SetPersistentStorage is a setter for the persistent storage field in
// LeaseCache
func (c *LeaseCache) SetPersistentStorage(storageIn persistcache.Storage) {
	c.ps = storageIn
}

// checkCacheForRequest checks the cache for a particular request based on its
// computed ID. It returns a non-nil *SendResponse  if an entry is found.
func (c *LeaseCache) checkCacheForRequest(id string) (*SendResponse, error) {
	index, err := c.db.Get(cachememdb.IndexNameID, id)
	if err != nil {
		return nil, err
	}

	if index == nil {
		return nil, nil
	}

	// Cached request is found, deserialize the response
	reader := bufio.NewReader(bytes.NewReader(index.Response))
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		c.logger.Error("failed to deserialize response", "error", err)
		return nil, err
	}

	sendResp, err := NewSendResponse(&api.Response{Response: resp}, index.Response)
	if err != nil {
		c.logger.Error("failed to create new send response", "error", err)
		return nil, err
	}
	sendResp.CacheMeta.Hit = true

	respTime, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		c.logger.Error("failed to parse cached response date", "error", err)
		return nil, err
	}
	sendResp.CacheMeta.Age = time.Now().Sub(respTime)

	return sendResp, nil
}

// Send performs a cache lookup on the incoming request. If it's a cache hit,
// it will return the cached response, otherwise it will delegate to the
// underlying Proxier and cache the received response.
func (c *LeaseCache) Send(ctx context.Context, req *SendRequest) (*SendResponse, error) {
	q.Q("got request token", req.Token) // DEBUG
	// Compute the index ID
	id, err := computeIndexID(req)
	if err != nil {
		c.logger.Error("failed to compute cache key", "error", err)
		return nil, err
	}

	// Check the inflight cache to see if there are other inflight requests
	// of the same kind, based on the computed ID. If so, we increment a counter

	var inflight *inflightRequest

	defer func() {
		// Cleanup on the cache if there are no remaining inflight requests.
		// This is the last step, so we defer the call first
		if inflight != nil && inflight.remaining.Load() == 0 {
			c.inflightCache.Delete(id)
		}
	}()

	idLock := locksutil.LockForKey(c.idLocks, id)

	// Briefly grab an ID-based lock in here to emulate a load-or-store behavior
	// and prevent concurrent cacheable requests from being proxied twice if
	// they both miss the cache due to it being clean when peeking the cache
	// entry.
	idLock.Lock()
	inflightRaw, found := c.inflightCache.Get(id)
	if found {
		idLock.Unlock()
		inflight = inflightRaw.(*inflightRequest)
		inflight.remaining.Inc()
		defer inflight.remaining.Dec()

		// If found it means that there's an inflight request being processed.
		// We wait until that's finished before proceeding further.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-inflight.ch:
		}
	} else {
		inflight = newInflightRequest()
		inflight.remaining.Inc()
		defer inflight.remaining.Dec()

		c.inflightCache.Set(id, inflight, gocache.NoExpiration)
		idLock.Unlock()

		// Signal that the processing request is done
		defer close(inflight.ch)
	}

	// Check if the response for this request is already in the cache
	cachedResp, err := c.checkCacheForRequest(id)
	if err != nil {
		return nil, err
	}
	if cachedResp != nil {
		c.logger.Debug("returning cached response", "path", req.Request.URL.Path)
		return cachedResp, nil
	}

	c.logger.Debug("forwarding request", "method", req.Request.Method, "path", req.Request.URL.Path)

	// Pass the request down and get a response
	resp, err := c.proxier.Send(ctx, req)
	if err != nil {
		return resp, err
	}

	// If this is a non-2xx or if the returned response does not contain JSON payload,
	// we skip caching
	if resp.Response.StatusCode >= 300 || resp.Response.Header.Get("Content-Type") != "application/json" {
		return resp, err
	}

	// Get the namespace from the request header
	namespace := req.Request.Header.Get(consts.NamespaceHeaderName)
	// We need to populate an empty value since go-memdb will skip over indexes
	// that contain empty values.
	if namespace == "" {
		namespace = "root/"
	}

	// Build the index to cache based on the response received
	index := &cachememdb.Index{
		ID:          id,
		Namespace:   namespace,
		RequestPath: req.Request.URL.Path,
	}
	q.Q("api parsesecret", string(resp.ResponseBody)) // DEBUG
	secret, err := api.ParseSecret(bytes.NewReader(resp.ResponseBody))
	if err != nil {
		c.logger.Error("failed to parse response as secret", "error", err)
		return nil, err
	}

	isRevocation, err := c.handleRevocationRequest(ctx, req, resp)
	if err != nil {
		c.logger.Error("failed to process the response", "error", err)
		return nil, err
	}

	// If this is a revocation request, do not go through cache logic.
	if isRevocation {
		return resp, nil
	}

	// Fast path for responses with no secrets
	if secret == nil {
		c.logger.Debug("pass-through response; no secret in response", "method", req.Request.Method, "path", req.Request.URL.Path)
		return resp, nil
	}

	// Short-circuit if the secret is not renewable
	tokenRenewable, err := secret.TokenIsRenewable()
	if err != nil {
		c.logger.Error("failed to parse renewable param", "error", err)
		return nil, err
	}
	if !secret.Renewable && !tokenRenewable {
		c.logger.Debug("pass-through response; secret not renewable", "method", req.Request.Method, "path", req.Request.URL.Path)
		return resp, nil
	}

	var renewCtxInfo *cachememdb.ContextInfo
	var indexType persistcache.IndexType
	switch {
	case secret.LeaseID != "":
		c.logger.Trace("processing lease response", "method", req.Request.Method, "path", req.Request.URL.Path)
		entry, err := c.db.Get(cachememdb.IndexNameToken, req.Token)
		if err != nil {
			return nil, err
		}
		// If the lease belongs to a token that is not managed by the agent,
		// return the response without caching it.
		if entry == nil {
			c.logger.Debug("pass-through lease response; token not managed by agent", "method", req.Request.Method, "path", req.Request.URL.Path)
			return resp, nil
		}

		// Derive a context for renewal using the token's context
		renewCtxInfo = cachememdb.NewContextInfo(entry.RenewCtxInfo.Ctx)

		index.Lease = secret.LeaseID
		index.LeaseToken = req.Token

		indexType = persistcache.SecretLeaseType

	case secret.Auth != nil:
		c.logger.Trace("processing auth response", "method", req.Request.Method, "path", req.Request.URL.Path)

		// Check if this token creation request resulted in a non-orphan token, and if so
		// correctly set the parentCtx to the request's token context.
		var parentCtx context.Context
		if !secret.Auth.Orphan {
			entry, err := c.db.Get(cachememdb.IndexNameToken, req.Token)
			if err != nil {
				return nil, err
			}
			// If parent token is not managed by the agent, child shouldn't be
			// either.
			if entry == nil {
				c.logger.Debug("pass-through auth response; parent token not managed by agent", "method", req.Request.Method, "path", req.Request.URL.Path)
				return resp, nil
			}

			c.logger.Debug("setting parent context", "method", req.Request.Method, "path", req.Request.URL.Path)
			parentCtx = entry.RenewCtxInfo.Ctx

			index.TokenParent = req.Token
		}

		renewCtxInfo = c.createCtxInfo(parentCtx)
		index.Token = secret.Auth.ClientToken
		index.TokenAccessor = secret.Auth.Accessor

		indexType = persistcache.AuthLeaseType

	default:
		// We shouldn't be hitting this, but will err on the side of caution and
		// simply proxy.
		c.logger.Debug("pass-through response; secret without lease and token", "method", req.Request.Method, "path", req.Request.URL.Path)
		return resp, nil
	}

	// Serialize the response to store it in the cached index
	var respBytes bytes.Buffer
	err = resp.Response.Write(&respBytes)
	if err != nil {
		c.logger.Error("failed to serialize response", "error", err)
		return nil, err
	}

	// Reset the response body for upper layers to read
	if resp.Response.Body != nil {
		resp.Response.Body.Close()
	}
	resp.Response.Body = ioutil.NopCloser(bytes.NewReader(resp.ResponseBody))

	// Set the index's Response
	index.Response = respBytes.Bytes()

	// Store the index ID in the lifetimewatcher context
	renewCtx := context.WithValue(renewCtxInfo.Ctx, contextIndexID, index.ID)

	// Store the lifetime watcher context in the index
	index.RenewCtxInfo = &cachememdb.ContextInfo{
		Ctx:        renewCtx,
		CancelFunc: renewCtxInfo.CancelFunc,
		DoneCh:     renewCtxInfo.DoneCh,
	}

	// Add extra information necessary for restoring from persisted cache
	index.RequestMethod = req.Request.Method
	index.RequestToken = req.Token
	index.RequestHeader = req.Request.Header

	// Store the index in the cache
	c.logger.Debug("storing response into the cache", "method", req.Request.Method, "path", req.Request.URL.Path)
	q.Q("storing in cache", index) // DEBUG
	err = c.Set(index, indexType)
	if err != nil {
		c.logger.Error("failed to cache the proxied response", "error", err)
		return nil, err
	}

	// Start renewing the secret in the response
	go c.startRenewing(renewCtx, index, req, secret)

	return resp, nil
}

func (c *LeaseCache) createCtxInfo(ctx context.Context) *cachememdb.ContextInfo {
	if ctx == nil {
		c.l.RLock()
		ctx = c.baseCtxInfo.Ctx
		c.l.RUnlock()
	}
	return cachememdb.NewContextInfo(ctx)
}

func (c *LeaseCache) startRenewing(ctx context.Context, index *cachememdb.Index, req *SendRequest, secret *api.Secret) {
	defer func() {
		id := ctx.Value(contextIndexID).(string)
		c.logger.Debug("evicting index from cache", "id", id, "method", req.Request.Method, "path", req.Request.URL.Path)
		err := c.Evict(id)
		c.logger.Trace("evicted index from cache", "id", id, "method", req.Request.Method, "path", req.Request.URL.Path)
		if err != nil {
			c.logger.Error("failed to evict index", "id", id, "error", err)
			return
		}
		c.logger.Trace("finished evict defer", "id", id, "method", req.Request.Method, "path", req.Request.URL.Path)
	}()

	client, err := c.client.Clone()
	if err != nil {
		c.logger.Error("failed to create API client in the lifetime watcher", "error", err)
		return
	}
	client.SetToken(req.Token)
	client.SetHeaders(req.Request.Header)

	watcher, err := client.NewLifetimeWatcher(&api.LifetimeWatcherInput{
		Secret: secret,
	})
	if err != nil {
		c.logger.Error("failed to create secret lifetime watcher", "error", err)
		return
	}

	c.logger.Debug("initiating renewal", "method", req.Request.Method, "path", req.Request.URL.Path)
	go watcher.Start()
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			// This is the case which captures context cancellations from token
			// and leases. Since all the contexts are derived from the agent's
			// context, this will also cover the shutdown scenario.
			c.logger.Debug("context cancelled; stopping lifetime watcher", "path", req.Request.URL.Path)
			return
		case err := <-watcher.DoneCh():
			// This case covers renewal completion and renewal errors
			if err != nil {
				c.logger.Error("failed to renew secret", "error", err)
				return
			}
			c.logger.Debug("renewal halted; evicting from cache", "path", req.Request.URL.Path)
			return
		case <-watcher.RenewCh():
			c.logger.Debug("secret renewed", "path", req.Request.URL.Path)
		case <-index.RenewCtxInfo.DoneCh:
			// This case indicates the renewal process to shutdown and evict
			// the cache entry. This is triggered when a specific secret
			// renewal needs to be killed without affecting any of the derived
			// context renewals.
			c.logger.Debug("done channel closed")
			return
		}
	}
}

// computeIndexID results in a value that uniquely identifies a request
// received by the agent. It does so by SHA256 hashing the serialized request
// object containing the request path, query parameters and body parameters.
func computeIndexID(req *SendRequest) (string, error) {
	var b bytes.Buffer

	// Serialize the request
	if err := req.Request.Write(&b); err != nil {
		return "", fmt.Errorf("failed to serialize request: %v", err)
	}

	// Reset the request body after it has been closed by Write
	req.Request.Body = ioutil.NopCloser(bytes.NewReader(req.RequestBody))

	// Append req.Token into the byte slice. This is needed since auto-auth'ed
	// requests sets the token directly into SendRequest.Token
	b.Write([]byte(req.Token))

	return hex.EncodeToString(cryptoutil.Blake2b256Hash(string(b.Bytes()))), nil
}

// HandleCacheClear returns a handlerFunc that can perform cache clearing operations.
func (c *LeaseCache) HandleCacheClear(ctx context.Context) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only handle POST/PUT requests
		switch r.Method {
		case http.MethodPost:
		case http.MethodPut:
		default:
			return
		}

		req := new(cacheClearRequest)
		if err := jsonutil.DecodeJSONFromReader(r.Body, req); err != nil {
			if err == io.EOF {
				err = errors.New("empty JSON provided")
			}
			logical.RespondError(w, http.StatusBadRequest, errwrap.Wrapf("failed to parse JSON input: {{err}}", err))
			return
		}

		c.logger.Debug("received cache-clear request", "type", req.Type, "namespace", req.Namespace, "value", req.Value)

		in, err := parseCacheClearInput(req)
		if err != nil {
			c.logger.Error("unable to parse clear input", "error", err)
			logical.RespondError(w, http.StatusBadRequest, errwrap.Wrapf("failed to parse clear input: {{err}}", err))
			return
		}

		if err := c.handleCacheClear(ctx, in); err != nil {
			// Default to 500 on error, unless the user provided an invalid type,
			// which would then be a 400.
			httpStatus := http.StatusInternalServerError
			if err == errInvalidType {
				httpStatus = http.StatusBadRequest
			}
			logical.RespondError(w, httpStatus, errwrap.Wrapf("failed to clear cache: {{err}}", err))
			return
		}

		return
	})
}

func (c *LeaseCache) handleCacheClear(ctx context.Context, in *cacheClearInput) error {
	if in == nil {
		return errors.New("no value(s) provided to clear corresponding cache entries")
	}

	switch in.Type {
	case "request_path":
		// For this particular case, we need to ensure that there are 2 provided
		// indexers for the proper lookup.
		if in.RequestPath == "" {
			return errors.New("request path not provided")
		}

		// The first value provided for this case will be the namespace, but if it's
		// an empty value we need to overwrite it with "root/" to ensure proper
		// cache lookup.
		if in.Namespace == "" {
			in.Namespace = "root/"
		}

		// Find all the cached entries which has the given request path and
		// cancel the contexts of all the respective lifetime watchers
		indexes, err := c.db.GetByPrefix(cachememdb.IndexNameRequestPath, in.Namespace, in.RequestPath)
		if err != nil {
			return err
		}
		for _, index := range indexes {
			index.RenewCtxInfo.CancelFunc()
		}

	case "token":
		if in.Token == "" {
			return errors.New("token not provided")
		}

		// Get the context for the given token and cancel its context
		index, err := c.db.Get(cachememdb.IndexNameToken, in.Token)
		if err != nil {
			return err
		}
		if index == nil {
			return nil
		}

		c.logger.Debug("canceling context of index attached to token")

		index.RenewCtxInfo.CancelFunc()

	case "token_accessor":
		if in.TokenAccessor == "" {
			return errors.New("token accessor not provided")
		}

		// Get the cached index and cancel the corresponding lifetime watcher
		// context
		index, err := c.db.Get(cachememdb.IndexNameTokenAccessor, in.TokenAccessor)
		if err != nil {
			return err
		}
		if index == nil {
			return nil
		}

		c.logger.Debug("canceling context of index attached to accessor")

		index.RenewCtxInfo.CancelFunc()

	case "lease":
		if in.Lease == "" {
			return errors.New("lease not provided")
		}

		// Get the cached index and cancel the corresponding lifetime watcher
		// context
		index, err := c.db.Get(cachememdb.IndexNameLease, in.Lease)
		if err != nil {
			return err
		}
		if index == nil {
			return nil
		}

		c.logger.Debug("canceling context of index attached to accessor")

		index.RenewCtxInfo.CancelFunc()

	case "all":
		// Cancel the base context which triggers all the goroutines to
		// stop and evict entries from cache.
		c.logger.Debug("canceling base context")
		c.l.Lock()
		c.baseCtxInfo.CancelFunc()
		// Reset the base context
		baseCtx, baseCancel := context.WithCancel(ctx)
		c.baseCtxInfo = &cachememdb.ContextInfo{
			Ctx:        baseCtx,
			CancelFunc: baseCancel,
		}
		c.l.Unlock()

		// Reset the memdb instance
		if err := c.db.Flush(); err != nil {
			return err
		}
		// TODO(tvoran): clear persistence here too? though everything should be
		// evicted when the context is cancelled. though might be a good
		// maintenance function.

	default:
		return errInvalidType
	}

	c.logger.Debug("successfully cleared matching cache entries")

	return nil
}

// handleRevocationRequest checks whether the originating request is a
// revocation request, and if so perform applicable cache cleanups.
// Returns true is this is a revocation request.
func (c *LeaseCache) handleRevocationRequest(ctx context.Context, req *SendRequest, resp *SendResponse) (bool, error) {
	// Lease and token revocations return 204's on success. Fast-path if that's
	// not the case.
	if resp.Response.StatusCode != http.StatusNoContent {
		return false, nil
	}

	_, path := deriveNamespaceAndRevocationPath(req)

	switch {
	case path == vaultPathTokenRevoke:
		// Get the token from the request body
		jsonBody := map[string]interface{}{}
		if err := json.Unmarshal(req.RequestBody, &jsonBody); err != nil {
			return false, err
		}
		tokenRaw, ok := jsonBody["token"]
		if !ok {
			return false, fmt.Errorf("failed to get token from request body")
		}
		token, ok := tokenRaw.(string)
		if !ok {
			return false, fmt.Errorf("expected token in the request body to be string")
		}

		// Clear the cache entry associated with the token and all the other
		// entries belonging to the leases derived from this token.
		in := &cacheClearInput{
			Type:  "token",
			Token: token,
		}
		if err := c.handleCacheClear(ctx, in); err != nil {
			return false, err
		}

	case path == vaultPathTokenRevokeSelf:
		// Clear the cache entry associated with the token and all the other
		// entries belonging to the leases derived from this token.
		in := &cacheClearInput{
			Type:  "token",
			Token: req.Token,
		}
		if err := c.handleCacheClear(ctx, in); err != nil {
			return false, err
		}

	case path == vaultPathTokenRevokeAccessor:
		jsonBody := map[string]interface{}{}
		if err := json.Unmarshal(req.RequestBody, &jsonBody); err != nil {
			return false, err
		}
		accessorRaw, ok := jsonBody["accessor"]
		if !ok {
			return false, fmt.Errorf("failed to get accessor from request body")
		}
		accessor, ok := accessorRaw.(string)
		if !ok {
			return false, fmt.Errorf("expected accessor in the request body to be string")
		}

		in := &cacheClearInput{
			Type:          "token_accessor",
			TokenAccessor: accessor,
		}
		if err := c.handleCacheClear(ctx, in); err != nil {
			return false, err
		}

	case path == vaultPathTokenRevokeOrphan:
		jsonBody := map[string]interface{}{}
		if err := json.Unmarshal(req.RequestBody, &jsonBody); err != nil {
			return false, err
		}
		tokenRaw, ok := jsonBody["token"]
		if !ok {
			return false, fmt.Errorf("failed to get token from request body")
		}
		token, ok := tokenRaw.(string)
		if !ok {
			return false, fmt.Errorf("expected token in the request body to be string")
		}

		// Kill the lifetime watchers of all the leases attached to the revoked
		// token
		indexes, err := c.db.GetByPrefix(cachememdb.IndexNameLeaseToken, token)
		if err != nil {
			return false, err
		}
		for _, index := range indexes {
			index.RenewCtxInfo.CancelFunc()
		}

		// Kill the lifetime watchers of the revoked token
		index, err := c.db.Get(cachememdb.IndexNameToken, token)
		if err != nil {
			return false, err
		}
		if index == nil {
			return true, nil
		}

		// Indicate the lifetime watcher goroutine for this index to return.
		// This will not affect the child tokens because the context is not
		// getting cancelled.
		close(index.RenewCtxInfo.DoneCh)

		// Clear the parent references of the revoked token in the entries
		// belonging to the child tokens of the revoked token.
		indexes, err = c.db.GetByPrefix(cachememdb.IndexNameTokenParent, token)
		if err != nil {
			return false, err
		}
		for _, index := range indexes {
			index.TokenParent = ""
			err = c.db.Set(index)
			if err != nil {
				c.logger.Error("failed to persist index", "error", err)
				return false, err
			}
		}

	case path == vaultPathLeaseRevoke:
		// TODO: Should lease present in the URL itself be considered here?
		// Get the lease from the request body
		jsonBody := map[string]interface{}{}
		if err := json.Unmarshal(req.RequestBody, &jsonBody); err != nil {
			return false, err
		}
		leaseIDRaw, ok := jsonBody["lease_id"]
		if !ok {
			return false, fmt.Errorf("failed to get lease_id from request body")
		}
		leaseID, ok := leaseIDRaw.(string)
		if !ok {
			return false, fmt.Errorf("expected lease_id the request body to be string")
		}
		in := &cacheClearInput{
			Type:  "lease",
			Lease: leaseID,
		}
		if err := c.handleCacheClear(ctx, in); err != nil {
			return false, err
		}

	case strings.HasPrefix(path, vaultPathLeaseRevokeForce):
		// Trim the URL path to get the request path prefix
		prefix := strings.TrimPrefix(path, vaultPathLeaseRevokeForce)
		// Get all the cache indexes that use the request path containing the
		// prefix and cancel the lifetime watcher context of each.
		indexes, err := c.db.GetByPrefix(cachememdb.IndexNameLease, prefix)
		if err != nil {
			return false, err
		}

		_, tokenNSID := namespace.SplitIDFromString(req.Token)
		for _, index := range indexes {
			_, leaseNSID := namespace.SplitIDFromString(index.Lease)
			// Only evict leases that match the token's namespace
			if tokenNSID == leaseNSID {
				index.RenewCtxInfo.CancelFunc()
			}
		}

	case strings.HasPrefix(path, vaultPathLeaseRevokePrefix):
		// Trim the URL path to get the request path prefix
		prefix := strings.TrimPrefix(path, vaultPathLeaseRevokePrefix)
		// Get all the cache indexes that use the request path containing the
		// prefix and cancel the lifetime watcher context of each.
		indexes, err := c.db.GetByPrefix(cachememdb.IndexNameLease, prefix)
		if err != nil {
			return false, err
		}

		_, tokenNSID := namespace.SplitIDFromString(req.Token)
		for _, index := range indexes {
			_, leaseNSID := namespace.SplitIDFromString(index.Lease)
			// Only evict leases that match the token's namespace
			if tokenNSID == leaseNSID {
				index.RenewCtxInfo.CancelFunc()
			}
		}

	default:
		return false, nil
	}

	c.logger.Debug("triggered caching eviction from revocation request")

	return true, nil
}

// Set stores the index in the cachememdb, and also stores it in the persistent
// cache (if enabled)
func (c *LeaseCache) Set(index *cachememdb.Index, indexType persistcache.IndexType) error {
	if err := c.db.Set(index); err != nil {
		return err
	}

	if c.ps != nil {
		b, err := index.Serialize()
		if err != nil {
			return err
		}

		// TODO(tvoran): encrypt here before setting in storage

		if err := c.ps.Set(index.ID, b, indexType); err != nil {
			return err
		}
		c.logger.Debug("set entry in persistent storage", "type", indexType, "path", index.RequestPath, "id", index.ID)
	}

	return nil
}

// Evict removes an Index from the cachememdb, and also removes it from the
// persistent cache (if enabled)
func (c *LeaseCache) Evict(id string) error {
	c.logger.Trace("going to evict index", "id", id)
	if err := c.db.Evict(cachememdb.IndexNameID, id); err != nil {
		return err
	}

	if c.ps != nil {
		if err := c.ps.Delete(id); err != nil {
			return err
		}
		c.logger.Debug("deleted item from persistent storage", "id", id)
	}

	return nil
}

// Restore loads the cachememdb from the persistent storage passed in. Loads
// tokens first, since restoring a lease's renewal context and watcher requires
// looking up the token in the cachememdb.
func (c *LeaseCache) Restore(storage persistcache.Storage) error {
	// Process tokens first
	tokens, err := storage.GetByType(persistcache.TokenType)
	if err != nil {
		return err
	}
	if err := c.restoreTokens(tokens); err != nil {
		return err
	}

	// Then process auth leases
	authLeases, err := storage.GetByType(persistcache.AuthLeaseType)
	if err != nil {
		return err
	}
	if err := c.restoreLeases(authLeases); err != nil {
		return err
	}

	// Then process secret leases
	secretLeases, err := storage.GetByType(persistcache.SecretLeaseType)
	if err != nil {
		return err
	}
	if err := c.restoreLeases(secretLeases); err != nil {
		return err
	}

	return nil
}

func (c *LeaseCache) restoreTokens(tokens [][]byte) error {
	for _, token := range tokens {
		newIndex, err := cachememdb.Deserialize(token)
		if err != nil {
			return err
		}
		newIndex.RenewCtxInfo = c.createCtxInfo(nil)
		if err := c.db.Set(newIndex); err != nil {
			return err
		}
		c.logger.Trace("restored token", "id", newIndex.ID)
	}
	return nil
}

func (c *LeaseCache) restoreLeases(leases [][]byte) error {
	for _, lease := range leases {
		newIndex, err := cachememdb.Deserialize(lease)
		if err != nil {
			return err
		}
		if err := c.ReCreateLeaseRenewCtx(newIndex); err != nil {
			return err
		}
		if err := c.db.Set(newIndex); err != nil {
			return err
		}
		c.logger.Trace("restored lease", "id", newIndex.ID, "path", newIndex.RequestPath)
	}
	return nil
}

// ReCreateLeaseRenewCtx re-creates a RenewCtx for an index object and starts
// the watcher go routine
func (c *LeaseCache) ReCreateLeaseRenewCtx(index *cachememdb.Index) error {
	if index.Response == nil {
		return fmt.Errorf("cached response was nil for %s", index.ID)
	}

	// Parse the secret to determine which type it is
	reader := bufio.NewReader(bytes.NewReader(index.Response))
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		c.logger.Error("failed to deserialize response", "error", err)
		return err
	}
	secret, err := api.ParseSecret(resp.Body)
	if err != nil {
		// q.Q(string(sendResp.ResponseBody)) // DEBUG
		c.logger.Error("failed to parse response as secret", "error", err)
		return err
	}

	var renewCtxInfo *cachememdb.ContextInfo
	switch {
	case secret.LeaseID != "":
		q.Q("got a secret with a lease", secret) // DEBUG
		// makeRenewCtxLease()
		entry, err := c.db.Get(cachememdb.IndexNameToken, index.RequestToken)
		if err != nil {
			return err
		}

		if entry == nil {
			// TODO(tvoran): log a warning here instead of failing
			return fmt.Errorf("could not find parent Token %s for req path %s", index.RequestToken, index.RequestPath)
		}

		// Derive a context for renewal using the token's context
		renewCtxInfo = cachememdb.NewContextInfo(entry.RenewCtxInfo.Ctx)

	case secret.Auth != nil:
		q.Q("got an auth secret", secret) // DEBUG
		var parentCtx context.Context
		if !secret.Auth.Orphan {
			entry, err := c.db.Get(cachememdb.IndexNameToken, index.RequestToken)
			if err != nil {
				return err
			}
			// If parent token is not managed by the agent, child shouldn't be
			// either.
			if entry == nil {
				// c.logger.Debug("pass-through auth response; parent token not managed by agent", "method", req.Request.Method, "path", req.Request.URL.Path)
				// TODO(tvoran): log a warning here instead of failing
				return fmt.Errorf("could not find parent Token %s for req path %s", index.RequestToken, index.RequestPath)
			}

			c.logger.Debug("setting parent context", "method", index.RequestMethod, "path", index.RequestPath)
			parentCtx = entry.RenewCtxInfo.Ctx
		}
		renewCtxInfo = c.createCtxInfo(parentCtx)
	default:
		q.Q("i dunno what this is", secret) // DEBUG
		return fmt.Errorf("unknown cached index item: %s", index.ID)
	}

	renewCtx := context.WithValue(renewCtxInfo.Ctx, contextIndexID, index.ID)
	index.RenewCtxInfo = &cachememdb.ContextInfo{
		Ctx:        renewCtx,
		CancelFunc: renewCtxInfo.CancelFunc,
		DoneCh:     renewCtxInfo.DoneCh,
	}

	sendReq := &SendRequest{
		Token: index.RequestToken,
		Request: &http.Request{
			Header: index.RequestHeader,
			Method: index.RequestMethod,
			URL: &url.URL{
				Path: index.RequestPath,
			},
		},
	}
	go c.startRenewing(renewCtx, index, sendReq, secret)

	return nil
}

// deriveNamespaceAndRevocationPath returns the namespace and relative path for
// revocation paths.
//
// If the path contains a namespace, but it's not a revocation path, it will be
// returned as-is, since there's no way to tell where the namespace ends and
// where the request path begins purely based off a string.
//
// Case 1: /v1/ns1/leases/revoke  -> ns1/, /v1/leases/revoke
// Case 2: ns1/ /v1/leases/revoke -> ns1/, /v1/leases/revoke
// Case 3: /v1/ns1/foo/bar  -> root/, /v1/ns1/foo/bar
// Case 4: ns1/ /v1/foo/bar -> ns1/, /v1/foo/bar
func deriveNamespaceAndRevocationPath(req *SendRequest) (string, string) {
	namespace := "root/"
	nsHeader := req.Request.Header.Get(consts.NamespaceHeaderName)
	if nsHeader != "" {
		namespace = nsHeader
	}

	fullPath := req.Request.URL.Path
	nonVersionedPath := strings.TrimPrefix(fullPath, "/v1")

	for _, pathToCheck := range revocationPaths {
		// We use strings.Contains here for paths that can contain
		// vars in the path, e.g. /v1/lease/revoke-prefix/:prefix
		i := strings.Index(nonVersionedPath, pathToCheck)
		// If there's no match, move on to the next check
		if i == -1 {
			continue
		}

		// If the index is 0, this is a relative path with no namespace preppended,
		// so we can break early
		if i == 0 {
			break
		}

		// We need to turn /ns1 into ns1/, this makes it easy
		namespaceInPath := nshelper.Canonicalize(nonVersionedPath[:i])

		// If it's root, we replace, otherwise we join
		if namespace == "root/" {
			namespace = namespaceInPath
		} else {
			namespace = namespace + namespaceInPath
		}

		return namespace, fmt.Sprintf("/v1%s", nonVersionedPath[i:])
	}

	return namespace, fmt.Sprintf("/v1%s", nonVersionedPath)
}

// RegisterAutoAuthToken adds the provided auto-token into the cache. This is
// primarily used to register the auto-auth token and should only be called
// within a sink's WriteToken func.
func (c *LeaseCache) RegisterAutoAuthToken(token string) error {
	// Get the token from the cache
	oldIndex, err := c.db.Get(cachememdb.IndexNameToken, token)
	if err != nil {
		return err
	}

	// If the index is found, defer its cancelFunc
	if oldIndex != nil {
		defer oldIndex.RenewCtxInfo.CancelFunc()
	}

	// The following randomly generated values are required for index stored by
	// the cache, but are not actually used. We use random values to prevent
	// accidental access.
	id, err := base62.Random(5)
	if err != nil {
		return err
	}
	namespace, err := base62.Random(5)
	if err != nil {
		return err
	}
	requestPath, err := base62.Random(5)
	if err != nil {
		return err
	}

	index := &cachememdb.Index{
		ID:          id,
		Token:       token,
		Namespace:   namespace,
		RequestPath: requestPath,
	}

	// Derive a context off of the lease cache's base context
	ctxInfo := c.createCtxInfo(nil)

	index.RenewCtxInfo = &cachememdb.ContextInfo{
		Ctx:        ctxInfo.Ctx,
		CancelFunc: ctxInfo.CancelFunc,
		DoneCh:     ctxInfo.DoneCh,
	}

	// Store the index in the cache
	c.logger.Debug("storing auto-auth token into the cache")
	err = c.Set(index, persistcache.TokenType)
	if err != nil {
		c.logger.Error("failed to cache the auto-auth token", "error", err)
		return err
	}

	return nil
}

type cacheClearInput struct {
	Type string

	RequestPath   string
	Namespace     string
	Token         string
	TokenAccessor string
	Lease         string
}

func parseCacheClearInput(req *cacheClearRequest) (*cacheClearInput, error) {
	if req == nil {
		return nil, errors.New("nil request options provided")
	}

	if req.Type == "" {
		return nil, errors.New("no type provided")
	}

	in := &cacheClearInput{
		Type:      req.Type,
		Namespace: req.Namespace,
	}

	switch req.Type {
	case "request_path":
		in.RequestPath = req.Value
	case "token":
		in.Token = req.Value
	case "token_accessor":
		in.TokenAccessor = req.Value
	case "lease":
		in.Lease = req.Value
	}

	return in, nil
}
