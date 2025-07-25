// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package kvstore

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"

	"go.etcd.io/etcd/api/v3/mvccpb"
	v3rpcErrors "go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	"go.etcd.io/etcd/client/pkg/v3/logutil"
	client "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
	clientyaml "go.etcd.io/etcd/client/v3/yaml"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/time/rate"
	"sigs.k8s.io/yaml"

	"github.com/cilium/cilium/api/v1/models"
	"github.com/cilium/cilium/pkg/backoff"
	"github.com/cilium/cilium/pkg/defaults"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
	ciliumrate "github.com/cilium/cilium/pkg/rate"
	ciliumratemetrics "github.com/cilium/cilium/pkg/rate/metrics"
	"github.com/cilium/cilium/pkg/spanstat"
	"github.com/cilium/cilium/pkg/time"
)

const (
	// EtcdBackendName is the backend name for etcd
	EtcdBackendName = "etcd"

	EtcdAddrOption               = "etcd.address"
	EtcdOptionConfig             = "etcd.config"
	EtcdOptionKeepAliveHeartbeat = "etcd.keepaliveHeartbeat"
	EtcdOptionKeepAliveTimeout   = "etcd.keepaliveTimeout"

	// EtcdRateLimitOption specifies maximum kv operations per second
	EtcdRateLimitOption = "etcd.qps"

	// EtcdBootstrapRateLimitOption specifies maximum kv operations per second
	// during bootstrapping
	EtcdBootstrapRateLimitOption = "etcd.bootstrapQps"

	// EtcdMaxInflightOption specifies maximum inflight concurrent kv store operations
	EtcdMaxInflightOption = "etcd.maxInflight"

	// EtcdListLimitOption limits the number of results retrieved in one batch
	// by ListAndWatch operations. A 0 value equals to no limit.
	EtcdListLimitOption = "etcd.limit"

	// etcdMaxKeysPerLease is the maximum number of keys that can be attached to a lease
	etcdMaxKeysPerLease = 1000
)

// ErrLockLeaseExpired is an error whenever the lease of the lock does not
// exist or it was expired.
var ErrLockLeaseExpired = errors.New("transaction did not succeed: lock lease expired")

// ErrOperationAbortedByInterceptor is an error that can be used by custom
// interceptors to signal that the given operation has been intentionally
// aborted, and should not be logged as an error.
var ErrOperationAbortedByInterceptor = errors.New("operation aborted")

type etcdModule struct {
	opts backendOptions
}

var (
	// statusCheckTimeout is the timeout when performing status checks with
	// all etcd endpoints
	statusCheckTimeout = 10 * time.Second

	// initialConnectionTimeout  is the timeout for the initial connection to
	// the etcd server
	initialConnectionTimeout = 15 * time.Minute

	// etcd3ClientLogger is the logger used for the underlying etcd clients. We
	// explicitly initialize a logger and propagate it to prevent each client from
	// automatically creating a new one, which comes with a significant memory cost.
	etcd3ClientLogger *zap.Logger
)

func newEtcdModule() backendModule {
	return &etcdModule{
		opts: backendOptions{
			EtcdAddrOption: &backendOption{
				description: "Addresses of etcd cluster",
			},
			EtcdOptionConfig: &backendOption{
				description: "Path to etcd configuration file",
			},
			EtcdOptionKeepAliveTimeout: &backendOption{
				description: "Timeout after which an unanswered heartbeat triggers the connection to be closed",
				validate: func(v string) error {
					_, err := time.ParseDuration(v)
					return err
				},
			},
			EtcdOptionKeepAliveHeartbeat: &backendOption{
				description: "Heartbeat interval to keep gRPC connection alive",
				validate: func(v string) error {
					_, err := time.ParseDuration(v)
					return err
				},
			},
			EtcdRateLimitOption: &backendOption{
				description: "Rate limit in kv store operations per second",
				validate: func(v string) error {
					_, err := strconv.Atoi(v)
					return err
				},
			},
			EtcdBootstrapRateLimitOption: &backendOption{
				description: "Rate limit in kv store operations per second during bootstrapping",
				validate: func(v string) error {
					_, err := strconv.Atoi(v)
					return err
				},
			},
			EtcdMaxInflightOption: &backendOption{
				description: "Maximum inflight concurrent kv store operations; defaults to etcd.qps if unset",
				validate: func(v string) error {
					_, err := strconv.Atoi(v)
					return err
				},
			},
			EtcdListLimitOption: &backendOption{
				description: "Max number of results retrieved in one batch by ListAndWatch operations (0 = no limit)",
				validate: func(v string) error {
					_, err := strconv.Atoi(v)
					return err
				},
			},
		},
	}
}

func (e *etcdModule) createInstance() backendModule {
	return newEtcdModule()
}

func (e *etcdModule) setConfig(logger *slog.Logger, opts map[string]string) error {
	return setOpts(logger, opts, e.opts)
}

func shuffleEndpoints(endpoints []string) {
	rand.Shuffle(len(endpoints), func(i, j int) {
		endpoints[i], endpoints[j] = endpoints[j], endpoints[i]
	})
}

type clientOptions struct {
	Endpoint   string
	ConfigPath string

	KeepAliveHeartbeat time.Duration
	KeepAliveTimeout   time.Duration
	RateLimit          int
	BootstrapRateLimit int
	MaxInflight        int
	ListBatchSize      int
}

func (e *etcdModule) newClient(ctx context.Context, logger *slog.Logger, opts ExtraOptions) (BackendOperations, chan error) {
	errChan := make(chan error, 1)

	clientOptions := clientOptions{
		KeepAliveHeartbeat: 15 * time.Second,
		KeepAliveTimeout:   25 * time.Second,
		RateLimit:          defaults.KVstoreQPS,
		ListBatchSize:      256,
	}

	if o, ok := e.opts[EtcdRateLimitOption]; ok && o.value != "" {
		clientOptions.RateLimit, _ = strconv.Atoi(o.value)
	}

	if o, ok := e.opts[EtcdBootstrapRateLimitOption]; ok && o.value != "" {
		clientOptions.BootstrapRateLimit, _ = strconv.Atoi(o.value)
	}

	if o, ok := e.opts[EtcdMaxInflightOption]; ok && o.value != "" {
		clientOptions.MaxInflight, _ = strconv.Atoi(o.value)
	}

	if clientOptions.MaxInflight == 0 {
		clientOptions.MaxInflight = clientOptions.RateLimit
	}

	if o, ok := e.opts[EtcdListLimitOption]; ok && o.value != "" {
		clientOptions.ListBatchSize, _ = strconv.Atoi(o.value)
	}

	if o, ok := e.opts[EtcdOptionKeepAliveTimeout]; ok && o.value != "" {
		clientOptions.KeepAliveTimeout, _ = time.ParseDuration(o.value)
	}

	if o, ok := e.opts[EtcdOptionKeepAliveHeartbeat]; ok && o.value != "" {
		clientOptions.KeepAliveHeartbeat, _ = time.ParseDuration(o.value)
	}

	clientOptions.Endpoint = e.opts[EtcdAddrOption].value
	clientOptions.ConfigPath = e.opts[EtcdOptionConfig].value

	if clientOptions.Endpoint == "" && clientOptions.ConfigPath == "" {
		errChan <- fmt.Errorf("invalid etcd configuration, %s or %s must be specified",
			EtcdOptionConfig, EtcdAddrOption)
		close(errChan)
		return nil, errChan
	}

	logger.Info(
		"Creating etcd client",
		logfields.ConfigPath, clientOptions.ConfigPath,
		logfields.KeepAliveHeartbeat, clientOptions.KeepAliveHeartbeat,
		logfields.KeepAliveTimeout, clientOptions.KeepAliveTimeout,
		logfields.RateLimit, clientOptions.RateLimit,
		logfields.MaxInflight, clientOptions.MaxInflight,
		logfields.ListLimit, clientOptions.ListBatchSize,
	)

	for {
		// connectEtcdClient will close errChan when the connection attempt has
		// been successful
		backend, err := connectEtcdClient(ctx, logger, errChan, clientOptions, opts)
		switch {
		case os.IsNotExist(err):
			logger.Info("Waiting for all etcd configuration files to be available",
				logfields.Error, err,
			)
			time.Sleep(5 * time.Second)
		case err != nil:
			errChan <- err
			close(errChan)
			return backend, errChan
		default:
			return backend, errChan
		}
	}
}

func init() {
	// register etcd module for use
	registerBackend(EtcdBackendName, newEtcdModule())

	if duration := os.Getenv("CILIUM_ETCD_STATUS_CHECK_INTERVAL"); duration != "" {
		timeout, err := time.ParseDuration(duration)
		if err == nil {
			statusCheckTimeout = timeout
		}
	}

	// Initialize the etcd client logger.
	// slogloggercheck: it's safe to use the default logger here since it's just to print a warning from etcdClientDebugLevel.
	l, err := logutil.CreateDefaultZapLogger(etcdClientDebugLevel(logging.DefaultSlogLogger))
	if err != nil {
		// slogloggercheck: it's safe to use the default logger here since it's just to print a warning.
		logging.DefaultSlogLogger.Warn("Failed to initialize etcd client logger",
			logfields.Error, err,
		)
		l = zap.NewNop()
	}
	etcd3ClientLogger = l.Named("etcd-client")
}

// etcdClientDebugLevel translates ETCD_CLIENT_DEBUG into zap log level.
// This is a copy of a private etcd client function:
// https://github.com/etcd-io/etcd/blob/v3.5.9/client/v3/logger.go#L47-L59
func etcdClientDebugLevel(logger *slog.Logger) zapcore.Level {
	envLevel := os.Getenv("ETCD_CLIENT_DEBUG")
	if envLevel == "" || envLevel == "true" {
		return zapcore.InfoLevel
	}
	var l zapcore.Level
	if err := l.Set(envLevel); err != nil {
		logger.Warn("Invalid value for environment variable 'ETCD_CLIENT_DEBUG'. Using default level: 'info'")
		return zapcore.InfoLevel
	}
	return l
}

// Hint tries to improve the error message displayed to te user.
func Hint(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("etcd client timeout exceeded")
	}
	return err
}

type etcdClient struct {
	// stopStatusChecker is closed when the status checker can be terminated
	stopStatusChecker chan struct{}

	client *client.Client

	// config and configPath are initialized once and never written to again, they can be accessed without locking
	config *client.Config

	// statusCheckErrors receives all errors reported by statusChecker()
	statusCheckErrors chan error

	// protects all sessions and sessionErr from concurrent access
	lock.RWMutex

	// leaseManager manages the acquisition of etcd leases for generic purposes
	leaseManager *etcdLeaseManager
	// lockLeaseManager manages the acquisition of etcd leases for locking
	// purposes, associated with a shorter TTL
	lockLeaseManager *etcdLeaseManager

	// statusLock protects status for read/write access
	statusLock lock.RWMutex

	// status is a snapshot of the latest etcd cluster status
	status models.Status

	extraOptions ExtraOptions

	limiter       *ciliumrate.APILimiter
	listBatchSize int

	lastHeartbeat time.Time

	leaseExpiredObservers lock.Map[string, func(string)]

	// logger is the scoped logger associated with this client
	logger *slog.Logger
}

type etcdMutex struct {
	mutex    *concurrency.Mutex
	onUnlock func()
	path     string
}

func (e *etcdMutex) Unlock(ctx context.Context) (err error) {
	e.onUnlock()
	defer func(duration *spanstat.SpanStat) {
		increaseMetric(e.path, metricDelete, "Unlock", duration.EndError(err).Total(), err)
	}(spanstat.Start())
	return e.mutex.Unlock(ctx)
}

func (e *etcdMutex) Comparator() any {
	return e.mutex.IsOwner()
}

// StatusCheckErrors returns a channel which receives status check errors
func (e *etcdClient) StatusCheckErrors() <-chan error {
	return e.statusCheckErrors
}

func (e *etcdClient) maybeWaitForInitLock(ctx context.Context) error {
	if e.extraOptions.NoLockQuorumCheck {
		return nil
	}
	limiter := e.newExpBackoffRateLimiter("etcd-client-init-lock")
	defer limiter.Reset()
	for {
		select {
		case <-e.client.Ctx().Done():
			return fmt.Errorf("client context ended: %w", e.client.Ctx().Err())
		case <-ctx.Done():
			return fmt.Errorf("caller context ended: %w", ctx.Err())
		default:
		}

		// Generate a random number so that we can acquire a lock even
		// if other agents are killed while locking this path.
		randNumber := strconv.FormatUint(rand.Uint64(), 16)
		locker, err := e.LockPath(ctx, InitLockPath+"/"+randNumber)
		if err == nil {
			locker.Unlock(context.Background())
			e.logger.Debug("Distributed lock successful, etcd has quorum")
			return nil
		}
		limiter.Wait(ctx)
	}
}

func (e *etcdClient) isConnectedAndHasQuorum(ctx context.Context) error {
	ctxTimeout, cancel := context.WithTimeout(ctx, statusCheckTimeout)
	defer cancel()

	if err := e.maybeWaitForInitLock(ctxTimeout); err != nil {
		recordQuorumError("lock timeout")
		return fmt.Errorf("unable to acquire lock: %w", err)
	}

	return nil
}

func connectEtcdClient(ctx context.Context, logger *slog.Logger, errChan chan error, clientOptions clientOptions, opts ExtraOptions) (BackendOperations, error) {
	config := &client.Config{
		Endpoints: []string{clientOptions.Endpoint},
	}

	if cfgPath := clientOptions.ConfigPath; cfgPath != "" {
		cfg, err := clientyaml.NewConfig(cfgPath)
		if err != nil {
			return nil, err
		}
		if cfg.TLS != nil {
			cfg.TLS.GetClientCertificate, err = getClientCertificateReloader(cfgPath)
			if err != nil {
				return nil, err
			}
		}
		config = cfg
	}

	// Shuffle the order of endpoints to avoid all agents connecting to the
	// same etcd endpoint and to work around etcd client library failover
	// bugs. (https://github.com/etcd-io/etcd/pull/9860)
	if config.Endpoints != nil {
		shuffleEndpoints(config.Endpoints)
	}

	// Set client context so that client can be cancelled from outside
	config.Context = ctx
	// Configure the dial options provided by the caller.
	config.DialOptions = append(config.DialOptions, opts.DialOption...)
	// Set DialTimeout to 0, otherwise the creation of a new client will
	// block until DialTimeout is reached or a connection to the server
	// is made.
	config.DialTimeout = 0
	// Ping the server to verify if the server connection is still valid
	config.DialKeepAliveTime = clientOptions.KeepAliveHeartbeat
	// Timeout if the server does not reply within 15 seconds and close the
	// connection. Ideally it should be lower than staleLockTimeout
	config.DialKeepAliveTimeout = clientOptions.KeepAliveTimeout

	// Use the shared etcd client logger to prevent unnecessary allocations.
	config.Logger = etcd3ClientLogger

	c, err := client.New(*config)
	if err != nil {
		return nil, err
	}

	ec := &etcdClient{
		client: c,
		config: config,
		status: models.Status{
			State: models.StatusStateWarning,
			Msg:   "Waiting for initial connection to be established",
		},
		stopStatusChecker: make(chan struct{}),
		extraOptions:      opts,
		listBatchSize:     clientOptions.ListBatchSize,
		statusCheckErrors: make(chan error, 128),
		logger: logger.With(
			logfields.Endpoints, config.Endpoints,
			logfields.Config, clientOptions.ConfigPath,
		),
	}

	initialLimit := clientOptions.RateLimit
	// If BootstrapRateLimit and BootstrapComplete are provided, set the
	// initial rate limit to BootstrapRateLimit and apply the standard rate limit
	// once the caller has signaled that bootstrap is complete by closing the channel.
	if clientOptions.BootstrapRateLimit > 0 && opts.BootstrapComplete != nil {
		ec.logger.Info(
			"Setting client QPS limit for bootstrap",
			logfields.EtcdQPSLimit, clientOptions.BootstrapRateLimit,
		)
		initialLimit = clientOptions.BootstrapRateLimit
		go func() {
			select {
			case <-ec.client.Ctx().Done():
			case <-opts.BootstrapComplete:
				ec.logger.Info(
					"Bootstrap complete, updating client QPS limit",
					logfields.EtcdQPSLimit, clientOptions.RateLimit,
				)
				ec.limiter.SetRateLimit(rate.Limit(clientOptions.RateLimit))
			}
		}()
	}

	ec.limiter = ciliumrate.NewAPILimiter(logger, makeSessionName("etcd", opts), ciliumrate.APILimiterParameters{
		RateLimit:        rate.Limit(initialLimit),
		RateBurst:        clientOptions.RateLimit,
		ParallelRequests: clientOptions.MaxInflight,
	}, ciliumratemetrics.APILimiterObserver())

	ec.logger.Info("Connecting to etcd server...")

	leaseTTL := cmp.Or(opts.LeaseTTL, defaults.KVstoreLeaseTTL)
	ec.leaseManager = newEtcdLeaseManager(ec.logger, c, leaseTTL, etcdMaxKeysPerLease, ec.expiredLeaseObserver)
	ec.lockLeaseManager = newEtcdLeaseManager(ec.logger, c, defaults.LockLeaseTTL, etcdMaxKeysPerLease, nil)

	go ec.asyncConnectEtcdClient(errChan)

	return ec, nil
}

func (e *etcdClient) asyncConnectEtcdClient(errChan chan<- error) {
	var (
		ctx      = e.client.Ctx()
		listDone = make(chan struct{})
	)

	propagateError := func(err error) {
		e.statusLock.Lock()
		e.status.State = models.StatusStateFailure
		e.status.Msg = fmt.Sprintf("Failed to establish initial connection: %s", err.Error())
		e.statusLock.Unlock()

		errChan <- err
		close(errChan)

		e.statusCheckErrors <- err
		close(e.statusCheckErrors)
	}

	wctx, wcancel := context.WithTimeout(ctx, initialConnectionTimeout)

	// Don't create a session when running with lock quorum check disabled
	// (i.e., for clustermesh clients), to not introduce unnecessary overhead
	// on the target etcd instance, considering that the session would never
	// be used again. Instead, we'll just rely on the successful synchronization
	// of the heartbeat watcher as a signal that we successfully connected.
	if !e.extraOptions.NoLockQuorumCheck {
		_, err := e.lockLeaseManager.GetSession(wctx, InitLockPath)
		if err != nil {
			wcancel()
			if errors.Is(err, context.DeadlineExceeded) {
				err = fmt.Errorf("timed out while waiting for etcd connection. Ensure that etcd is running on %s", e.config.Endpoints)
			}

			propagateError(err)
			return
		}
	}

	go func() {
		// Report connection established to the caller and start the status
		// checker only after successfully starting the heatbeat watcher, as
		// additional sanity check. This also guarantees that there's already
		// been an interaction with the target etcd instance at that point,
		// and its corresponding cluster ID has been retrieved if using the
		// "clusterLock" interceptors.
		select {
		case <-wctx.Done():
			propagateError(fmt.Errorf("timed out while starting the heartbeat watcher. Ensure that etcd is running on %s", e.config.Endpoints))
			return
		case <-listDone:
			e.logger.Info("Initial etcd connection established")
			close(errChan)
		}

		wcancel()
		e.statusChecker()
	}()

	events := e.ListAndWatch(ctx, HeartbeatPath)
	for event := range events {
		switch event.Typ {
		case EventTypeDelete:
			// A deletion event is not an heartbeat signal
			continue

		case EventTypeListDone:
			// A list done event signals the initial connection, but
			// is also not an heartbeat signal.
			close(listDone)
			continue
		}

		// It is tempting to compare against the heartbeat value stored in
		// the key. However, this would require the time on all nodes to
		// be synchronized. Instead, let's just assume current time.
		e.RWMutex.Lock()
		e.lastHeartbeat = time.Now()
		e.RWMutex.Unlock()
		e.logger.Debug("Received update notification of heartbeat")
	}
}

// makeSessionName builds up a session/locksession controller name
// clusterName is expected to be empty for main kvstore connection
func makeSessionName(sessionPrefix string, opts ExtraOptions) string {
	if opts.ClusterName != "" {
		return sessionPrefix + "-" + opts.ClusterName
	}
	return sessionPrefix
}

func (e *etcdClient) newExpBackoffRateLimiter(name string) backoff.Exponential {
	return backoff.Exponential{
		Logger: e.logger,
		Name:   name,
		Min:    50 * time.Millisecond,
		Max:    1 * time.Minute,

		NodeManager: backoff.NewNodeManager(e.extraOptions.ClusterSizeDependantInterval),
	}
}

func (e *etcdClient) LockPath(ctx context.Context, path string) (locker KVLocker, err error) {
	// Create the context first, so that the timeout also accounts for the time
	// possibly required to acquire a new session (if not already established).
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	session, err := e.lockLeaseManager.GetSession(ctx, path)
	if err != nil {
		return nil, Hint(err)
	}

	defer func(duration *spanstat.SpanStat) {
		increaseMetric(path, metricSet, "Lock", duration.EndError(err).Total(), err)
	}(spanstat.Start())
	mu := concurrency.NewMutex(session, path)
	err = mu.Lock(ctx)
	if err != nil {
		e.lockLeaseManager.CancelIfExpired(err, session.Lease())
		return nil, Hint(err)
	}

	release := func() { e.lockLeaseManager.Release(path) }
	return &etcdMutex{mutex: mu, onUnlock: release, path: path}, nil
}

func (e *etcdClient) DeletePrefix(ctx context.Context, path string) (err error) {
	defer func() {
		Trace(e.logger, "DeletePrefix",
			logfields.Error, err,
			fieldPrefix, path,
		)
	}()
	lr, err := e.limiter.Wait(ctx)
	if err != nil {
		return Hint(err)
	}

	defer func(duration *spanstat.SpanStat) {
		increaseMetric(path, metricDelete, "DeletePrefix", duration.EndError(err).Total(), err)
	}(spanstat.Start())

	_, err = e.client.Delete(ctx, path, client.WithPrefix())
	// Using lr.Error for convenience, as it matches lr.Done() when err is nil
	lr.Error(err, -1)

	if err == nil {
		e.leaseManager.ReleasePrefix(path)
	}

	return Hint(err)
}

// watch starts watching for changes in a prefix
func (e *etcdClient) watch(ctx context.Context, prefix string, events emitter) {
	localCache := watcherCache{}
	listSignalSent := false

	scopedLog := e.logger.With(fieldPrefix, prefix)
	scopedLog.Info("Starting watcher")

	defer func() {
		scopedLog.Info("Stopped watcher")
		events.close()
	}()

	// errLimiter is used to rate limit the retry of the first Get request in case an error
	// has occurred, to prevent overloading the etcd server due to the more aggressive
	// default rate limiter.
	errLimiter := e.newExpBackoffRateLimiter("etcd-list-before-watch-error")

reList:
	for {
		select {
		case <-e.client.Ctx().Done():
			return
		case <-ctx.Done():
			return
		default:
		}

		lr, err := e.limiter.Wait(ctx)
		if err != nil {
			continue
		}
		kvs, revision, err := e.paginatedList(ctx, scopedLog, prefix)
		if err != nil {
			lr.Error(err, -1)

			if attempt := errLimiter.Attempt(); attempt < 10 {
				scopedLog.Info(
					"Unable to list keys before starting watcher, will retry",
					logfields.Error, Hint(err),
					logfields.Attempt, attempt,
				)
			} else {
				scopedLog.Warn(
					"Unable to list keys before starting watcher, will retry",
					logfields.Error, Hint(err),
					logfields.Attempt, attempt,
				)
			}

			errLimiter.Wait(ctx)
			continue
		}
		lr.Done()
		errLimiter.Reset()

		scopedLog.Info(
			"Successfully listed keys before starting watcher",
			logfields.Count, len(kvs),
			fieldRev, revision,
		)

		for _, key := range kvs {
			t := EventTypeCreate
			if localCache.Exists(key.Key) {
				t = EventTypeModify
			}

			localCache.MarkInUse(key.Key)

			if traceEnabled {
				scopedLog.Debug("Emitting list result",
					logfields.EventType, t,
					logfields.Key, key.Key,
					logfields.Value, key.Value,
				)
			}

			if !events.emit(ctx, KeyValueEvent{
				Key:   string(key.Key),
				Value: key.Value,
				Typ:   t,
			}) {
				return
			}
		}

		nextRev := revision + 1

		// Send out deletion events for all keys that were deleted
		// between our last known revision and the latest revision
		// received via Get
		if !localCache.RemoveDeleted(func(k string) bool {
			event := KeyValueEvent{
				Key: k,
				Typ: EventTypeDelete,
			}

			if traceEnabled {
				scopedLog.Debug("Emitting EventTypeDelete event",
					logfields.Key, k,
				)
			}
			return events.emit(ctx, event)
		}) {
			return
		}

		// Only send the list signal once
		if !listSignalSent {
			if !events.emit(ctx, KeyValueEvent{Typ: EventTypeListDone}) {
				return
			}
			listSignalSent = true
		}

	recreateWatcher:
		scopedLog.Info(
			"Starting to watch prefix",
			fieldRev, nextRev,
		)

		lr, err = e.limiter.Wait(ctx)
		if err != nil {
			select {
			case <-e.client.Ctx().Done():
				return
			case <-ctx.Done():
				return
			default:
				goto recreateWatcher
			}
		}

		etcdWatch := e.client.Watch(client.WithRequireLeader(ctx), prefix,
			client.WithPrefix(), client.WithRev(nextRev))
		lr.Done()

		for {
			select {
			case <-e.client.Ctx().Done():
				return
			case <-ctx.Done():
				return
			case r, ok := <-etcdWatch:
				if !ok {
					time.Sleep(50 * time.Millisecond)
					goto recreateWatcher
				}

				if err := r.Err(); err != nil {
					switch {
					case errors.Is(err, ErrOperationAbortedByInterceptor):
						// Aborted on purpose by a custom interceptor.
						scopedLog.Debug("Etcd watcher aborted",
							logfields.Error, Hint(err),
							fieldRev, r.Header.Revision,
						)
					case errors.Is(err, v3rpcErrors.ErrCompacted):
						// We tried to watch on a compacted
						// revision that may no longer exist,
						// recreate the watcher and try to
						// watch on the next possible revision
						scopedLog.Info("Tried watching on compacted revision. Triggering relist of all keys",
							logfields.Error, Hint(err),
							fieldRev, r.Header.Revision,
						)
					default:
						scopedLog.Info("Etcd watcher errored. Triggering relist of all keys",
							logfields.Error, Hint(err),
							fieldRev, r.Header.Revision,
						)
					}

					// mark all local keys in state for
					// deletion unless the upcoming GET
					// marks them alive
					localCache.MarkAllForDeletion()

					goto reList
				}

				nextRev = r.Header.Revision + 1
				if traceEnabled {
					scopedLog.Debug("Received event from etcd",
						logfields.Response, r,
					)
				}

				for _, ev := range r.Events {
					event := KeyValueEvent{
						Key:   string(ev.Kv.Key),
						Value: ev.Kv.Value,
					}

					switch {
					case ev.Type == client.EventTypeDelete:
						event.Typ = EventTypeDelete
						localCache.RemoveKey(ev.Kv.Key)
					case ev.IsCreate():
						event.Typ = EventTypeCreate
						localCache.MarkInUse(ev.Kv.Key)
					default:
						event.Typ = EventTypeModify
						localCache.MarkInUse(ev.Kv.Key)
					}

					if traceEnabled {
						scopedLog.Debug("Emitting event",
							logfields.EventType, event.Typ,
							logfields.Key, event.Key,
							logfields.Value, event.Value,
						)
					}
					if !events.emit(ctx, event) {
						return
					}
				}
			}
		}
	}
}

func (e *etcdClient) paginatedList(ctx context.Context, log *slog.Logger, prefix string) (kvs []*mvccpb.KeyValue, revision int64, err error) {
	start, end := prefix, client.GetPrefixRangeEnd(prefix)

	for {
		res, err := e.client.Get(ctx, start, client.WithRange(end),
			client.WithSort(client.SortByKey, client.SortAscend),
			client.WithRev(revision), client.WithSerializable(),
			client.WithLimit(int64(e.listBatchSize)),
		)
		if err != nil {
			return nil, 0, err
		}

		log.Debug(
			"Received list response from etcd",
			fieldNumEntries, len(res.Kvs),
			fieldRemainingEntries, res.Count-int64(len(res.Kvs)),
		)

		if kvs == nil {
			kvs = make([]*mvccpb.KeyValue, 0, res.Count)
		}

		kvs = append(kvs, res.Kvs...)

		// Do not modify the revision once set, as subsequent Get queries may
		// return higher revisions in case other operations are performed in
		// parallel (regardless of whether we specify WithRev), leading to
		// possibly missing the events happened in the meantime.
		if revision == 0 {
			revision = res.Header.Revision
		}

		if !res.More || len(res.Kvs) == 0 {
			return kvs, revision, nil
		}

		start = string(res.Kvs[len(res.Kvs)-1].Key) + "\x00"
	}
}

func (e *etcdClient) determineEndpointStatus(ctx context.Context, endpointAddress string) (string, error) {
	ctxTimeout, cancel := context.WithTimeout(ctx, statusCheckTimeout)
	defer cancel()

	e.logger.Debug("Checking status to etcd endpoint",
		logfields.Endpoint, endpointAddress,
	)

	status, err := e.client.Status(ctxTimeout, endpointAddress)
	if err != nil {
		return fmt.Sprintf("%s - %s", endpointAddress, err), Hint(err)
	}

	str := fmt.Sprintf("%s - %s", endpointAddress, status.Version)
	if status.Header.MemberId == status.Leader {
		str += " (Leader)"
	}

	return str, nil
}

func (e *etcdClient) statusChecker() {
	ctx := context.Background()

	var consecutiveQuorumErrors uint
	var err error

	e.RWMutex.Lock()
	// Ensure that lastHearbeat is always set to a non-zero value when starting
	// the status checker, to guarantee that we can correctly compute the time
	// difference even in case we don't receive any heartbeat event. Indeed, we
	// want to consider that as an heartbeat failure after the usual timeout.
	if e.lastHeartbeat.IsZero() {
		e.lastHeartbeat = time.Now()
	}
	e.RWMutex.Unlock()

	for {
		newStatus := []string{}
		ok := 0

		quorumError := e.isConnectedAndHasQuorum(ctx)

		e.RWMutex.RLock()
		lastHeartbeat := e.lastHeartbeat
		e.RWMutex.RUnlock()

		if heartbeatDelta := time.Since(lastHeartbeat); heartbeatDelta > 2*HeartbeatWriteInterval {
			recordQuorumError("no event received")
			quorumError = fmt.Errorf("%s since last heartbeat update has been received", heartbeatDelta)
		}

		endpoints := e.client.Endpoints()
		if e.extraOptions.NoEndpointStatusChecks {
			newStatus = append(newStatus, "endpoint status checks are disabled")

			if quorumError == nil {
				ok = len(endpoints)
			}
		} else {
			for _, ep := range endpoints {
				st, err := e.determineEndpointStatus(ctx, ep)
				if err == nil {
					ok++
				}

				newStatus = append(newStatus, st)
			}
		}

		allConnected := len(endpoints) == ok

		quorumString := "true"
		if quorumError != nil {
			quorumString = quorumError.Error()
			consecutiveQuorumErrors++
			quorumString += fmt.Sprintf(", consecutive-errors=%d", consecutiveQuorumErrors)
		} else {
			consecutiveQuorumErrors = 0
		}

		e.statusLock.Lock()

		switch {
		case consecutiveQuorumErrors > cmp.Or(e.extraOptions.MaxConsecutiveQuorumErrors, defaults.KVstoreMaxConsecutiveQuorumErrors):
			err = fmt.Errorf("quorum check failed %d times in a row: %w", consecutiveQuorumErrors, quorumError)
			e.status.State = models.StatusStateFailure
			e.status.Msg = fmt.Sprintf("Err: %s", err.Error())
		case len(endpoints) > 0 && ok == 0:
			err = fmt.Errorf("not able to connect to any etcd endpoints")
			e.status.State = models.StatusStateFailure
			e.status.Msg = fmt.Sprintf("Err: %s", err.Error())
		default:
			err = nil
			e.status.State = models.StatusStateOk
			e.status.Msg = fmt.Sprintf("etcd: %d/%d connected, leases=%d, lock leases=%d, has-quorum=%s: %s",
				ok, len(endpoints), e.leaseManager.TotalLeases(), e.lockLeaseManager.TotalLeases(), quorumString, strings.Join(newStatus, "; "))
		}

		e.statusLock.Unlock()
		if err != nil {
			select {
			case e.statusCheckErrors <- err:
			default:
				// Channel's buffer is full, skip sending errors to the channel but log warnings instead
				e.logger.Warn(
					"Status check error channel is full, dropping this error",
					logfields.Error, err,
				)
			}
		}

		select {
		case <-e.stopStatusChecker:
			close(e.statusCheckErrors)
			return
		case <-time.After(e.extraOptions.StatusCheckInterval(allConnected)):
		}
	}
}

func (e *etcdClient) Status() *models.Status {
	e.statusLock.RLock()
	defer e.statusLock.RUnlock()

	return &models.Status{
		State: e.status.State,
		Msg:   e.status.Msg,
	}
}

// GetIfLocked returns value of key if the client is still holding the given lock.
func (e *etcdClient) GetIfLocked(ctx context.Context, key string, lock KVLocker) (bv []byte, err error) {
	if traceEnabled {
		defer func() {
			Trace(e.logger, "GetIfLocked",
				logfields.Error, err,
				fieldKey, key,
				fieldValue, string(bv),
			)
		}()
	}
	lr, err := e.limiter.Wait(ctx)
	if err != nil {
		return nil, Hint(err)
	}
	defer func(duration *spanstat.SpanStat) {
		increaseMetric(key, metricRead, "GetLocked", duration.EndError(err).Total(), err)
	}(spanstat.Start())

	opGet := client.OpGet(key)
	cmp := lock.Comparator().(client.Cmp)
	txnReply, err := e.client.Txn(ctx).If(cmp).Then(opGet).Commit()
	if err == nil && !txnReply.Succeeded {
		err = ErrLockLeaseExpired
	}

	if err != nil {
		lr.Error(err, -1)
		return nil, Hint(err)
	}

	lr.Done()
	getR := txnReply.Responses[0].GetResponseRange()
	// RangeResponse
	if getR.Count == 0 {
		return nil, nil
	}
	return getR.Kvs[0].Value, nil
}

// Get returns value of key
func (e *etcdClient) Get(ctx context.Context, key string) (bv []byte, err error) {
	if traceEnabled {
		defer func() {
			Trace(e.logger, "Get",
				logfields.Error, err,
				fieldKey, key,
				fieldValue, string(bv),
			)
		}()
	}
	lr, err := e.limiter.Wait(ctx)
	if err != nil {
		return nil, Hint(err)
	}
	defer func(duration *spanstat.SpanStat) {
		increaseMetric(key, metricRead, "Get", duration.EndError(err).Total(), err)
	}(spanstat.Start())

	getR, err := e.client.Get(ctx, key)
	if err != nil {
		lr.Error(err, -1)
		return nil, Hint(err)
	}
	lr.Done()

	if getR.Count == 0 {
		return nil, nil
	}
	return getR.Kvs[0].Value, nil
}

// DeleteIfLocked deletes a key if the client is still holding the given lock.
func (e *etcdClient) DeleteIfLocked(ctx context.Context, key string, lock KVLocker) (err error) {
	if traceEnabled {
		defer func() {
			Trace(e.logger, "DeleteIfLocked",
				logfields.Error, err,
				fieldKey, key,
			)
		}()
	}
	lr, err := e.limiter.Wait(ctx)
	if err != nil {
		return Hint(err)
	}
	defer func(duration *spanstat.SpanStat) {
		increaseMetric(key, metricDelete, "DeleteLocked", duration.EndError(err).Total(), err)
	}(spanstat.Start())

	opDel := client.OpDelete(key)
	cmp := lock.Comparator().(client.Cmp)
	txnReply, err := e.client.Txn(ctx).If(cmp).Then(opDel).Commit()
	if err == nil && !txnReply.Succeeded {
		err = ErrLockLeaseExpired
	}
	if err == nil {
		e.leaseManager.Release(key)
	}

	// Using lr.Error for convenience, as it matches lr.Done() when err is nil
	lr.Error(err, -1)
	return Hint(err)
}

// Delete deletes a key
func (e *etcdClient) Delete(ctx context.Context, key string) (err error) {
	if traceEnabled {
		defer func() {
			Trace(e.logger, "Delete",
				logfields.Error, err,
				fieldKey, key,
			)
		}()
	}
	lr, err := e.limiter.Wait(ctx)
	if err != nil {
		return Hint(err)
	}
	defer func(duration *spanstat.SpanStat) {
		increaseMetric(key, metricDelete, "Delete", duration.EndError(err).Total(), err)
	}(spanstat.Start())

	_, err = e.client.Delete(ctx, key)
	// Using lr.Error for convenience, as it matches lr.Done() when err is nil
	lr.Error(err, -1)

	if err == nil {
		e.leaseManager.Release(key)
	}

	return Hint(err)
}

// UpdateIfLocked updates a key if the client is still holding the given lock.
func (e *etcdClient) UpdateIfLocked(ctx context.Context, key string, value []byte, lease bool, lock KVLocker) (err error) {
	if traceEnabled {
		defer func() {
			Trace(e.logger, "UpdateIfLocked",
				logfields.Error, err,
				fieldKey, key,
				fieldValue, string(value),
				fieldAttachLease, lease,
			)
		}()
	}
	var leaseID client.LeaseID
	if lease {
		leaseID, err = e.leaseManager.GetLeaseID(ctx, key)
		if err != nil {
			return Hint(err)
		}
	}
	lr, err := e.limiter.Wait(ctx)
	if err != nil {
		return Hint(err)
	}
	defer func(duration *spanstat.SpanStat) {
		increaseMetric(key, metricSet, "UpdateIfLocked", duration.EndError(err).Total(), err)
	}(spanstat.Start())

	var txnReply *client.TxnResponse
	opPut := client.OpPut(key, string(value), client.WithLease(leaseID))
	cmp := lock.Comparator().(client.Cmp)
	txnReply, err = e.client.Txn(ctx).If(cmp).Then(opPut).Commit()
	e.leaseManager.CancelIfExpired(err, leaseID)

	if err == nil && !txnReply.Succeeded {
		err = ErrLockLeaseExpired
	}

	// Using lr.Error for convenience, as it matches lr.Done() when err is nil
	lr.Error(err, -1)
	return Hint(err)
}

// Update creates or updates a key
func (e *etcdClient) Update(ctx context.Context, key string, value []byte, lease bool) (err error) {
	if traceEnabled {
		defer func() {
			Trace(e.logger, "Update",
				logfields.Error, err,
				fieldKey, key,
				fieldValue, string(value),
				fieldAttachLease, lease,
			)
		}()
	}
	var leaseID client.LeaseID
	if lease {
		leaseID, err = e.leaseManager.GetLeaseID(ctx, key)
		if err != nil {
			return Hint(err)
		}
	}
	lr, err := e.limiter.Wait(ctx)
	if err != nil {
		return Hint(err)
	}
	defer func(duration *spanstat.SpanStat) {
		increaseMetric(key, metricSet, "Update", duration.EndError(err).Total(), err)
	}(spanstat.Start())

	_, err = e.client.Put(ctx, key, string(value), client.WithLease(leaseID))
	e.leaseManager.CancelIfExpired(err, leaseID)

	// Using lr.Error for convenience, as it matches lr.Done() when err is nil
	lr.Error(err, -1)
	return Hint(err)
}

// UpdateIfDifferentIfLocked updates a key if the value is different and if the client is still holding the given lock.
func (e *etcdClient) UpdateIfDifferentIfLocked(ctx context.Context, key string, value []byte, lease bool, lock KVLocker) (recreated bool, err error) {
	if traceEnabled {
		defer func() {
			Trace(e.logger, "UpdateIfDifferentIfLocked",
				logfields.Error, err,
				fieldKey, key,
				fieldValue, string(value),
				fieldAttachLease, lease,
				fieldRecreated, recreated,
			)
		}()
	}
	lr, err := e.limiter.Wait(ctx)
	if err != nil {
		return false, Hint(err)
	}
	duration := spanstat.Start()

	cnds := lock.Comparator().(client.Cmp)
	txnresp, err := e.client.Txn(ctx).If(cnds).Then(client.OpGet(key)).Commit()
	// Using lr.Error for convenience, as it matches lr.Done() when err is nil
	lr.Error(err, -1)
	increaseMetric(key, metricRead, "Get", duration.EndError(err).Total(), err)

	// On error, attempt update blindly
	if err != nil {
		return true, e.UpdateIfLocked(ctx, key, value, lease, lock)
	}

	if !txnresp.Succeeded {
		return false, ErrLockLeaseExpired
	}

	getR := txnresp.Responses[0].GetResponseRange()
	if getR.Count == 0 {
		return true, e.UpdateIfLocked(ctx, key, value, lease, lock)
	}

	if lease && !e.leaseManager.KeyHasLease(key, client.LeaseID(getR.Kvs[0].Lease)) {
		return true, e.UpdateIfLocked(ctx, key, value, lease, lock)
	}
	// if value is not equal then update.
	if !bytes.Equal(getR.Kvs[0].Value, value) {
		return true, e.UpdateIfLocked(ctx, key, value, lease, lock)
	}

	return false, nil
}

// UpdateIfDifferent updates a key if the value is different
func (e *etcdClient) UpdateIfDifferent(ctx context.Context, key string, value []byte, lease bool) (recreated bool, err error) {
	if traceEnabled {
		defer func() {
			Trace(e.logger, "UpdateIfDifferent",
				logfields.Error, err,
				fieldKey, key,
				fieldValue, string(value),
				fieldAttachLease, lease,
				fieldRecreated, recreated,
			)
		}()
	}
	lr, err := e.limiter.Wait(ctx)
	if err != nil {
		return false, Hint(err)
	}
	duration := spanstat.Start()

	getR, err := e.client.Get(ctx, key)
	// Using lr.Error for convenience, as it matches lr.Done() when err is nil
	lr.Error(err, -1)
	increaseMetric(key, metricRead, "Get", duration.EndError(err).Total(), err)
	// On error, attempt update blindly
	if err != nil || getR.Count == 0 {
		return true, e.Update(ctx, key, value, lease)
	}
	if lease && !e.leaseManager.KeyHasLease(key, client.LeaseID(getR.Kvs[0].Lease)) {
		return true, e.Update(ctx, key, value, lease)
	}
	// if value is not equal then update.
	if !bytes.Equal(getR.Kvs[0].Value, value) {
		return true, e.Update(ctx, key, value, lease)
	}

	return false, nil
}

// CreateOnlyIfLocked atomically creates a key if the client is still holding the given lock or fails if it already exists
func (e *etcdClient) CreateOnlyIfLocked(ctx context.Context, key string, value []byte, lease bool, lock KVLocker) (success bool, err error) {
	if traceEnabled {
		defer func() {
			Trace(e.logger, "CreateOnlyIfLocked",
				logfields.Error, err,
				fieldKey, key,
				fieldValue, string(value),
				fieldAttachLease, lease,
				fieldSuccess, success,
			)
		}()
	}
	var leaseID client.LeaseID
	if lease {
		leaseID, err = e.leaseManager.GetLeaseID(ctx, key)
		if err != nil {
			return false, Hint(err)
		}
	}
	lr, err := e.limiter.Wait(ctx)
	if err != nil {
		return false, Hint(err)
	}
	duration := spanstat.Start()

	req := client.OpPut(key, string(value), client.WithLease(leaseID))
	cnds := []client.Cmp{
		client.Compare(client.Version(key), "=", 0),
		lock.Comparator().(client.Cmp),
	}

	// We need to do a get in the else of the txn to detect if the lock is still
	// valid or not.
	opGets := []client.Op{
		client.OpGet(key),
	}
	txnresp, err := e.client.Txn(ctx).If(cnds...).Then(req).Else(opGets...).Commit()
	increaseMetric(key, metricSet, "CreateOnlyLocked", duration.EndError(err).Total(), err)
	if err != nil {
		lr.Error(err, -1)
		e.leaseManager.CancelIfExpired(err, leaseID)
		return false, Hint(err)
	}
	lr.Done()

	// The txn can failed for the following reasons:
	//  - Key version is not zero;
	//  - Lock does not exist or is expired.
	// For both of those cases, the key that we are comparing might or not
	// exist, so we have:
	//  A - Key does not exist and lock does not exist => ErrLockLeaseExpired
	//  B - Key does not exist and lock exist => txn should succeed
	//  C - Key does exist, version is == 0 and lock does not exist => ErrLockLeaseExpired
	//  D - Key does exist, version is != 0 and lock does not exist => ErrLockLeaseExpired
	//  E - Key does exist, version is == 0 and lock does exist => txn should succeed
	//  F - Key does exist, version is != 0 and lock does exist => txn fails but returned is nil!

	if !txnresp.Succeeded {
		// case F
		if len(txnresp.Responses[0].GetResponseRange().Kvs) != 0 &&
			txnresp.Responses[0].GetResponseRange().Kvs[0].Version != 0 {
			return false, nil
		}

		// case A, C and D
		return false, ErrLockLeaseExpired
	}

	// case B and E
	return true, nil
}

// CreateOnly creates a key with the value and will fail if the key already exists
func (e *etcdClient) CreateOnly(ctx context.Context, key string, value []byte, lease bool) (success bool, err error) {
	if traceEnabled {
		defer func() {
			Trace(e.logger, "CreateOnly",
				logfields.Error, err,
				fieldKey, key,
				fieldValue, string(value),
				fieldAttachLease, lease,
				fieldSuccess, success,
			)
		}()
	}
	var leaseID client.LeaseID
	if lease {
		leaseID, err = e.leaseManager.GetLeaseID(ctx, key)
		if err != nil {
			return false, Hint(err)
		}
	}
	lr, err := e.limiter.Wait(ctx)
	if err != nil {
		return false, Hint(err)
	}
	defer func(duration *spanstat.SpanStat) {
		increaseMetric(key, metricSet, "CreateOnly", duration.EndError(err).Total(), err)
	}(spanstat.Start())

	req := client.OpPut(key, string(value), client.WithLease(leaseID))
	cond := client.Compare(client.Version(key), "=", 0)

	txnresp, err := e.client.Txn(ctx).If(cond).Then(req).Commit()

	if err != nil {
		lr.Error(err, -1)
		e.leaseManager.CancelIfExpired(err, leaseID)
		return false, Hint(err)
	}

	lr.Done()
	return txnresp.Succeeded, nil
}

// ListPrefixIfLocked returns a list of keys matching the prefix only if the client is still holding the given lock.
func (e *etcdClient) ListPrefixIfLocked(ctx context.Context, prefix string, lock KVLocker) (v KeyValuePairs, err error) {
	if traceEnabled {
		defer func() {
			Trace(e.logger, "ListPrefixIfLocked",
				logfields.Error, err,
				fieldPrefix, prefix,
				fieldNumEntries, len(v),
			)
		}()
	}
	lr, err := e.limiter.Wait(ctx)
	if err != nil {
		return nil, Hint(err)
	}
	defer func(duration *spanstat.SpanStat) {
		increaseMetric(prefix, metricRead, "ListPrefixLocked", duration.EndError(err).Total(), err)
	}(spanstat.Start())

	opGet := client.OpGet(prefix, client.WithPrefix())
	cmp := lock.Comparator().(client.Cmp)
	txnReply, err := e.client.Txn(ctx).If(cmp).Then(opGet).Commit()
	if err == nil && !txnReply.Succeeded {
		err = ErrLockLeaseExpired
	}
	if err != nil {
		lr.Error(err, -1)
		return nil, Hint(err)
	}
	lr.Done()
	getR := txnReply.Responses[0].GetResponseRange()

	pairs := KeyValuePairs(make(map[string]Value, getR.Count))
	for i := int64(0); i < getR.Count; i++ {
		pairs[string(getR.Kvs[i].Key)] = Value{
			Data:        getR.Kvs[i].Value,
			ModRevision: uint64(getR.Kvs[i].ModRevision),
		}

	}

	return pairs, nil
}

// ListPrefix returns a map of matching keys
func (e *etcdClient) ListPrefix(ctx context.Context, prefix string) (v KeyValuePairs, err error) {
	if traceEnabled {
		defer func() {
			Trace(e.logger, "ListPrefix",
				logfields.Error, err,
				fieldPrefix, prefix,
				fieldNumEntries, len(v),
			)
		}()
	}
	lr, err := e.limiter.Wait(ctx)
	if err != nil {
		return nil, Hint(err)
	}
	defer func(duration *spanstat.SpanStat) {
		increaseMetric(prefix, metricRead, "ListPrefix", duration.EndError(err).Total(), err)
	}(spanstat.Start())

	getR, err := e.client.Get(ctx, prefix, client.WithPrefix())
	if err != nil {
		lr.Error(err, -1)
		return nil, Hint(err)
	}
	lr.Done()

	pairs := KeyValuePairs(make(map[string]Value, getR.Count))
	for i := int64(0); i < getR.Count; i++ {
		pairs[string(getR.Kvs[i].Key)] = Value{
			Data:        getR.Kvs[i].Value,
			ModRevision: uint64(getR.Kvs[i].ModRevision),
			LeaseID:     getR.Kvs[i].Lease,
		}

	}

	return pairs, nil
}

// Close closes the etcd session
func (e *etcdClient) Close() {
	close(e.stopStatusChecker)

	if err := e.client.Close(); err != nil {
		e.logger.Warn(
			"Failed to close etcd client",
			logfields.Error, err,
		)
	}

	// Wait until all child goroutines spawned by the lease managers have terminated.
	e.leaseManager.Wait()
	e.lockLeaseManager.Wait()
}

// ListAndWatch implements the BackendOperations.ListAndWatch using etcd
func (e *etcdClient) ListAndWatch(ctx context.Context, prefix string) EventChan {
	events := make(chan KeyValueEvent)

	go e.watch(ctx, prefix, emitter{events: events, scope: GetScopeFromKey(strings.TrimRight(prefix, "/"))})

	return events
}

// RegisterLeaseExpiredObserver registers a function which is executed when
// the lease associated with a key having the given prefix is detected as expired.
// If the function is nil, the previous observer (if any) is unregistered.
func (e *etcdClient) RegisterLeaseExpiredObserver(prefix string, fn func(key string)) {
	if fn == nil {
		e.leaseExpiredObservers.Delete(prefix)
	} else {
		e.leaseExpiredObservers.Store(prefix, fn)
	}
}

func (e *etcdClient) expiredLeaseObserver(key string) {
	e.leaseExpiredObservers.Range(func(prefix string, fn func(string)) bool {
		if strings.HasPrefix(key, prefix) {
			fn(key)
		}
		return true
	})
}

// UserEnforcePresence creates a user in etcd if not already present, and grants the specified roles.
func (e *etcdClient) UserEnforcePresence(ctx context.Context, name string, roles []string) error {
	e.logger.Debug("Creating user", FieldUser, name)
	_, err := e.client.Auth.UserAddWithOptions(ctx, name, "", &client.UserAddOptions{NoPassword: true})
	if err != nil {
		if errors.Is(err, v3rpcErrors.ErrUserAlreadyExist) {
			e.logger.Debug("User already exists", FieldUser, name)
		} else {
			return err
		}
	}

	for _, role := range roles {
		e.logger.Debug("Granting role to user",
			FieldRole, role,
			FieldUser, name,
		)

		_, err := e.client.Auth.UserGrantRole(ctx, name, role)
		if err != nil {
			return err
		}
	}

	return nil
}

// UserEnforcePresence deletes a user from etcd, if present.
func (e *etcdClient) UserEnforceAbsence(ctx context.Context, name string) error {
	e.logger.Debug("Deleting user", FieldUser, name)
	_, err := e.client.Auth.UserDelete(ctx, name)
	if err != nil {
		if errors.Is(err, v3rpcErrors.ErrUserNotFound) {
			e.logger.Debug("User not found", FieldUser, name)
		} else {
			return err
		}
	}

	return nil
}

// reload on-disk certificate and key when needed
func getClientCertificateReloader(fpath string) (func(*tls.CertificateRequestInfo) (*tls.Certificate, error), error) {
	yc := &yamlKeyPairConfig{}
	b, err := os.ReadFile(fpath)
	if err != nil {
		return nil, err
	}
	err = yaml.Unmarshal(b, yc)
	if err != nil {
		return nil, err
	}
	if yc.Certfile == "" || yc.Keyfile == "" {
		return nil, nil
	}
	reloader := func(_ *tls.CertificateRequestInfo) (*tls.Certificate, error) {
		cer, err := tls.LoadX509KeyPair(yc.Certfile, yc.Keyfile)
		return &cer, err
	}
	return reloader, nil
}

// copy of relevant internal structure fields in go.etcd.io/etcd/clientv3/yaml
// needed to implement certificates reload, not depending on the deprecated
// newconfig/yamlConfig.
type yamlKeyPairConfig struct {
	Certfile string `json:"cert-file"`
	Keyfile  string `json:"key-file"`
}
