package assistant

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

const warmupTimeZone = "Asia/Singapore"

type WarmupSchedule struct {
	Enabled  bool   `json:"enabled"`
	Start    string `json:"start"`
	End      string `json:"end"`
	TimeZone string `json:"time_zone"`
}

func defaultWarmupSchedule() WarmupSchedule {
	return WarmupSchedule{Start: "08:00", End: "23:00", TimeZone: warmupTimeZone}
}
func minuteOfDay(text string) (int, error) {
	if len(text) != 5 || text[2] != ':' {
		return 0, errors.New("时间格式应为 HH:MM")
	}
	h, errH := strconv.Atoi(text[:2])
	m, errM := strconv.Atoi(text[3:])
	if errH != nil || errM != nil || h < 0 || h > 23 || m < 0 || m > 59 || fmt.Sprintf("%02d:%02d", h, m) != text {
		return 0, errors.New("请输入有效的 00:00–23:59 时间")
	}
	return h*60 + m, nil
}
func (s WarmupSchedule) validate() error {
	if s.TimeZone != warmupTimeZone {
		return errors.New("预热时区应为 Asia/Singapore")
	}
	start, e := minuteOfDay(s.Start)
	if e != nil {
		return e
	}
	end, e := minuteOfDay(s.End)
	if e != nil {
		return e
	}
	if s.Enabled && start == end {
		return errors.New("开始和结束不能相同；全天允许请关闭时段限制")
	}
	return nil
}
func (s WarmupSchedule) permits(now time.Time) bool {
	if !s.Enabled {
		return true
	}
	if s.validate() != nil {
		return false
	}
	zone, err := time.LoadLocation(s.TimeZone)
	if err != nil {
		return false
	}
	local := now.In(zone)
	minute := local.Hour()*60 + local.Minute()
	start, _ := minuteOfDay(s.Start)
	end, _ := minuteOfDay(s.End)
	if start < end {
		return minute >= start && minute < end
	}
	return minute >= start || minute < end
}
func (s WarmupSchedule) nextOpening(now time.Time) time.Time {
	if !s.Enabled || s.permits(now) {
		return now
	}
	zone, err := time.LoadLocation(s.TimeZone)
	if err != nil {
		return now.Add(time.Minute)
	}
	local := now.In(zone)
	start, _ := minuteOfDay(s.Start)
	next := time.Date(local.Year(), local.Month(), local.Day(), start/60, start%60, 0, 0, zone)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}
func (s WarmupSchedule) description() string {
	if !s.Enabled {
		return "全天允许预热"
	}
	start, _ := minuteOfDay(s.Start)
	end, _ := minuteOfDay(s.End)
	suffix := ""
	if end < start {
		suffix = "次日 "
	}
	return "每日 " + s.Start + "–" + suffix + s.End + "（UTC+8）"
}

func (r Reader) ConfigureWarmupSchedule(enabled bool, start, end string) (WarmupReport, error) {
	s := WarmupSchedule{Enabled: enabled, Start: strings.TrimSpace(start), End: strings.TrimSpace(end), TimeZone: warmupTimeZone}
	if err := s.validate(); err != nil {
		return WarmupReport{}, err
	}
	unlock, err := lockWarmup(filepath.Join(r.warmupRoot(), "policy.lock"))
	if err != nil {
		return WarmupReport{}, err
	}
	defer unlock()
	p, j, err := r.readWarmupState()
	if err != nil {
		return WarmupReport{}, err
	}
	p.Schedule = &s
	if err = writeState(filepath.Join(r.warmupRoot(), "policy.json"), p); err != nil {
		return WarmupReport{}, err
	}
	// No request registration or account selection is reset by editing hours.
	return r.warmupReport(p, j), nil
}

func scheduleOf(policy warmupPolicy) WarmupSchedule {
	if policy.Schedule == nil {
		return defaultWarmupSchedule()
	}
	return *policy.Schedule
}
