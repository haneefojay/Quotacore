package store

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Options configure the data-store pool. The pool itself is go-redis's: a
// bounded set of connections shared by the instance, sized by the library
// default. DataScriptTimeout is the value of QUOTACORE_DATASCRIPT_TIMEOUT, the
// hard bound on a script execution (NFR-L4); IP-05 owns the script call that
// spends it, this phase owns carrying it on the connection.
type Options struct {
	URL               string
	DataScriptTimeout time.Duration
}

// Store is the data plane's handle on the fast datastore. It holds the client
// and the pool; it performs no enforcement and decides nothing.
type Store struct {
	client  *redis.Client
	timeout time.Duration
}

// Open builds a Store from a URL, or returns an error if the URL cannot parse.
// It does not dial: the first command dials, so a datastore that is down at
// startup degrades the service to not-ready rather than preventing start, which
// is the NFR-A3 fail-closed shape (fail at request time, fast, and without
// terminating after a competing readiness signal).
func Open(opts Options) (*Store, error) {
	base, err := redis.ParseURL(opts.URL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	return &Store{
		client:  redis.NewClient(base),
		timeout: opts.DataScriptTimeout,
	}, nil
}

// Client exposes the underlying go-redis client to the packages that need it.
// The data plane uses it for bounded single-key commands; the script layer in
// IP-05 uses it for a script call inside DataScriptTimeout.
func (s *Store) Client() *redis.Client { return s.client }

// DataScriptTimeout returns the hard bound on one script execution, carried so
// the caller needs no second source for it.
func (s *Store) DataScriptTimeout() time.Duration { return s.timeout }

// Ping checks the connection without carrying state. Failures here feed the
// readiness term, not the request path.
func (s *Store) Ping(ctx context.Context) error { return s.client.Ping(ctx).Err() }

// Close releases the pool.
func (s *Store) Close() error { return s.client.Close() }

// ConfigVersion reads `qc:cfg:version`, the monotonically increasing
// configuration version the subscriber compares an invalidation message to. An
// absent key is version zero, meaning "no configuration published yet".
func (s *Store) ConfigVersion(ctx context.Context) (int64, error) {
	v, err := s.client.Get(ctx, ConfigVersionKey()).Result()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read config version: %w", err)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse config version %q: %w", v, err)
	}
	return n, nil
}

// TenantVersions reads `qc:cfg:tenants`, tenant to the version at which that
// tenant last changed. It is one HGETALL of a bounded hash and runs off the
// request path, in the invalidator; it is not a cursor or a scan.
func (s *Store) TenantVersions(ctx context.Context) (map[string]int64, error) {
	raw, err := s.client.HGetAll(ctx, ConfigTenantsKey()).Result()
	if err != nil {
		return nil, fmt.Errorf("read tenant versions: %w", err)
	}
	out := make(map[string]int64, len(raw))
	for tenant, v := range raw {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse tenant version %q: %w", v, err)
		}
		out[tenant] = n
	}
	return out, nil
}

// PublishInvalidation publishes `qc:cfg:invalidate` with a version. A control
// plane write publishes after commit, best-effort (request-lifecycle.md
// section 4); this helper exists so IP-04's tests can drive the channel without
// a control plane, and IP-09 and IP-10 will call it after their committed
// writes.
func (s *Store) PublishInvalidation(ctx context.Context, version int64) error {
	return s.client.Publish(ctx, ConfigInvalidateChannel(), "{\"version\":\""+strconv.FormatInt(version, 10)+"\"}").Err()
}

// InvalidationStream is a subscription to the configuration invalidation
// channel. Messages are delivered as payload strings; using it is the
// subscriber's business (internal/snapshot). Its shape is declared by
// internal/snapshot as an interface, and this type satisfies it without either
// package importing the other.
type InvalidationStream struct {
	pubsub *redis.PubSub
	msgs   <-chan *redis.Message
}

// Messages returns the channel of payloads. go-redis's Channel mechanism
// reconnects internally, so a published message that is missed widens the
// propagation lag but never wedges the subscriber; the periodic tick is the
// correctness backstop (data-model.md section 3.4).
func (s *InvalidationStream) Messages() <-chan string {
	out := make(chan string)
	go func() {
		defer close(out)
		for m := range s.msgs {
			out <- m.Payload
		}
	}()
	return out
}

// Close unsubscribes and releases the connection.
func (s *InvalidationStream) Close() error { return s.pubsub.Close() }

// SubscribeInvalidations opens a subscription to the invalidation channel.
func (s *Store) SubscribeInvalidations(ctx context.Context) (*InvalidationStream, error) {
	ps := s.client.Subscribe(ctx, ConfigInvalidateChannel())
	if _, err := ps.Receive(ctx); err != nil {
		ps.Close()
		return nil, fmt.Errorf("subscribe to invalidation channel: %w", err)
	}
	return &InvalidationStream{pubsub: ps, msgs: ps.Channel()}, nil
}
