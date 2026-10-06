package main

import (
	"context"
	"log"
	"logwolf-toolbox/data"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// storageMeterInterval is how often the storage job measures every project,
// from STORAGE_METER_INTERVAL. Each pass reads every log, so it runs hourly
// unless told otherwise.
func storageMeterInterval() time.Duration {
	if s := os.Getenv("STORAGE_METER_INTERVAL"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			log.Printf("Warning: invalid STORAGE_METER_INTERVAL %q, using default 1h", s)
		} else {
			return d
		}
	}
	return time.Hour
}

// projectStorageTimeout bounds measuring and recording one project's storage.
// Like the retention cleanup, each project gets its own, so a huge one cannot
// use up the time of the projects after it; it keeps its last measure instead.
const projectStorageTimeout = 2 * time.Minute

// storageStore is what the storage job needs from the database.
type storageStore interface {
	GetAllProjects(ctx context.Context) ([]data.Project, error)
	MeasureProjectStorage(ctx context.Context, projectID primitive.ObjectID) (data.ProjectStorage, error)
	RecordProjectStorage(ctx context.Context, projectID primitive.ObjectID, at time.Time, s data.ProjectStorage) error
}

// runStorageMeter measures what every project stores, now and then every
// storageMeterInterval, into the usage collection (see data.RecordProjectStorage).
func (app *Config) runStorageMeter(ctx context.Context) {
	interval := storageMeterInterval()
	log.Printf("Storage metering: starting, interval=%s", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		meterStorage(ctx, &app.Models, projectStorageTimeout)
		select {
		case <-ctx.Done():
			log.Println("Storage metering: shutting down")
			return
		case <-ticker.C:
		}
	}
}

// meterStorage measures and records the storage of every project, giving each
// one projectTimeout of its own. ctx stops the pass on shutdown.
func meterStorage(ctx context.Context, store storageStore, projectTimeout time.Duration) {
	listCtx, cancel := context.WithTimeout(ctx, projectListTimeout)
	projects, err := store.GetAllProjects(listCtx)
	cancel()
	if err != nil {
		log.Printf("Storage metering: error fetching projects: %v", err)
		return
	}

	for _, p := range projects {
		if ctx.Err() != nil {
			return
		}
		meterProjectStorage(ctx, store, p.ID, projectTimeout)
	}
}

func meterProjectStorage(ctx context.Context, store storageStore, projectID primitive.ObjectID, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	at := time.Now()
	s, err := store.MeasureProjectStorage(ctx, projectID)
	if err != nil {
		log.Printf("Storage metering: project %s: error measuring, keeping its last measure: %v", projectID.Hex(), err)
		return
	}
	if err := store.RecordProjectStorage(ctx, projectID, at, s); err != nil {
		log.Printf("Storage metering: project %s: error recording: %v", projectID.Hex(), err)
	}
}
