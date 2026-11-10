// Package proxy contains the data-plane server: identity resolution, route
// matching, policy enforcement, and reverse proxying (ADR-0001 §5).
package proxy

import (
	"context"
	"net"
	"sync"
	"time"
)

// dnsCache caches upstream host lookups for a TTL (ADR-0001 §13
// ROUTEGATE_UPSTREAM_DNS_TTL). It keeps connection reuse (keep-alive) while
// still re-balancing to restarted or scaled replicas within the TTL — the
// property the legacy per-request resolution provided, at far lower cost.
type dnsCache struct {
	mu    sync.Mutex
	ttl   time.Duration
	byKey map[string]dnsEntry
}

type dnsEntry struct {
	addrs   []string
	fetched time.Time
}

func newDNSCache(ttl time.Duration) *dnsCache {
	return &dnsCache{ttl: ttl, byKey: map[string]dnsEntry{}}
}

// lookup returns cached addresses for host, refreshing on miss or expiry. On
// lookup failure a still-cached (stale) entry is returned if present.
func (c *dnsCache) lookup(ctx context.Context, resolver *net.Resolver, host string) ([]string, error) {
	c.mu.Lock()
	e, ok := c.byKey[host]
	fresh := ok && time.Since(e.fetched) < c.ttl
	c.mu.Unlock()
	if fresh {
		return e.addrs, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := resolver.LookupHost(ctx, host)
	if err != nil {
		if ok {
			return e.addrs, nil // stale on failure
		}
		return nil, err
	}
	c.mu.Lock()
	c.byKey[host] = dnsEntry{addrs: addrs, fetched: time.Now()}
	c.mu.Unlock()
	return addrs, nil
}
