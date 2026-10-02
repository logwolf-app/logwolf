package data

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ErrInvalidUser is what UpsertUser answers for a user it cannot key: no GitHub
// user ID, or no login.
var ErrInvalidUser = errors.New("invalid user")

// User is a person who has signed in to the dashboard, keyed by their GitHub
// user ID. A login is not an identity: GitHub lets people rename their account,
// and another account can then take the old name. The numeric ID never changes,
// so it is what a user is found by.
//
// GithubLogin is the login the user had when they last signed in, normalized
// like every stored login (NormalizeGithubLogin). It is for display and for
// matching invites, never for deciding who someone is: after a rename two users
// can briefly share one, until the stale one signs in again.
type User struct {
	ID          primitive.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	GithubID    int64              `bson:"github_id" json:"github_id"`
	GithubLogin string             `bson:"github_login" json:"github_login"`
	Email       string             `bson:"email" json:"email"`
	CreatedAt   time.Time          `bson:"created_at" json:"created_at"`
}

// RPCUpsertUserArgs is the RPC argument for UpsertUser: the user as GitHub
// describes them at sign-in.
type RPCUpsertUserArgs struct {
	GithubID    int64
	GithubLogin string
	Email       string
}

// RPCGetUserArgs is the RPC argument for GetUser.
type RPCGetUserArgs struct {
	GithubID int64
}

// RPCGetUserReply is what GetUser answers. An unknown user is not an error, it
// is Found false: net/rpc sends errors as bare strings, which a caller could
// only tell apart by their text. User is the zero value unless Found.
type RPCGetUserReply struct {
	Found bool
	User  User
}

func (m *Models) users() *mongo.Collection {
	return m.client.Database("logs").Collection("users")
}

// EnsureUserIndexes creates the unique index on github_id, which keeps one user
// per GitHub account and serves every lookup. Safe to call on startup —
// CreateOne is idempotent for identical index definitions.
func (m *Models) EnsureUserIndexes() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := m.users().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "github_id", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("unique_github_id"),
	}); err != nil {
		return fmt.Errorf("EnsureUserIndexes users.github_id: %w", err)
	}
	return nil
}

// UpsertUser records a sign-in: it creates the user with this GitHub ID, or
// refreshes the login and email of the one that exists, and returns the user as
// stored. The ID and creation time never change, so a rename updates what is
// displayed without touching who the user is.
//
// Login and email are what GitHub says now. An empty email clears a stored one:
// the user has made theirs private, and it is not kept against their wish.
func (m *Models) UpsertUser(githubID int64, login, email string) (*User, error) {
	login = NormalizeGithubLogin(login)
	if githubID <= 0 {
		return nil, fmt.Errorf("UpsertUser: %w: GitHub user ID must be positive, got %d", ErrInvalidUser, githubID)
	}
	if login == "" {
		return nil, fmt.Errorf("UpsertUser: %w: login is required", ErrInvalidUser)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// An insert takes github_id from the filter.
	filter := bson.M{"github_id": githubID}
	update := bson.M{
		"$set":         bson.M{"github_login": login, "email": strings.TrimSpace(email)},
		"$setOnInsert": bson.M{"created_at": time.Now()},
	}
	opts := options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)

	// Two first sign-ins of one user at once can both miss and both insert; the
	// unique index refuses the second, which then finds the first's document.
	var user User
	err := m.users().FindOneAndUpdate(ctx, filter, update, opts).Decode(&user)
	if mongo.IsDuplicateKeyError(err) {
		err = m.users().FindOneAndUpdate(ctx, filter, update, opts).Decode(&user)
	}
	if err != nil {
		return nil, fmt.Errorf("UpsertUser: %w", err)
	}
	return &user, nil
}

// GetUserByGithubID returns the user with this GitHub user ID, or
// mongo.ErrNoDocuments (wrapped) if they have never signed in.
func (m *Models) GetUserByGithubID(githubID int64) (*User, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var user User
	if err := m.users().FindOne(ctx, bson.M{"github_id": githubID}).Decode(&user); err != nil {
		return nil, fmt.Errorf("GetUserByGithubID: %w", err)
	}
	return &user, nil
}
