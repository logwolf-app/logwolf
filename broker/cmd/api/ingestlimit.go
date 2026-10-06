package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// --- Ingestion rate limiter ---
// A token bucket per API key, sized from the plan of the key's project
// (limits.Plan.IngestRate and IngestBurst): each event takes a token, and the
// bucket refills at the plan's rate up to its burst. A request whose events do
// not fit is refused whole with 429 and a Retry-After.
//
// The buckets live in this broker's memory, so each replica holds a key to the
// rate on its own. With more than one, they move to shared storage.

// ingestPlanTTL is how long a bucket keeps the size it was given before the
// plan is asked again, so a plan change reaches a busy key within it. It also
// bounds the plan lookups: one per key per TTL, not one per request.
const ingestPlanTTL = time.Minute

type ingestBucket struct {
	// rate is tokens per second and burst the most the bucket holds. A rate of
	// 0 is a plan without a limit: the bucket only remembers that, so the plan
	// is not asked again until sizedAt + ingestPlanTTL.
	rate, burst float64
	tokens      float64
	// last is when tokens was last brought up to date.
	last time.Time
	// sizedAt is when the plan last set rate and burst.
	sizedAt time.Time
}

var (
	ingestBuckets   = make(map[string]*ingestBucket)
	ingestBucketsMu sync.Mutex

	// maxIngestBuckets caps ingestBuckets, like the key cache it follows. A key
	// whose bucket is evicted starts again from a full one.
	maxIngestBuckets = 10_000
)

// newIngestBucket is a full bucket of the given size.
func newIngestBucket(rate, burst int, now time.Time) *ingestBucket {
	return &ingestBucket{rate: float64(rate), burst: float64(burst), tokens: float64(burst), last: now, sizedAt: now}
}

// refill adds the tokens earned since b.last, up to the burst.
func (b *ingestBucket) refill(now time.Time) {
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(b.burst, b.tokens+elapsed*b.rate)
	}
	b.last = now
}

// resize gives b the plan's rate and burst, keeping the tokens it has earned
// under the old ones, up to the new burst.
func (b *ingestBucket) resize(rate, burst int, now time.Time) {
	b.refill(now)
	b.rate, b.burst = float64(rate), float64(burst)
	b.tokens = math.Min(b.tokens, b.burst)
	b.sizedAt = now
}

// take spends n tokens if the bucket has them. If not, it spends none and
// returns how long until it will. A request of more events than the bucket
// holds needs a full one, and empties it, rather than being refused forever.
func (b *ingestBucket) take(n int, now time.Time) (bool, time.Duration) {
	if b.rate == 0 {
		return true, 0
	}
	b.refill(now)
	cost := math.Min(float64(n), b.burst)
	if b.tokens >= cost {
		b.tokens -= cost
		return true, 0
	}
	return false, time.Duration((cost - b.tokens) / b.rate * float64(time.Second))
}

// full reports whether b would be full by now: forgetting it changes nothing
// but the plan lookup the next request makes.
func (b *ingestBucket) full(now time.Time) bool {
	return b.rate == 0 || b.tokens+now.Sub(b.last).Seconds()*b.rate >= b.burst
}

// allowIngest takes n events from the bucket of the key keyID, of the project
// projectID, sizing the bucket from the project's plan when it has none or its
// size is older than ingestPlanTTL. When it refuses, it says how long until
// the events would fit.
//
// The error is a plan that could not be looked up for a key with no bucket. A
// key that has one keeps its old size until a lookup succeeds.
func (app *Config) allowIngest(ctx context.Context, keyID, projectID string, n int) (bool, time.Duration, error) {
	now := time.Now()

	ingestBucketsMu.Lock()
	if b, ok := ingestBuckets[keyID]; ok && now.Sub(b.sizedAt) < ingestPlanTTL {
		allowed, wait := b.take(n, now)
		ingestBucketsMu.Unlock()
		return allowed, wait, nil
	}
	ingestBucketsMu.Unlock()

	// The lookup may be an RPC to the logger: not under the lock.
	plan, err := app.limitsProvider().Plan(ctx, projectID)

	ingestBucketsMu.Lock()
	defer ingestBucketsMu.Unlock()
	b, ok := ingestBuckets[keyID]
	switch {
	case err != nil && !ok:
		return false, 0, err
	case err != nil:
		// Try the plan again after another TTL, not on every request.
		log.Printf(`{"event":"ingest_limit","outcome":"error","reason":"plan_lookup","project_id":"%s","error":%q}`, projectID, err.Error())
		b.sizedAt = now
	case ok:
		b.resize(plan.IngestRate, plan.IngestBurst, now)
	default:
		if len(ingestBuckets) >= maxIngestBuckets {
			for k := range ingestBuckets {
				delete(ingestBuckets, k)
				break
			}
		}
		b = newIngestBucket(plan.IngestRate, plan.IngestBurst, now)
		ingestBuckets[keyID] = b
	}
	allowed, wait := b.take(n, now)
	return allowed, wait, nil
}

// limitIngest holds n events sent with the request's API key to the key's
// rate. It answers the request and returns false when they may not be queued:
// 429 with a Retry-After over the rate, 500 when the plan could not be told.
func (app *Config) limitIngest(w http.ResponseWriter, r *http.Request, n int) bool {
	keyID, projectID := keyIDFromContext(r), projectIDFromContext(r)
	allowed, wait, err := app.allowIngest(r.Context(), keyID, projectID, n)
	if err != nil {
		log.Printf(`{"event":"ingest_limit","outcome":"error","reason":"plan_lookup","project_id":"%s","error":%q}`, projectID, err.Error())
		app.errorJSON(w, fmt.Errorf("could not check the ingestion limit, try again"), http.StatusInternalServerError)
		return false
	}
	if !allowed {
		log.Printf(`{"event":"ingest_limit","outcome":"deny","project_id":"%s","count":%d,"retry_after_ms":%d}`, projectID, n, wait.Milliseconds())
		setRetryAfter(w, wait)
		app.errorJSON(w, fmt.Errorf("ingestion rate exceeded for this API key"), http.StatusTooManyRequests)
		return false
	}
	return true
}

// setRetryAfter sets Retry-After to wait in whole seconds, rounded up and at
// least 1, so a client that waits that long is not refused again.
func setRetryAfter(w http.ResponseWriter, wait time.Duration) {
	secs := int64(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
}
