package papers

import (
	"context"
	"fmt"
	"time"
)

// Window is one arXiv submission interval, minute resolution in UTC.
// After is exclusive and Through is inclusive: a paper's published minute
// belongs to the batch that ends at that minute, not the next one.
type Window struct {
	After   time.Time
	Through time.Time
}

func (w Window) Valid() bool {
	return !w.After.IsZero() && !w.Through.IsZero() && w.Through.After(w.After)
}

// Contains reports whether published falls in this submission window.
// The comparison uses the UTC minute, matching arXiv's submittedDate filter.
func (w Window) Contains(published time.Time) bool {
	if published.IsZero() || !w.Valid() {
		return false
	}
	minute := published.UTC().Truncate(time.Minute)
	start := w.After.UTC().Truncate(time.Minute).Add(time.Minute)
	end := w.Through.UTC().Truncate(time.Minute)
	return !minute.Before(start) && !minute.After(end)
}

func (w Window) submittedDateClause() (string, error) {
	if !w.Valid() {
		return "", fmt.Errorf("invalid arXiv submission window")
	}
	start := w.After.UTC().Truncate(time.Minute).Add(time.Minute)
	end := w.Through.UTC().Truncate(time.Minute)
	if end.Before(start) {
		return "", fmt.Errorf("invalid arXiv submission window")
	}
	return fmt.Sprintf("submittedDate:[%s TO %s]", start.Format("200601021504"), end.Format("200601021504")), nil
}

// Batch is one arXiv announcement. Announcement is the America/New_York
// calendar date of the 20:00 ET mailing. Window is the submission interval
// that mailing publishes.
type Batch struct {
	Announcement string
	Window       Window
}

// DigestBatch maps a Shanghai digest date to the single arXiv announcement
// that is new for that morning's 08:00 run.
//
// arXiv freezes submissions at 14:00 America/New_York and announces them at
// 20:00 the same evening, Sunday through Thursday. Friday and Saturday have
// no announcement. The Sunday mailing publishes submissions from Thursday
// 14:00 through Friday 14:00. The Monday mailing publishes Friday 14:00
// through Monday 14:00. Tuesday through Thursday each publish the previous
// deadline through 14:00 that day.
//
// The digest for Shanghai calendar date D runs at 08:00–09:00 Asia/Shanghai.
// It takes the latest announcement strictly before 08:00 that morning. During
// daylight saving that instant is 08:00 Shanghai on the next calendar day;
// during standard time it is 09:00. Either way the announcement is not yet
// due at 08:00 on its Shanghai calendar day, so it is collected the following
// morning. The resulting schedule is the same in both seasons:
//
//	Shanghai Tue → Sunday announcement
//	Shanghai Wed → Monday announcement
//	Shanghai Thu → Tuesday announcement
//	Shanghai Fri → Wednesday announcement
//	Shanghai Sat → Thursday announcement
//	Shanghai Sun, Mon → no new announcement
//
// ok is false when that latest announcement was already due the previous
// morning. Those digests stay empty and do not reread the previous batch.
// Successive batches tile the submission timeline: each published minute
// belongs to one announcement. Replacements and later cross-lists keep the
// original submission timestamp, so they are not collected again.
//
// arXiv sometimes defers a mailing for a US holiday and does not publish a
// calendar of those days. This mapping follows the regular Sunday–Thursday
// schedule. A deferred batch is not pulled into the next digest.
func DigestBatch(shanghaiDate string) (Batch, bool, error) {
	ready, err := shanghaiMorning(shanghaiDate)
	if err != nil {
		return Batch{}, false, err
	}
	current, ok := latestAnnouncementBefore(ready)
	if !ok {
		return Batch{}, false, fmt.Errorf("no arXiv announcement before %s", shanghaiDate)
	}
	prior, ok := latestAnnouncementBefore(ready.AddDate(0, 0, -1))
	if !ok {
		return Batch{}, false, fmt.Errorf("no arXiv announcement before %s", shanghaiDate)
	}
	if current.Equal(prior) {
		return Batch{}, false, nil
	}
	return announcementBatch(current), true, nil
}

// AnnouncementBatches returns one batch per digest date that has a new
// announcement, in date order. Days without a batch are omitted.
func AnnouncementBatches(dates []string) ([]Batch, error) {
	batches := make([]Batch, 0, len(dates))
	for _, date := range dates {
		batch, ok, err := DigestBatch(date)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		batches = append(batches, batch)
	}
	return batches, nil
}

// FetchEachBatch loads one announcement window at a time. The arXiv page cap
// is sized for a single announcement, so a multi-day preview must not merge
// those windows into one query.
func FetchEachBatch(ctx context.Context, dates []string, query string, fetch func(context.Context, Window, string) ([]Paper, error)) ([]Paper, error) {
	if fetch == nil {
		return nil, fmt.Errorf("arxiv fetch is not configured")
	}
	batches, err := AnnouncementBatches(dates)
	if err != nil {
		return nil, err
	}
	var fetched []Paper
	for _, batch := range batches {
		page, err := fetch(ctx, batch.Window, query)
		if err != nil {
			return nil, err
		}
		fetched = append(fetched, page...)
	}
	return fetched, nil
}

// Cover is the submission span of every digest date that has a new batch.
// ok is false when none of the dates has one. Do not pass this span to
// FetchWindow for more than one announcement; use FetchEachBatch instead.
func Cover(dates []string) (Window, bool, error) {
	var merged Window
	any := false
	for _, date := range dates {
		batch, ok, err := DigestBatch(date)
		if err != nil {
			return Window{}, false, err
		}
		if !ok {
			continue
		}
		if !any {
			merged = batch.Window
			any = true
			continue
		}
		if batch.Window.After.Before(merged.After) {
			merged.After = batch.Window.After
		}
		if batch.Window.Through.After(merged.Through) {
			merged.Through = batch.Window.Through
		}
	}
	return merged, any, nil
}

func shanghaiMorning(date string) (time.Time, error) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.Time{}, err
	}
	day, err := time.ParseInLocation("2006-01-02", date, loc)
	if err != nil || day.Format("2006-01-02") != date {
		return time.Time{}, fmt.Errorf("invalid date %q", date)
	}
	return time.Date(day.Year(), day.Month(), day.Day(), 8, 0, 0, 0, loc), nil
}

func latestAnnouncementBefore(ready time.Time) (time.Time, bool) {
	et, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.Time{}, false
	}
	cursor := ready.In(et)
	var best time.Time
	found := false
	for i := 0; i < 10; i++ {
		year, month, day := cursor.Date()
		midnight := time.Date(year, month, day, 0, 0, 0, 0, et)
		if announces(midnight.Weekday()) {
			at := time.Date(year, month, day, 20, 0, 0, 0, et)
			if at.Before(ready) && (!found || at.After(best)) {
				best = at
				found = true
			}
		}
		cursor = midnight.AddDate(0, 0, -1)
	}
	return best, found
}

func announces(day time.Weekday) bool {
	return day != time.Friday && day != time.Saturday
}

func announcementBatch(at time.Time) Batch {
	et := at.In(mustLocation("America/New_York"))
	year, month, day := et.Date()
	announcement := time.Date(year, month, day, 0, 0, 0, 0, et.Location())
	return Batch{
		Announcement: announcement.Format("2006-01-02"),
		Window: Window{
			After:   submissionDeadline(previousAnnouncement(announcement)),
			Through: submissionDeadline(announcement),
		},
	}
}

func previousAnnouncement(day time.Time) time.Time {
	switch day.Weekday() {
	case time.Sunday:
		return day.AddDate(0, 0, -3)
	case time.Monday:
		return day.AddDate(0, 0, -1)
	default:
		return day.AddDate(0, 0, -1)
	}
}

func submissionDeadline(announcement time.Time) time.Time {
	et := announcement.In(mustLocation("America/New_York"))
	year, month, day := et.Date()
	deadlineDay := time.Date(year, month, day, 0, 0, 0, 0, et.Location())
	if deadlineDay.Weekday() == time.Sunday {
		deadlineDay = deadlineDay.AddDate(0, 0, -2)
	}
	return time.Date(deadlineDay.Year(), deadlineDay.Month(), deadlineDay.Day(), 14, 0, 0, 0, et.Location())
}

func mustLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}
