package papers

import (
	"context"
	"testing"
	"time"
)

func TestDigestBatchFollowsTheEasternAnnouncementCalendar(t *testing.T) {
	cases := []struct {
		date         string
		ok           bool
		announcement string
		throughUTC   string
	}{
		{date: "2026-10-06", ok: true, announcement: "2026-10-04", throughUTC: "2026-10-02T18:00:00Z"},
		{date: "2026-10-07", ok: true, announcement: "2026-10-05", throughUTC: "2026-10-05T18:00:00Z"},
		{date: "2026-10-08", ok: true, announcement: "2026-10-06", throughUTC: "2026-10-06T18:00:00Z"},
		{date: "2026-10-09", ok: true, announcement: "2026-10-07", throughUTC: "2026-10-07T18:00:00Z"},
		{date: "2026-10-10", ok: true, announcement: "2026-10-08", throughUTC: "2026-10-08T18:00:00Z"},
		{date: "2026-10-11", ok: false},
		{date: "2026-10-12", ok: false},
		{date: "2026-01-06", ok: true, announcement: "2026-01-04", throughUTC: "2026-01-02T19:00:00Z"},
		{date: "2026-01-07", ok: true, announcement: "2026-01-05", throughUTC: "2026-01-05T19:00:00Z"},
		{date: "2026-01-10", ok: true, announcement: "2026-01-08", throughUTC: "2026-01-08T19:00:00Z"},
		{date: "2026-01-11", ok: false},
		{date: "2026-01-12", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.date, func(t *testing.T) {
			batch, ok, err := DigestBatch(tc.date)
			if err != nil {
				t.Fatal(err)
			}
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if batch.Announcement != tc.announcement {
				t.Fatalf("announcement = %s, want %s", batch.Announcement, tc.announcement)
			}
			if got := batch.Window.Through.UTC().Format(time.RFC3339); got != tc.throughUTC {
				t.Fatalf("through = %s, want %s", got, tc.throughUTC)
			}
		})
	}
}

func TestDigestBatchesTileSubmissionMinutesAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, loc)
	var previous *Window
	for i := 0; i < 400; i++ {
		date := day.Format("2006-01-02")
		batch, ok, err := DigestBatch(date)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			if previous != nil && !batch.Window.After.Equal(previous.Through) {
				t.Fatalf("%s starts at %s, previous batch ended at %s", date, batch.Window.After, previous.Through)
			}
			if batch.Window.Contains(batch.Window.After) || !batch.Window.Contains(batch.Window.Through) {
				t.Fatalf("%s boundary inclusion after=%v through=%v", date, batch.Window.Contains(batch.Window.After), batch.Window.Contains(batch.Window.Through))
			}
			next := batch.Window.Through.Add(time.Minute)
			if batch.Window.Contains(next) {
				t.Fatalf("%s also contains the next minute %s", date, next)
			}
			copied := batch.Window
			previous = &copied
		}
		day = day.AddDate(0, 0, 1)
	}
	if previous == nil {
		t.Fatal("no batches in 2026")
	}
}

func TestOctober9DigestKeepsThatSubmissionDayOnly(t *testing.T) {
	batch, ok, err := DigestBatch("2026-10-09")
	if err != nil || !ok {
		t.Fatalf("batch ok=%v err=%v", ok, err)
	}
	inside := []string{
		"2026-10-07T17:38:35Z",
		"2026-10-07T17:15:53Z",
		"2026-10-07T15:33:17Z",
		"2026-10-07T15:16:34Z",
		"2026-10-07T15:07:37Z",
	}
	for _, value := range inside {
		published, err := time.Parse(time.RFC3339, value)
		if err != nil {
			t.Fatal(err)
		}
		if !batch.Window.Contains(published) {
			t.Fatalf("%s is outside the 2026-10-09 batch", value)
		}
	}
	later, err := time.Parse(time.RFC3339, "2026-10-07T22:14:05Z")
	if err != nil {
		t.Fatal(err)
	}
	if batch.Window.Contains(later) {
		t.Fatal("a submission after the Wednesday 14:00 ET deadline was included")
	}
	next, ok, err := DigestBatch("2026-10-10")
	if err != nil || !ok || !next.Window.Contains(later) || next.Window.Contains(batch.Window.Through) {
		t.Fatalf("next batch ok=%v err=%v contains later=%v contains boundary=%v", ok, err, next.Window.Contains(later), next.Window.Contains(batch.Window.Through))
	}
	sunday, ok, err := DigestBatch("2026-10-11")
	if err != nil || ok || sunday.Window.Contains(later) {
		t.Fatalf("sunday ok=%v window=%+v err=%v", ok, sunday.Window, err)
	}
}

func TestFetchEachBatchKeepsTheDailyPageCap(t *testing.T) {
	dates := []string{"2026-10-09", "2026-10-10", "2026-10-11"}
	var windows []Window
	fetched, err := FetchEachBatch(context.Background(), dates, "cat:cs.IR", func(_ context.Context, window Window, query string) ([]Paper, error) {
		if query != "cat:cs.IR" {
			t.Fatalf("query = %s", query)
		}
		windows = append(windows, window)
		return []Paper{{ID: window.Through.Format(time.RFC3339)}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 2 || len(fetched) != 2 {
		t.Fatalf("windows=%d papers=%d", len(windows), len(fetched))
	}
	for i, date := range []string{"2026-10-09", "2026-10-10"} {
		batch, ok, err := DigestBatch(date)
		if err != nil || !ok {
			t.Fatalf("%s ok=%v err=%v", date, ok, err)
		}
		if !windows[i].After.Equal(batch.Window.After) || !windows[i].Through.Equal(batch.Window.Through) {
			t.Fatalf("%s window = %s..%s, want %s..%s", date, windows[i].After, windows[i].Through, batch.Window.After, batch.Window.Through)
		}
	}
	merged, ok, err := Cover(dates)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if windows[0].After.Equal(merged.After) && windows[0].Through.Equal(merged.Through) {
		t.Fatal("first fetch used the merged multi-day window")
	}
}

func TestDigestBatchRejectsBadDates(t *testing.T) {
	if _, _, err := DigestBatch("2026-02-30"); err == nil {
		t.Fatal("invalid date was accepted")
	}
}
