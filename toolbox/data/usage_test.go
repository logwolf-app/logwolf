package data

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestUsageHour(t *testing.T) {
	loc := time.FixedZone("UTC-3", -3*60*60)
	got := UsageHour(time.Date(2026, 10, 5, 21, 59, 59, 999, loc))
	want := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("UsageHour = %v, want %v", got, want)
	}
}

// The checks below run before any query. The zero-value Models has no database,
// so a call that got past them would panic instead of answering.

func TestRecordUsage_RefusesBadInput(t *testing.T) {
	var m Models
	ctx := context.Background()
	count := UsageCount{ProjectID: primitive.NewObjectID(), Hour: time.Now(), Events: 1, Bytes: 10}

	cases := []struct {
		name   string
		source string
		counts []UsageCount
	}{
		{"no source", "", []UsageCount{count}},
		{"the storage job's source", StorageSource, []UsageCount{count}},
		{"negative events", "broker-a", []UsageCount{count, {ProjectID: count.ProjectID, Events: -1}}},
		{"negative bytes", "broker-a", []UsageCount{{ProjectID: count.ProjectID, Bytes: -1}, count}},
	}
	for _, tc := range cases {
		if err := m.RecordUsage(ctx, tc.source, tc.counts); !errors.Is(err, ErrInvalidUsage) {
			t.Errorf("%s: RecordUsage = %v, want ErrInvalidUsage", tc.name, err)
		}
	}
}

func TestRecordUsage_NothingToRecord(t *testing.T) {
	var m Models
	if err := m.RecordUsage(context.Background(), "broker-a", nil); err != nil {
		t.Errorf("RecordUsage with no counts = %v, want nil", err)
	}
}
