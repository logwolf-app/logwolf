//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"logwolf-toolbox/data"
)

// Helpers for driving the Broker over HTTP the way its two kinds of caller do:
// an SDK client holding an API key, and the dashboard holding the internal
// secret plus the signed-in user (GitHub user ID and login).

// --- SDK routes (API key) ---

// postLog sends a single log entry to the broker and asserts a 202 response.
func postLog(t *testing.T, brokerURL, apiKey, name string) {
	t.Helper()

	body, _ := json.Marshal(map[string]interface{}{
		"name":     name,
		"data":     `{}`,
		"severity": "info",
		"tags":     []string{},
	})

	req, _ := http.NewRequest(http.MethodPost, brokerURL+"/logs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("postLog %q: %v", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("postLog %q: expected 202, got %d: %s", name, resp.StatusCode, b)
	}
}

// getLogs calls GET /logs on the broker with the given API key and decodes the
// log entry names from the response.
func getLogs(t *testing.T, brokerURL, apiKey string) []string {
	t.Helper()

	req, _ := http.NewRequest(http.MethodGet, brokerURL+"/logs", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("getLogs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("getLogs: expected 200, got %d: %s", resp.StatusCode, b)
	}

	var envelope struct {
		Data []struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("getLogs: decode: %v", err)
	}

	names := make([]string, 0, len(envelope.Data))
	for _, e := range envelope.Data {
		names = append(names, e.Name)
	}
	return names
}

// deleteLog sends DELETE /logs with the given log ID scoped to apiKey's project.
func deleteLog(t *testing.T, brokerURL, apiKey, logID string) {
	t.Helper()

	deleteLogCount(t, brokerURL, apiKey, logID)
}

// deleteLogCount sends DELETE /logs and returns the number of entries deleted,
// as reported by the broker in the response body.
func deleteLogCount(t *testing.T, brokerURL, apiKey, logID string) int {
	t.Helper()

	body, _ := json.Marshal(map[string]string{"id": logID})
	req, _ := http.NewRequest(http.MethodDelete, brokerURL+"/logs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("deleteLogCount: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("deleteLogCount: expected 202, got %d: %s", resp.StatusCode, b)
	}

	var envelope struct {
		Data string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("deleteLogCount: decode: %v", err)
	}

	var n int
	fmt.Sscanf(envelope.Data, "Deleted entries: %d", &n)
	return n
}

// --- internal routes (dashboard) ---

// testUserID is the GitHub user ID the tests give a login: a fixed positive
// number per login, whatever its casing, so a user's requests and their
// memberships agree without a table of users.
func testUserID(login string) int64 {
	h := fnv.New64a()
	h.Write([]byte(data.NormalizeGithubLogin(login)))
	return int64(h.Sum64()>>2) + 1
}

// internalCall performs a dashboard-style request: internal secret plus the
// signed-in user the broker checks project membership against, by GitHub user
// ID (testUserID of the login) and login. It returns the status code and the
// decoded "data" member of the response envelope.
func internalCall(t *testing.T, brokerURL, method, path, userLogin string, body any) (int, json.RawMessage) {
	t.Helper()
	return internalCallAs(t, brokerURL, method, path, userLogin, testUserID(userLogin), body)
}

// internalCallAs is internalCall as the user with this GitHub user ID; 0 leaves
// X-User-ID out.
func internalCallAs(t *testing.T, brokerURL, method, path, userLogin string, userID int64, body any) (int, json.RawMessage) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("internalCall %s %s: marshal body: %v", method, path, err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, brokerURL+path, reader)
	if err != nil {
		t.Fatalf("internalCall %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Secret", internalSecret)
	if userID != 0 {
		req.Header.Set("X-User-ID", strconv.FormatInt(userID, 10))
	}
	req.Header.Set("X-User-Login", userLogin)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("internalCall %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("internalCall %s %s: decode %q: %v", method, path, raw, err)
		}
	}

	return resp.StatusCode, envelope.Data
}

// mustInternalCall is internalCall with the status asserted.
func mustInternalCall(t *testing.T, brokerURL, method, path, userLogin string, body any, wantStatus int) json.RawMessage {
	t.Helper()

	status, data := internalCall(t, brokerURL, method, path, userLogin, body)
	if status != wantStatus {
		t.Fatalf("%s %s as %q: got %d, want %d (data: %s)", method, path, userLogin, status, wantStatus, data)
	}
	return data
}

// --- MongoDB polling ---

// waitForLog polls MongoDB until a document with the given name appears.
func waitForLog(t *testing.T, mongoURI, name string) {
	t.Helper()

	ctx := context.Background()
	coll := testMongo(t, mongoURI).Database("logs").Collection("logs")

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		count, _ := coll.CountDocuments(ctx, bson.M{"name": name})
		if count > 0 {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("waitForLog: %q never appeared in MongoDB", name)
}

// fetchLogID reads the _id of the first log entry matching name directly from MongoDB.
func fetchLogID(t *testing.T, mongoURI, name string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var doc struct {
		ID primitive.ObjectID `bson:"_id"`
	}
	err := testMongo(t, mongoURI).Database("logs").Collection("logs").
		FindOne(ctx, bson.M{"name": name}).Decode(&doc)
	if err != nil {
		t.Fatalf("fetchLogID: find %q: %v", name, err)
	}

	return doc.ID.Hex()
}

func containsName(names []string, target string) bool {
	for _, n := range names {
		if n == target {
			return true
		}
	}
	return false
}
