package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"logwolf-toolbox/data"
	"net"
	"net/rpc"
	"os"
	"sync"
	"time"
)

// --- Usage metering ---
// The broker counts the events it accepts, and their bytes, per project per
// hour: at the point where it answers 202, so a refused or unqueued event is
// never counted. It has no database, so it keeps the counts in memory and
// flushes them to the logger (RPCServer.RecordUsage) every usageFlushInterval,
// and once more on shutdown, after the last request has drained.
//
// A flush sends the running totals of the hours that changed since the last
// one, under this run's source, and the logger keeps the larger of what it has
// and what it is sent (see data.RecordUsage). So a failed flush is simply sent
// again by the next one, and one that succeeded without the broker hearing so
// counts nothing twice.
//
// The loss window: a broker that is killed, or crashes, loses what it counted
// since its last flush, at most usageFlushInterval of events. One stopped
// cleanly loses nothing, unless the logger cannot be reached for the final
// flush; then it loses everything not flushed since the logger went away.

// usageFlushTimeout bounds one flush, dial included.
const usageFlushTimeout = 10 * time.Second

// usageFlushInterval is how often the counts are flushed, from
// USAGE_FLUSH_INTERVAL; a minute by default. It is the most a broker that dies
// without warning loses.
func usageFlushInterval() time.Duration {
	if s := os.Getenv("USAGE_FLUSH_INTERVAL"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			log.Printf("Warning: invalid USAGE_FLUSH_INTERVAL %q, using default 1m", s)
		} else {
			return d
		}
	}
	return time.Minute
}

// usageSource names this broker run in the usage buckets: the host, which says
// which replica it was, and a random suffix, which tells a restarted broker
// from the run before it. Each run's buckets hold its own totals.
func usageSource() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "broker"
	}
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		log.Panicf("usage source: %v", err)
	}
	return host + "-" + hex.EncodeToString(b[:])
}

type usageKey struct {
	projectID string
	hour      time.Time
}

// usageCount is a project's running total for one hour, and the part of it the
// logger is known to have.
type usageCount struct {
	events, bytes         int64
	sentEvents, sentBytes int64
}

func (c *usageCount) dirty() bool {
	return c.events != c.sentEvents || c.bytes != c.sentBytes
}

// usageSender sends a flush to the logger: recordUsage, or a fake in tests.
type usageSender func(ctx context.Context, args *data.RPCRecordUsageArgs) error

// usageMeter is the broker's count of accepted events. A nil one counts
// nothing.
type usageMeter struct {
	source string
	now    func() time.Time

	mu     sync.Mutex
	counts map[usageKey]*usageCount

	// flushMu keeps one flush at a time: the shutdown flush may start while
	// the loop's last one is still running.
	flushMu sync.Mutex
}

func newUsageMeter(source string) *usageMeter {
	return &usageMeter{source: source, now: time.Now, counts: make(map[usageKey]*usageCount)}
}

// record counts events accepted for a project, bytes long all together, in
// the current hour.
func (m *usageMeter) record(projectID string, events, bytes int64) {
	if m == nil || events == 0 {
		return
	}
	key := usageKey{projectID: projectID, hour: data.UsageHour(m.now())}

	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.counts[key]
	if !ok {
		c = &usageCount{}
		m.counts[key] = c
	}
	c.events += events
	c.bytes += bytes
}

// flush sends the totals of every hour that changed since the last successful
// flush. Once the logger has them, it forgets the hours that are over: no event
// can be counted in them any more. When send fails it keeps everything, and the
// next flush sends it again.
func (m *usageMeter) flush(ctx context.Context, send usageSender) error {
	if m == nil {
		return nil
	}
	m.flushMu.Lock()
	defer m.flushMu.Unlock()

	m.mu.Lock()
	var counts []data.RPCUsageCount
	for k, c := range m.counts {
		if c.dirty() {
			counts = append(counts, data.RPCUsageCount{ProjectID: k.projectID, Hour: k.hour, Events: c.events, Bytes: c.bytes})
		}
	}
	m.mu.Unlock()

	if len(counts) > 0 {
		if err := send(ctx, &data.RPCRecordUsageArgs{Source: m.source, Counts: counts}); err != nil {
			return err
		}
	}

	current := data.UsageHour(m.now())
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sent := range counts {
		// Events counted while the flush was out stay dirty, for the next one.
		c := m.counts[usageKey{projectID: sent.ProjectID, hour: sent.Hour}]
		c.sentEvents, c.sentBytes = sent.Events, sent.Bytes
	}
	for k, c := range m.counts {
		if k.hour.Before(current) && !c.dirty() {
			delete(m.counts, k)
		}
	}
	return nil
}

// run flushes every interval until ctx is done. The final flush, once the
// requests have drained, is the caller's.
func (m *usageMeter) run(ctx context.Context, interval time.Duration, send usageSender) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.flushLogged(ctx, send)
		}
	}
}

// flushLogged flushes under usageFlushTimeout, and logs a failure.
func (m *usageMeter) flushLogged(ctx context.Context, send usageSender) {
	ctx, cancel := context.WithTimeout(ctx, usageFlushTimeout)
	defer cancel()
	if err := m.flush(ctx, send); err != nil {
		log.Printf(`{"event":"usage_flush","outcome":"error","source":%q,"error":%q}`, m.source, err.Error())
	}
}

// recordUsage is the usageSender of a running broker: it sends the flush to the
// logger over a connection of its own, and gives up when ctx is done.
func recordUsage(ctx context.Context, args *data.RPCRecordUsageArgs) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", loggerRPCAddr())
	if err != nil {
		return err
	}
	client := rpc.NewClient(conn)
	defer client.Close()

	var reply string
	call := client.Go("RPCServer.RecordUsage", args, &reply, nil)
	select {
	case <-call.Done:
		return call.Error
	case <-ctx.Done():
		return ctx.Err()
	}
}
