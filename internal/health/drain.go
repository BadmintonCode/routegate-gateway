package health

import "sync/atomic"

// Drain is the process-drain flag shared between the shutdown sequence and
// the health listener. Begin is called on SIGTERM: /__ready then answers 503
// immediately so the load balancer removes this instance before the listeners
// stop, while in-flight requests finish under the configured grace
// (ADR-0001 §14). Liveness (/__health) deliberately stays 200 — draining is
// not a reason for the orchestrator to kill the process.
type Drain struct {
	v atomic.Bool
}

// Begin marks the process as draining.
func (d *Drain) Begin() { d.v.Store(true) }

// Active reports whether draining has begun.
func (d *Drain) Active() bool { return d.v.Load() }
