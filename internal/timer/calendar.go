package timer

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Recurrence retains civil time and zone independently of UTC persistence.
// Missing DST wall times are skipped; repeated wall times use the earlier instant.
type Recurrence struct {
	Kind     string        `json:"kind,omitempty"` // daily, weekly, interval
	Interval time.Duration `json:"interval,omitempty"`
	Zone     string        `json:"zone,omitempty"`
	Hour     int           `json:"hour,omitempty"`
	Minute   int           `json:"minute,omitempty"`
	Weekday  time.Weekday  `json:"weekday,omitempty"`
}

// ResolveSchedule interprets calendar vocabulary in now's location (the host's
// time.Local in production). Dates use ISO YYYY-MM-DD; unqualified hours are 24h.
func ResolveSchedule(raw string, now time.Time) (time.Time, Recurrence, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	loc := now.Location()
	zone := loc.String()
	if zone == "Local" {
		zone = hostZone()
		var err error
		loc, err = time.LoadLocation(zone)
		if err != nil {
			return time.Time{}, Recurrence{}, fmt.Errorf("cannot load host timezone: %w", err)
		}
		now = now.In(loc)
	}
	var repeat Recurrence
	if strings.HasPrefix(s, "every ") && !strings.Contains(s, " at ") {
		duration := strings.TrimPrefix(s, "every ")
		if unit, ok := units[duration]; ok && unit.name == duration {
			duration = "one " + duration
		}
		if d := reminderDuration(duration); d > 0 {
			return now.Add(d), Recurrence{Kind: "interval", Interval: d}, nil
		}
	}
	parts := strings.Split(s, " at ")
	if len(parts) != 2 {
		return time.Time{}, repeat, fmt.Errorf("use tomorrow at 9 am, today at 18:30, on 2026-12-25 at 9, every day at 9, or every Monday at 9")
	}
	hour, minute, ok := clockTime(parts[1])
	if !ok {
		return time.Time{}, repeat, fmt.Errorf("use a time from 00:00 to 23:59, or 1 to 12 with am or pm")
	}
	date := strings.TrimSpace(parts[0])
	y, m, d := now.Date()
	day := time.Date(y, m, d, 12, 0, 0, 0, loc)
	switch {
	case date == "today":
	case date == "tomorrow":
		day = day.AddDate(0, 0, 1)
	case date == "every day" || date == "daily":
		repeat = Recurrence{Kind: "daily", Zone: zone, Hour: hour, Minute: minute}
	case strings.HasPrefix(date, "every ") || strings.HasPrefix(date, "weekly on "):
		name := strings.TrimPrefix(strings.TrimPrefix(date, "every "), "weekly on ")
		weekday, valid := weekdayName(name)
		if !valid {
			return time.Time{}, repeat, fmt.Errorf("name a weekday, for example every Monday at 9")
		}
		repeat = Recurrence{Kind: "weekly", Zone: zone, Hour: hour, Minute: minute, Weekday: weekday}
	case strings.HasPrefix(date, "next "):
		weekday, valid := weekdayName(strings.TrimPrefix(date, "next "))
		if !valid {
			return time.Time{}, repeat, fmt.Errorf("name a weekday after next")
		}
		days := (int(weekday) - int(day.Weekday()) + 7) % 7
		if days == 0 {
			days = 7
		}
		day = day.AddDate(0, 0, days)
	default:
		var err error
		day, err = time.ParseInLocation("2006-01-02", strings.TrimPrefix(date, "on "), loc)
		if err != nil {
			return time.Time{}, repeat, fmt.Errorf("use today, tomorrow, next Monday, or an ISO date such as on 2026-12-25")
		}
	}
	if repeat.Kind != "" {
		when, err := repeat.next(now, now)
		return when, repeat, err
	}
	when, valid := civilTime(day, hour, minute)
	if !valid {
		return time.Time{}, repeat, fmt.Errorf("that local time does not exist because the clocks change; choose another time")
	}
	if !when.After(now) {
		return time.Time{}, repeat, fmt.Errorf("that date and time is in the past; choose a future time")
	}
	if when.Sub(now) > MaxReminderDuration {
		return time.Time{}, repeat, fmt.Errorf("reminders can be scheduled up to 365 days ahead")
	}
	return when, repeat, nil
}

func hostZone() string {
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" {
		return strings.TrimPrefix(tz, "/usr/share/zoneinfo/")
	}
	if path, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		if i := strings.Index(path, "/zoneinfo/"); i >= 0 {
			return path[i+len("/zoneinfo/"):]
		}
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil && strings.TrimSpace(string(b)) != "" {
		return strings.TrimSpace(string(b))
	}
	// Local also supports systems whose /etc/localtime is a copied zone file.
	return "Local"
}

func clockTime(s string) (hour, minute int, ok bool) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(strings.ReplaceAll(s, "a.m.", "am"), "p.m.", "pm")
	s = strings.ReplaceAll(strings.ReplaceAll(s, "a.m", "am"), "p.m", "pm")
	meridian := ""
	for _, suffix := range []string{"am", "pm"} {
		if strings.HasSuffix(s, suffix) {
			meridian = suffix
			s = strings.TrimSpace(strings.TrimSuffix(s, suffix))
			break
		}
	}
	if s != "a" && s != "an" && s != "half" {
		words := strings.Fields(strings.ReplaceAll(s, "-", " "))
		if len(words) > 0 && words[0][0] >= 'a' && words[0][0] <= 'z' {
			if n, _, valid := countBefore(words); valid && n > 0 {
				s = strconv.Itoa(n)
			}
		}
	}
	parts := strings.Split(s, ":")
	if len(parts) > 2 || len(parts[0]) == 0 || len(parts[0]) > 2 {
		return 0, 0, false
	}
	for _, part := range parts {
		for _, r := range part {
			if r < '0' || r > '9' {
				return 0, 0, false
			}
		}
	}
	var err error
	hour, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	if len(parts) == 2 {
		if len(parts[1]) != 2 {
			return 0, 0, false
		}
		minute, err = strconv.Atoi(parts[1])
		if err != nil {
			return 0, 0, false
		}
	}
	if minute > 59 {
		return 0, 0, false
	}
	if meridian != "" {
		if hour < 1 || hour > 12 {
			return 0, 0, false
		}
		hour %= 12
		if meridian == "pm" {
			hour += 12
		}
	}
	return hour, minute, hour >= 0 && hour < 24
}

func weekdayName(name string) (time.Weekday, bool) {
	for d := time.Sunday; d <= time.Saturday; d++ {
		if strings.EqualFold(name, d.String()) {
			return d, true
		}
	}
	return 0, false
}

func civilTime(day time.Time, hour, minute int) (time.Time, bool) {
	y, m, d := day.Date()
	candidate := time.Date(y, m, d, hour, minute, 0, 0, day.Location())
	matches := func(t time.Time) bool {
		yy, mm, dd := t.Date()
		return yy == y && mm == m && dd == d && t.Hour() == hour && t.Minute() == minute
	}
	if !matches(candidate) {
		return time.Time{}, false
	}
	// Include half-hour DST transitions (Lord Howe), not just one-hour folds.
	for delta := 3 * time.Hour; delta > 0; delta -= 15 * time.Minute {
		if earlier := candidate.Add(-delta); matches(earlier) {
			return earlier, true
		}
	}
	return candidate, true
}

func (r Recurrence) next(anchor, now time.Time) (time.Time, error) {
	if r.Kind == "interval" {
		if r.Interval <= 0 || r.Interval > MaxReminderDuration {
			return time.Time{}, fmt.Errorf("invalid reminder interval")
		}
		if anchor.After(now) {
			return anchor, nil
		}
		// Work from the remainder to avoid multiplication overflow after downtime.
		return now.Add(r.Interval - now.Sub(anchor)%r.Interval), nil
	}
	if r.Kind != "daily" && r.Kind != "weekly" {
		return time.Time{}, fmt.Errorf("invalid recurrence %q", r.Kind)
	}
	loc, err := time.LoadLocation(r.Zone)
	if err != nil {
		return time.Time{}, err
	}
	if r.Hour < 0 || r.Hour > 23 || r.Minute < 0 || r.Minute > 59 || r.Weekday < time.Sunday || r.Weekday > time.Saturday {
		return time.Time{}, fmt.Errorf("invalid recurrence wall time")
	}
	local := now.In(loc)
	y, m, d := local.Date()
	day := time.Date(y, m, d, 12, 0, 0, 0, loc)
	for range 370 {
		if r.Kind == "daily" || day.Weekday() == r.Weekday {
			if when, valid := civilTime(day, r.Hour, r.Minute); valid && when.After(now) {
				return when, nil
			}
		}
		day = day.AddDate(0, 0, 1)
	}
	return time.Time{}, fmt.Errorf("no valid recurrence in the next year")
}

// ScheduleDescription is shared by confirmations and listing, so the spoken
// zone and recurrence cannot diverge from the durable schedule.
func (t Timer) ScheduleDescription() string {
	loc := time.Local
	zone := t.Zone
	if zone != "" {
		if loaded, err := time.LoadLocation(zone); err == nil {
			loc = loaded
		}
	}
	when := t.Deadline.In(loc)
	text := when.Format("Monday 2006-01-02 at 15:04 MST -07:00")
	if zone != "" {
		text += " (" + zone + ")"
	}
	switch t.Repeat.Kind {
	case "daily":
		text += fmt.Sprintf(", repeating daily at %02d:%02d", t.Repeat.Hour, t.Repeat.Minute)
	case "weekly":
		text += fmt.Sprintf(", repeating every %s at %02d:%02d", t.Repeat.Weekday, t.Repeat.Hour, t.Repeat.Minute)
	case "interval":
		text += ", repeating every " + Spoken(t.Repeat.Interval)
	}
	if !t.Resume.IsZero() {
		text += ", snoozed; the original repeating schedule resumes afterward"
	}
	return text
}
