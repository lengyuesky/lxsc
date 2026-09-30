package backup

import (
	"testing"
	"time"
)

func TestScheduleCatchesMissedSlots(t *testing.T) {
	location := time.FixedZone("test", 8*3600)
	for _, tc := range []struct {
		date, frequency string
		weekday         int
		want            string
	}{
		{"2026-09-30 12:00", "daily", 0, "2026-09-30"},
		{"2026-09-30 02:00", "daily", 0, "2026-09-29"},
		{"2026-09-30 12:00", "weekly", 1, "2026-W40"},
		{"2026-09-28 02:00", "weekly", 1, "2026-W39"},
	} {
		now, err := time.ParseInLocation("2006-01-02 15:04", tc.date, location)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := scheduleSlot(WebDAVConfig{Time: "03:00", Frequency: tc.frequency, Weekday: tc.weekday}, now)
		if !ok || got != tc.want {
			t.Fatalf("%s: %q != %q", tc.date, got, tc.want)
		}
	}
}
