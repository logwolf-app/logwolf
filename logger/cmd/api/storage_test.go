package main

import (
	"context"
	"errors"
	"logwolf-toolbox/data"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// fakeStorageStore measures the i-th project as storing i+1 events of 100
// bytes. A slow project's measure runs until its context is done, as a huge
// project would; a failing one's errors.
type fakeStorageStore struct {
	projects []data.Project
	slow     map[primitive.ObjectID]bool
	failing  map[primitive.ObjectID]bool

	mu       sync.Mutex
	recorded map[primitive.ObjectID]data.ProjectStorage
}

func newFakeStorageStore(n int) *fakeStorageStore {
	f := &fakeStorageStore{
		slow:     map[primitive.ObjectID]bool{},
		failing:  map[primitive.ObjectID]bool{},
		recorded: map[primitive.ObjectID]data.ProjectStorage{},
	}
	for i := 0; i < n; i++ {
		f.projects = append(f.projects, data.Project{ID: primitive.NewObjectID()})
	}
	return f
}

func (f *fakeStorageStore) GetAllProjects(ctx context.Context) ([]data.Project, error) {
	return f.projects, nil
}

func (f *fakeStorageStore) MeasureProjectStorage(ctx context.Context, projectID primitive.ObjectID) (data.ProjectStorage, error) {
	if f.slow[projectID] {
		<-ctx.Done()
		return data.ProjectStorage{}, ctx.Err()
	}
	if f.failing[projectID] {
		return data.ProjectStorage{}, errors.New("measure failed")
	}
	for i, p := range f.projects {
		if p.ID == projectID {
			return data.ProjectStorage{Events: int64(i + 1), Bytes: int64(100 * (i + 1))}, nil
		}
	}
	return data.ProjectStorage{}, nil
}

func (f *fakeStorageStore) RecordProjectStorage(ctx context.Context, projectID primitive.ObjectID, at time.Time, s data.ProjectStorage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded[projectID] = s
	return nil
}

// TestMeterStorage_EveryProject: every project's measure is recorded, but one
// that fails or outlasts its timeout keeps its last measure, and does not hold
// up the projects after it.
func TestMeterStorage_EveryProject(t *testing.T) {
	f := newFakeStorageStore(4)
	f.slow[f.projects[0].ID] = true
	f.failing[f.projects[1].ID] = true

	start := time.Now()
	meterStorage(context.Background(), f, 50*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("pass took %s; the slow project held up the rest", elapsed)
	}

	for i, p := range f.projects {
		got, ok := f.recorded[p.ID]
		switch i {
		case 0, 1:
			if ok {
				t.Errorf("project %d: recorded %+v, want nothing: its measure did not complete", i, got)
			}
		default:
			want := data.ProjectStorage{Events: int64(i + 1), Bytes: int64(100 * (i + 1))}
			if !ok || got != want {
				t.Errorf("project %d: recorded %+v (%v), want %+v", i, got, ok, want)
			}
		}
	}
}

// TestMeterStorage_StopsOnShutdown: a cancelled pass records nothing more.
func TestMeterStorage_StopsOnShutdown(t *testing.T) {
	f := newFakeStorageStore(3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	meterStorage(ctx, f, time.Second)
	if len(f.recorded) != 0 {
		t.Errorf("recorded %d projects after shutdown, want 0", len(f.recorded))
	}
}

func TestStorageMeterInterval(t *testing.T) {
	cases := map[string]time.Duration{
		"":      time.Hour,
		"15m":   15 * time.Minute,
		"bogus": time.Hour,
		"-5m":   time.Hour,
		"0s":    time.Hour,
	}
	for env, want := range cases {
		t.Setenv("STORAGE_METER_INTERVAL", env)
		if got := storageMeterInterval(); got != want {
			t.Errorf("STORAGE_METER_INTERVAL=%q: interval = %s, want %s", env, got, want)
		}
	}
}
