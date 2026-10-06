package main

import (
	"context"
	"fmt"
	"log"
	"logwolf-toolbox/data"
	"logwolf-toolbox/limits"
	"net/http"
	"sync"
	"time"
)

// --- Monthly event quota ---
// An organization's projects may ingest, between them, its plan's
// MonthlyEvents in a calendar month (UTC). The broker keeps a counter per
// organization: the events the logger had counted for the month at the last
// lookup (limits.Provider.MonthlyQuota), plus those this broker has accepted
// since. Events that would take the counter past the limit are refused whole,
// before anything is queued, with 429, the code quota_exceeded and a
// Retry-After until the month is over. Self-hosted has no quota and looks
// nothing up.
//
// The counter can trail what the organization really ingested, never lead it:
// the logger has only what the brokers have flushed (see usageMeter), and a
// lookup never lowers the counter within a month. One broker sees everything
// it accepted, so with one the quota holds exactly. With more, each misses
// what the others have not flushed yet, and the organization can go past its
// quota by up to USAGE_FLUSH_INTERVAL of ingestion through each other broker,
// which the keys' rates bound in turn.

// quotaTTL is how long a project's quota stands before the provider is asked
// again: how soon a plan change, or what other brokers have flushed, reaches
// this one. It also bounds the lookups: one per project per TTL.
const quotaTTL = time.Minute

// quotaIdle is how long a project's quota is kept without a request. It is
// well past USAGE_FLUSH_INTERVAL's default, so by then everything this broker
// counted has been flushed, and a lookup has it all.
const quotaIdle = 10 * time.Minute

// quotaEntry is which organization's counter a project's events go to.
type quotaEntry struct {
	// orgID is the organization; empty when the project has no quota.
	orgID string
	// checkedAt is when the provider was last asked, successfully or not.
	checkedAt time.Time
	// usedAt is when a request last came for the project.
	usedAt time.Time
}

// quotaCounter is an organization's monthly quota as this broker counts it.
type quotaCounter struct {
	// limit is the plan's MonthlyEvents; limits.Unlimited is none.
	limit int64
	// month is the month used counts, as data.UsageMonth.
	month time.Time
	// used is the events counted against the month.
	used int64
}

var (
	quotaProjects = make(map[string]*quotaEntry)
	quotaCounters = make(map[string]*quotaCounter)
	quotaMu       sync.Mutex

	// maxQuotaProjects caps quotaProjects, like the ingest buckets. A project
	// whose entry is evicted is looked up again.
	maxQuotaProjects = 10_000
)

// quotaReservation is n events counted against an organization's month, which
// releaseQuota takes back if they are not queued after all. The zero value
// counted nothing.
type quotaReservation struct {
	orgID string
	month time.Time
	n     int64
}

// reserveQuota counts n events of the project against its organization's
// monthly quota, if they fit. When they do not, it counts nothing and says how
// long until the quota renews.
//
// It asks the provider when the broker has no quota for the project, or has
// had it for quotaTTL. The error is a quota that could not be looked up for a
// project with none yet; a project that has one keeps it until a lookup
// succeeds.
func (app *Config) reserveQuota(ctx context.Context, projectID string, n int, now time.Time) (quotaReservation, bool, time.Duration, error) {
	quotaMu.Lock()
	if p, ok := quotaProjects[projectID]; ok && now.Sub(p.checkedAt) < quotaTTL {
		defer quotaMu.Unlock()
		res, allowed, wait := takeQuota(p, int64(n), now)
		return res, allowed, wait, nil
	}
	quotaMu.Unlock()

	// The lookup may be an RPC to the logger: not under the lock.
	q, err := app.limitsProvider().MonthlyQuota(ctx, projectID)

	quotaMu.Lock()
	defer quotaMu.Unlock()
	p, ok := quotaProjects[projectID]
	switch {
	case err != nil && !ok:
		return quotaReservation{}, false, 0, err
	case err != nil:
		// Try again after another TTL, not on every request.
		log.Printf(`{"event":"quota","outcome":"error","reason":"lookup","project_id":"%s","error":%q}`, projectID, err.Error())
		p.checkedAt = now
	default:
		p = learnQuota(projectID, q, now)
	}
	res, allowed, wait := takeQuota(p, int64(n), now)
	return res, allowed, wait, nil
}

// learnQuota files what the provider said of a project's quota: which
// organization's counter it goes to, and that counter's limit and month. The
// caller holds quotaMu.
func learnQuota(projectID string, q limits.Quota, now time.Time) *quotaEntry {
	p, ok := quotaProjects[projectID]
	if !ok {
		if len(quotaProjects) >= maxQuotaProjects {
			for k := range quotaProjects {
				delete(quotaProjects, k)
				break
			}
		}
		p = &quotaEntry{}
		quotaProjects[projectID] = p
	}
	p.orgID, p.checkedAt = q.OrganizationID, now
	if p.orgID == "" {
		return p
	}

	c, ok := quotaCounters[p.orgID]
	if !ok {
		c = &quotaCounter{month: q.Month, used: q.Used}
		quotaCounters[p.orgID] = c
	}
	c.limit = q.Limit
	switch {
	case q.Month.After(c.month):
		c.month, c.used = q.Month, q.Used
	case q.Month.Equal(c.month):
		// The logger's count has what every broker flushed; this one's has
		// what it accepted since the last lookup. Each falls short of the
		// truth, so the larger is the better count.
		c.used = max(c.used, q.Used)
	}
	return p
}

// takeQuota counts n events against the counter of p's organization, unless
// that takes it past the limit. The caller holds quotaMu.
func takeQuota(p *quotaEntry, n int64, now time.Time) (quotaReservation, bool, time.Duration) {
	p.usedAt = now
	c := quotaCounters[p.orgID]
	if p.orgID == "" || c == nil || n == 0 {
		return quotaReservation{}, true, 0
	}

	// The month turned since the counter last heard from the logger: the
	// quota renews, whatever the next lookup says of the month before.
	month := data.UsageMonth(now)
	if month.After(c.month) {
		c.month, c.used = month, 0
	}

	if c.limit == limits.Unlimited {
		return quotaReservation{}, true, 0
	}
	if c.used+n > c.limit {
		return quotaReservation{}, false, data.NextUsageMonth(now).Sub(now)
	}
	c.used += n
	return quotaReservation{orgID: p.orgID, month: c.month, n: n}, true, 0
}

// releaseQuota takes back events reserveQuota counted that were not queued.
func releaseQuota(res quotaReservation) {
	if res.n == 0 {
		return
	}
	quotaMu.Lock()
	defer quotaMu.Unlock()
	if c, ok := quotaCounters[res.orgID]; ok && c.month.Equal(res.month) {
		c.used = max(0, c.used-res.n)
	}
}

// sweepQuotas forgets the projects no request has come for in quotaIdle, and
// the counters of organizations none of the remaining projects is in.
func sweepQuotas(now time.Time) {
	quotaMu.Lock()
	defer quotaMu.Unlock()

	inUse := make(map[string]bool, len(quotaCounters))
	for k, p := range quotaProjects {
		if now.Sub(p.usedAt) >= quotaIdle {
			delete(quotaProjects, k)
			continue
		}
		inUse[p.orgID] = true
	}
	for org := range quotaCounters {
		if !inUse[org] {
			delete(quotaCounters, org)
		}
	}
}

// limitQuota holds n events for the project to its organization's monthly
// quota. It answers the request and returns false when they may not be queued:
// 429 with the code quota_exceeded and a Retry-After over the quota, 500 when
// the quota could not be told. A caller that then fails to queue the events
// gives the reservation back with releaseQuota.
func (app *Config) limitQuota(w http.ResponseWriter, r *http.Request, projectID string, n int) (quotaReservation, bool) {
	res, allowed, wait, err := app.reserveQuota(r.Context(), projectID, n, time.Now())
	if err != nil {
		log.Printf(`{"event":"quota","outcome":"error","reason":"lookup","project_id":"%s","error":%q}`, projectID, err.Error())
		app.errorJSON(w, fmt.Errorf("could not check the monthly event quota, try again"), http.StatusInternalServerError)
		return quotaReservation{}, false
	}
	if !allowed {
		log.Printf(`{"event":"quota","outcome":"deny","project_id":"%s","count":%d,"retry_after_s":%d}`, projectID, n, int64(wait.Seconds()))
		setRetryAfter(w, wait)
		app.errorCodeJSON(w, fmt.Errorf("the organization of this project has used its monthly event quota"), http.StatusTooManyRequests, codeQuotaExceeded)
		return quotaReservation{}, false
	}
	return res, true
}
