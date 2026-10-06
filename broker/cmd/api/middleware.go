package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"logwolf-toolbox/data"
	"net/http"
	"net/rpc"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type contextKey string

const projectIDKey contextKey = "projectID"
const userLoginKey contextKey = "userLogin"
const userIDKey contextKey = "userID"
const keyScopesKey contextKey = "keyScopes"
const keyIDKey contextKey = "keyID"

func projectIDFromContext(r *http.Request) string {
	if v, ok := r.Context().Value(projectIDKey).(string); ok {
		return v
	}
	return ""
}

// keyIDFromContext returns the id of the request's API key, which
// requireAPIKey stored; "" outside the API key routes.
func keyIDFromContext(r *http.Request) string {
	if v, ok := r.Context().Value(keyIDKey).(string); ok {
		return v
	}
	return ""
}

func keyScopesFromContext(r *http.Request) []string {
	if v, ok := r.Context().Value(keyScopesKey).([]string); ok {
		return v
	}
	return nil
}

func userLoginFromContext(r *http.Request) string {
	if v, ok := r.Context().Value(userLoginKey).(string); ok {
		return v
	}
	return ""
}

// userIDFromContext returns the signed-in user's GitHub user ID, which
// requireUserLogin stored; 0 outside the dashboard routes.
func userIDFromContext(r *http.Request) int64 {
	if v, ok := r.Context().Value(userIDKey).(int64); ok {
		return v
	}
	return 0
}

type cacheEntry struct {
	valid bool
	// keyID and projectID name the key a valid entry stands for, so revoking
	// the key or deleting its project can evict it (see forgetCachedKeys).
	keyID     string
	projectID string
	scopes    []string
	expiresAt time.Time
}

var (
	keyCache   = make(map[string]cacheEntry)
	keyCacheMu sync.RWMutex
	cacheTTL   = 60 * time.Second

	// maxKeyCacheEntries caps keyCache. Invalid keys are cached too, so without
	// a cap a flood of made-up keys from many addresses would grow it without
	// bound between two sweeps.
	maxKeyCacheEntries = 10_000

	// keyCacheGen counts forgetCachedKeys calls. A validation that started
	// before one may have read a key the eviction was about, so its result is
	// not cached (see cacheKeyResult). Guarded by keyCacheMu.
	keyCacheGen uint64
)

// keyCacheGeneration returns the current keyCacheGen, to hand to
// cacheKeyResult once the validation it precedes is done.
func keyCacheGeneration() uint64 {
	keyCacheMu.RLock()
	defer keyCacheMu.RUnlock()
	return keyCacheGen
}

// cacheKeyResult stores entry under cacheKey, first evicting an arbitrary
// entry if the cache is full. Evicting a live entry costs one extra RPC the
// next time its key is seen, nothing more.
//
// gen is keyCacheGeneration from before the validation. If keys were evicted
// since, the result may predate a revocation and is dropped: caching it would
// bring a revoked key back for a whole cacheTTL.
func cacheKeyResult(cacheKey string, entry cacheEntry, gen uint64) {
	keyCacheMu.Lock()
	defer keyCacheMu.Unlock()

	if gen != keyCacheGen {
		return
	}
	if _, ok := keyCache[cacheKey]; !ok && len(keyCache) >= maxKeyCacheEntries {
		for k := range keyCache {
			delete(keyCache, k)
			break
		}
	}
	keyCache[cacheKey] = entry
}

// forgetCachedKeys evicts every cached key that match reports true for, so the
// next request with one of them is validated against the logger again. The
// broker calls it once it has revoked a key or deleted a project; without it the
// key kept working for up to cacheTTL.
//
// It only reaches this broker's cache. Another broker replica keeps its entries
// until they expire.
func forgetCachedKeys(match func(cacheEntry) bool) {
	keyCacheMu.Lock()
	defer keyCacheMu.Unlock()

	keyCacheGen++
	for k, e := range keyCache {
		if e.valid && match(e) {
			delete(keyCache, k)
		}
	}
}

// forgetCachedKey evicts the key with the given id.
func forgetCachedKey(keyID string) {
	forgetCachedKeys(func(e cacheEntry) bool { return e.keyID == keyID })
}

// forgetCachedProjectKeys evicts every key of the given project.
func forgetCachedProjectKeys(projectID string) {
	forgetCachedKeys(func(e cacheEntry) bool { return e.projectID == projectID })
}

func hashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// --- IP rate limiter ---
// Sliding-window counter: tracks failed auth attempts per client IP (see
// clientIP).
// After maxFailures within the window, requests are rejected with 429.

const (
	rateLimitWindow = 1 * time.Minute
	maxFailures     = 10
)

type ipEntry struct {
	failures  int
	windowEnd time.Time
}

var (
	ipLimiter   = make(map[string]*ipEntry)
	ipLimiterMu sync.Mutex

	// maxIPLimiterEntries caps ipLimiter, which gains an entry for every
	// address that fails once, whether or not it ever comes back.
	maxIPLimiterEntries = 10_000
)

// recordFailure increments the failure counter for addr and returns true if
// the IP is now rate-limited (i.e. failures >= maxFailures within the window).
func recordFailure(addr string) bool {
	ipLimiterMu.Lock()
	defer ipLimiterMu.Unlock()

	now := time.Now()
	entry, ok := ipLimiter[addr]
	if !ok || now.After(entry.windowEnd) {
		// First failure in this window (or previous window expired). When the
		// limiter is full, make room by forgetting an arbitrary address; the
		// sweep keeps that rare by removing expired windows first.
		if !ok && len(ipLimiter) >= maxIPLimiterEntries {
			for a := range ipLimiter {
				delete(ipLimiter, a)
				break
			}
		}
		ipLimiter[addr] = &ipEntry{failures: 1, windowEnd: now.Add(rateLimitWindow)}
		return false
	}

	entry.failures++
	return entry.failures >= maxFailures
}

// isRateLimited checks whether addr has already hit the limit, without
// incrementing the counter, and if so for how much longer.
func isRateLimited(addr string) (bool, time.Duration) {
	ipLimiterMu.Lock()
	defer ipLimiterMu.Unlock()

	entry, ok := ipLimiter[addr]
	if !ok {
		return false, 0
	}
	now := time.Now()
	if now.After(entry.windowEnd) {
		delete(ipLimiter, addr)
		return false, 0
	}
	return entry.failures >= maxFailures, entry.windowEnd.Sub(now)
}

// --- Sweeping ---
// Reads ignore expired entries but never delete them, so both maps would
// otherwise keep every key and address they have ever seen.

// authCacheSweepInterval is how often sweepAuthCachesEvery clears expired
// entries. It matches cacheTTL and rateLimitWindow, so nothing outlives its
// expiry by more than one interval.
const authCacheSweepInterval = time.Minute

// sweepAuthCaches deletes the keyCache entries and ipLimiter windows that have
// expired by now, and the ingestBuckets that are full again and have not been
// sized for ingestPlanTTL: a key's next request starts it again from a full
// bucket, which is what it would have found.
func sweepAuthCaches(now time.Time) {
	keyCacheMu.Lock()
	for k, e := range keyCache {
		if !now.Before(e.expiresAt) {
			delete(keyCache, k)
		}
	}
	keyCacheMu.Unlock()

	ipLimiterMu.Lock()
	for a, e := range ipLimiter {
		if now.After(e.windowEnd) {
			delete(ipLimiter, a)
		}
	}
	ipLimiterMu.Unlock()

	ingestBucketsMu.Lock()
	for k, b := range ingestBuckets {
		if now.Sub(b.sizedAt) >= ingestPlanTTL && b.full(now) {
			delete(ingestBuckets, k)
		}
	}
	ingestBucketsMu.Unlock()
}

// sweepAuthCachesEvery runs sweepAuthCaches every interval until ctx is done.
func sweepAuthCachesEvery(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			sweepAuthCaches(now)
		}
	}
}

// --- Middleware ---

type keyValidator interface {
	ValidateAPIKey(plaintext string) (bool, *data.APIKey, error)
}

// loggerKeyValidator validates keys through the logger, which owns the
// api_keys collection.
type loggerKeyValidator struct{}

func (loggerKeyValidator) ValidateAPIKey(plaintext string) (bool, *data.APIKey, error) {
	client, err := rpc.Dial("tcp", loggerRPCAddr())
	if err != nil {
		return false, nil, err
	}
	defer client.Close()

	var reply data.RPCValidateAPIKeyReply
	if err := client.Call("RPCServer.ValidateAPIKey", &data.RPCValidateAPIKeyArgs{Plaintext: plaintext}, &reply); err != nil {
		return false, nil, err
	}
	if !reply.Valid {
		return false, nil, nil
	}
	return true, &reply.Key, nil
}

func (app *Config) requireAPIKey(next http.Handler) http.Handler {
	return app.requireAPIKeyWith(loggerKeyValidator{}, next)
}

func (app *Config) requireAPIKeyWith(v keyValidator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r, app.TrustedProxies)

		// Pre-check: reject immediately if this IP is already rate-limited.
		if limited, wait := isRateLimited(ip); limited {
			log.Printf(`{"event":"auth","outcome":"deny","reason":"rate_limited","method":"%s","path":"%s","remote_addr":"%s","client_ip":"%s"}`,
				r.Method, r.URL.Path, r.RemoteAddr, ip)
			setRetryAfter(w, wait)
			app.errorJSON(w, fmt.Errorf("too many failed attempts"), http.StatusTooManyRequests)
			return
		}

		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") {
			recordFailure(ip)
			log.Printf(`{"event":"auth","outcome":"deny","reason":"missing_or_malformed_header","method":"%s","path":"%s","remote_addr":"%s","client_ip":"%s"}`,
				r.Method, r.URL.Path, r.RemoteAddr, ip)
			app.errorJSON(w, fmt.Errorf("missing or malformed Authorization header"), http.StatusUnauthorized)
			return
		}

		plaintext := strings.TrimPrefix(authHeader, "Bearer ")
		keyPrefix := safePrefix(plaintext)
		cacheKey := hashKey(plaintext)

		// Check cache first (keyed on hash, not plaintext).
		keyCacheMu.RLock()
		entry, cached := keyCache[cacheKey]
		keyCacheMu.RUnlock()

		if cached && time.Now().Before(entry.expiresAt) {
			if !entry.valid {
				limited := recordFailure(ip)
				log.Printf(`{"event":"auth","outcome":"deny","reason":"invalid_key","key_prefix":"%s","method":"%s","path":"%s","remote_addr":"%s","client_ip":"%s","source":"cache","rate_limited":%v}`,
					keyPrefix, r.Method, r.URL.Path, r.RemoteAddr, ip, limited)
				app.errorJSON(w, fmt.Errorf("invalid API key"), http.StatusUnauthorized)
				return
			}
			log.Printf(`{"event":"auth","outcome":"allow","key_prefix":"%s","method":"%s","path":"%s","remote_addr":"%s","client_ip":"%s","source":"cache"}`,
				keyPrefix, r.Method, r.URL.Path, r.RemoteAddr, ip)
			next.ServeHTTP(w, withKey(r, entry.keyID, entry.projectID, entry.scopes))
			return
		}

		// Cache miss — validate against DB via Logger RPC
		gen := keyCacheGeneration()
		valid, key, err := v.ValidateAPIKey(plaintext)
		if err != nil {
			log.Printf(`{"event":"auth","outcome":"error","reason":"db_error","key_prefix":"%s","method":"%s","path":"%s","remote_addr":"%s","client_ip":"%s","error":"%s"}`,
				keyPrefix, r.Method, r.URL.Path, r.RemoteAddr, ip, err.Error())
			app.errorJSON(w, fmt.Errorf("error validating API key"), http.StatusInternalServerError)
			return
		}

		keyID, projectID := "", ""
		var scopes []string
		if key != nil {
			keyID = key.ID.Hex()
			projectID = key.ProjectID.Hex()
			scopes = key.Scopes
		}

		// Write result to cache (keyed on hash).
		cacheKeyResult(cacheKey, cacheEntry{valid: valid, keyID: keyID, projectID: projectID, scopes: scopes, expiresAt: time.Now().Add(cacheTTL)}, gen)

		if !valid {
			limited := recordFailure(ip)
			log.Printf(`{"event":"auth","outcome":"deny","reason":"invalid_key","key_prefix":"%s","method":"%s","path":"%s","remote_addr":"%s","client_ip":"%s","source":"db","rate_limited":%v}`,
				keyPrefix, r.Method, r.URL.Path, r.RemoteAddr, ip, limited)
			app.errorJSON(w, fmt.Errorf("invalid API key"), http.StatusUnauthorized)
			return
		}

		log.Printf(`{"event":"auth","outcome":"allow","key_prefix":"%s","method":"%s","path":"%s","remote_addr":"%s","client_ip":"%s","source":"db"}`,
			keyPrefix, r.Method, r.URL.Path, r.RemoteAddr, ip)
		next.ServeHTTP(w, withKey(r, keyID, projectID, scopes))
	})
}

// withKey stores what requireAPIKey learned about the key in the request
// context: its id, which its ingestion rate is kept under, the project it
// belongs to and the scopes requireScope checks.
func withKey(r *http.Request, keyID, projectID string, scopes []string) *http.Request {
	ctx := context.WithValue(r.Context(), keyIDKey, keyID)
	ctx = context.WithValue(ctx, projectIDKey, projectID)
	ctx = context.WithValue(ctx, keyScopesKey, scopes)
	return r.WithContext(ctx)
}

// requireScope refuses with 403 a request whose API key does not grant scope.
// It MUST run after requireAPIKey, which puts the key's scopes in the context;
// without it there are none, and every request is refused.
//
// A failed scope check is not recorded against the IP rate limiter: the key is
// genuine, and the caller learns nothing by retrying.
func (app *Config) requireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !slices.Contains(keyScopesFromContext(r), scope) {
				log.Printf(`{"event":"auth","outcome":"deny","reason":"missing_scope","scope":"%s","method":"%s","path":"%s","remote_addr":"%s"}`,
					scope, r.Method, r.URL.Path, r.RemoteAddr)
				app.errorJSON(w, fmt.Errorf("API key lacks the %q scope", scope), http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// safePrefix returns the first 10 chars of the key ("lw_" + 7 chars) for logging.
// Never logs the full key.
func safePrefix(key string) string {
	if len(key) >= 10 {
		return key[:10]
	}
	return "[invalid]"
}

// requireUserLogin identifies the signed-in user: their GitHub user ID from
// X-User-ID, which is who they are and what memberships are matched on, and
// their login from X-User-Login, normalized, which is what they are called. It
// stores both in the request context for downstream handlers. The login still
// finds memberships stored before user IDs, until they are linked to a user
// (data.MemberFilter), and is what the dashboard records at sign-in.
//
// Both headers are caller-supplied and trusted without further verification.
// This middleware MUST run after requireInternalSecret; without that guard,
// any client could impersonate any user by forging them.
func (app *Config) requireUserLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login := data.NormalizeGithubLogin(r.Header.Get("X-User-Login"))
		if login == "" {
			log.Printf(`{"event":"auth","outcome":"deny","reason":"missing_x_user_login","method":"%s","path":"%s","remote_addr":"%s"}`,
				r.Method, r.URL.Path, r.RemoteAddr)
			app.errorJSON(w, fmt.Errorf("missing X-User-Login header"), http.StatusUnauthorized)
			return
		}
		// GitHub user IDs are positive integers; anything else names nobody.
		userID, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get("X-User-ID")), 10, 64)
		if err != nil || userID <= 0 {
			log.Printf(`{"event":"auth","outcome":"deny","reason":"missing_x_user_id","method":"%s","path":"%s","remote_addr":"%s"}`,
				r.Method, r.URL.Path, r.RemoteAddr)
			app.errorJSON(w, fmt.Errorf("missing or invalid X-User-ID header"), http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), userLoginKey, login)
		ctx = context.WithValue(ctx, userIDKey, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (app *Config) requireInternalSecret(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret := os.Getenv("INTERNAL_API_SECRET")
		header := r.Header.Get("X-Internal-Secret")
		if secret == "" || (subtle.ConstantTimeCompare([]byte(header), []byte(secret)) == 0) {
			app.errorJSON(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
