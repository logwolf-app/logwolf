//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/rpc"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"logwolf-toolbox/data"
)

// setupUserModels returns a data.Models on the shared data-layer MongoDB with
// the users collection cleared and its index recreated.
func setupUserModels(t *testing.T) (data.Models, *mongo.Collection) {
	t.Helper()

	client := testMongo(t, sharedModelsMongo(t))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	users := client.Database("logs").Collection("users")
	if err := users.Drop(ctx); err != nil {
		t.Fatalf("setupUserModels: drop users: %v", err)
	}

	m := data.New(client)
	if err := m.EnsureUserIndexes(); err != nil {
		t.Fatalf("setupUserModels: indexes: %v", err)
	}
	return m, users
}

func TestUpsertUser_CreatesThenRefreshes(t *testing.T) {
	m, _ := setupUserModels(t)

	created, err := m.UpsertUser(1001, "JDoe", "jdoe@example.com")
	if err != nil {
		t.Fatalf("first UpsertUser: %v", err)
	}
	if created.ID.IsZero() || created.CreatedAt.IsZero() {
		t.Fatalf("first UpsertUser: want an id and a creation time, got %+v", created)
	}
	if created.GithubID != 1001 || created.GithubLogin != "jdoe" || created.Email != "jdoe@example.com" {
		t.Errorf("first UpsertUser: got %+v, want github_id 1001, login jdoe (normalized), the email", created)
	}

	// The same account, renamed on GitHub: the same user, with the new login.
	renamed, err := m.UpsertUser(1001, "Jane-Doe", "jane@example.com")
	if err != nil {
		t.Fatalf("UpsertUser after a rename: %v", err)
	}
	if renamed.ID != created.ID {
		t.Errorf("a rename made a new user: id %s, want %s", renamed.ID.Hex(), created.ID.Hex())
	}
	if !renamed.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("a rename moved created_at from %v to %v", created.CreatedAt, renamed.CreatedAt)
	}
	if renamed.GithubLogin != "jane-doe" || renamed.Email != "jane@example.com" {
		t.Errorf("UpsertUser after a rename: got %+v, want login jane-doe and the new email", renamed)
	}

	got, err := m.GetUserByGithubID(1001)
	if err != nil {
		t.Fatalf("GetUserByGithubID: %v", err)
	}
	if got.ID != created.ID || got.GithubLogin != "jane-doe" {
		t.Errorf("GetUserByGithubID: got %+v, want the renamed user", got)
	}
}

// A user who makes their email private no longer has one stored.
func TestUpsertUser_EmptyEmailClearsIt(t *testing.T) {
	m, _ := setupUserModels(t)

	if _, err := m.UpsertUser(1002, "jdoe", "jdoe@example.com"); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	user, err := m.UpsertUser(1002, "jdoe", "")
	if err != nil {
		t.Fatalf("UpsertUser without an email: %v", err)
	}
	if user.Email != "" {
		t.Errorf("email = %q, want it cleared", user.Email)
	}
}

// Someone else taking a login a renamed user gave up is a different user: the
// GitHub ID decides, never the login.
func TestUpsertUser_SameLoginDifferentIDIsAnotherUser(t *testing.T) {
	m, _ := setupUserModels(t)

	first, err := m.UpsertUser(1003, "taken", "")
	if err != nil {
		t.Fatalf("UpsertUser 1003: %v", err)
	}
	second, err := m.UpsertUser(1004, "taken", "")
	if err != nil {
		t.Fatalf("UpsertUser 1004 with the same login: %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("two GitHub accounts with one login were merged into one user")
	}

	got, err := m.GetUserByGithubID(1003)
	if err != nil || got.ID != first.ID {
		t.Errorf("GetUserByGithubID(1003) = %+v, %v; want the first user", got, err)
	}
}

func TestGetUserByGithubID_NotFound(t *testing.T) {
	m, _ := setupUserModels(t)

	_, err := m.GetUserByGithubID(9999)
	if !errors.Is(err, mongo.ErrNoDocuments) {
		t.Errorf("GetUserByGithubID for an unknown id: want mongo.ErrNoDocuments, got %v", err)
	}
}

// Simultaneous first sign-ins of one account leave exactly one user.
func TestUpsertUser_ConcurrentFirstSignIns(t *testing.T) {
	m, users := setupUserModels(t)

	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.UpsertUser(1005, "racer", ""); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent UpsertUser: %v", err)
	}

	count, err := users.CountDocuments(context.Background(), bson.M{"github_id": 1005})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("%d users for one GitHub account, want 1", count)
	}
}

// The unique index holds even against writes that bypass UpsertUser.
func TestUserIndexes_GithubIDIsUnique(t *testing.T) {
	_, users := setupUserModels(t)

	ctx := context.Background()
	if _, err := users.InsertOne(ctx, bson.M{"github_id": int64(1006), "github_login": "a"}); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err := users.InsertOne(ctx, bson.M{"github_id": int64(1006), "github_login": "b"})
	if !mongo.IsDuplicateKeyError(err) {
		t.Errorf("second user with github_id 1006: want a duplicate key error, got %v", err)
	}
}

// TestUserRPC goes through the logger's RPC server, which also proves the
// logger created the unique index at startup.
func TestUserRPC(t *testing.T) {
	stack := sharedStack(t)

	conn, err := rpc.Dial("tcp", stack.loggerRPCAddr)
	if err != nil {
		t.Fatalf("dial logger RPC: %v", err)
	}
	defer conn.Close()

	// The stack's database is shared, so the id is one no other test uses.
	githubID := time.Now().UnixNano()

	var miss data.RPCGetUserReply
	if err := conn.Call("RPCServer.GetUser", data.RPCGetUserArgs{GithubID: githubID}, &miss); err != nil {
		t.Fatalf("GetUser before sign-in: %v", err)
	}
	if miss.Found {
		t.Fatalf("GetUser before sign-in found %+v", miss.User)
	}

	var created data.User
	if err := conn.Call("RPCServer.UpsertUser", data.RPCUpsertUserArgs{
		GithubID: githubID, GithubLogin: "RPC-User", Email: "rpc@example.com",
	}, &created); err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	if created.ID.IsZero() || created.GithubID != githubID || created.GithubLogin != "rpc-user" {
		t.Errorf("UpsertUser reply: got %+v", created)
	}

	var hit data.RPCGetUserReply
	if err := conn.Call("RPCServer.GetUser", data.RPCGetUserArgs{GithubID: githubID}, &hit); err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if !hit.Found || hit.User.ID != created.ID || hit.User.Email != "rpc@example.com" {
		t.Errorf("GetUser: got %+v, want the user just upserted", hit)
	}

	client := testMongo(t, stack.mongoURI)
	indexes, err := client.Database("logs").Collection("users").Indexes().ListSpecifications(context.Background())
	if err != nil {
		t.Fatalf("list users indexes: %v", err)
	}
	unique := false
	for _, ix := range indexes {
		if ix.Name == "unique_github_id" && ix.Unique != nil && *ix.Unique {
			unique = true
		}
	}
	if !unique {
		t.Errorf("logger did not create the unique github_id index; indexes: %+v", indexes)
	}
}

// TestSignInThroughBroker records sign-ins the way the dashboard does, through
// the broker's PUT /users/me: a rename is the same user under the new login.
func TestSignInThroughBroker(t *testing.T) {
	stack := sharedStack(t)

	// The stack's database is shared, so the id is one no other test uses.
	githubID := time.Now().UnixNano()

	var first data.User
	raw := mustInternalCall(t, stack.brokerURL, http.MethodPut, "/users/me", "Broker-User",
		map[string]any{"github_id": githubID, "email": "broker@example.com"}, http.StatusOK)
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatalf("decode first sign-in: %v", err)
	}
	if first.ID.IsZero() || first.GithubID != githubID || first.GithubLogin != "broker-user" || first.Email != "broker@example.com" {
		t.Errorf("first sign-in: got %+v", first)
	}

	var renamed data.User
	raw = mustInternalCall(t, stack.brokerURL, http.MethodPut, "/users/me", "Broker-User-Renamed",
		map[string]any{"github_id": githubID}, http.StatusOK)
	if err := json.Unmarshal(raw, &renamed); err != nil {
		t.Fatalf("decode sign-in after rename: %v", err)
	}
	if renamed.ID != first.ID || renamed.GithubLogin != "broker-user-renamed" || renamed.Email != "" {
		t.Errorf("sign-in after rename: got %+v, want user %s with the new login and no email", renamed, first.ID.Hex())
	}

	if status, _ := internalCall(t, stack.brokerURL, http.MethodPut, "/users/me", "broker-user", map[string]any{}); status != http.StatusBadRequest {
		t.Errorf("sign-in without a GitHub ID = %d, want 400", status)
	}
}
