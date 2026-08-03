package quota

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultAccessTimezone = "Asia/Shanghai"

var accessLocationCache sync.Map // map[string]*time.Location

// AccessRule is one recurring weekly access interval. Weekdays use the
// operator-facing convention Monday=1 through Sunday=7. A start later than
// end spans midnight into the following calendar day.
type AccessRule struct {
	Weekdays []int  `yaml:"weekdays" json:"weekdays"`
	Start    string `yaml:"start" json:"start"`
	End      string `yaml:"end" json:"end"`
}

func normalizeAccessRules(rules []AccessRule, timezone string) ([]AccessRule, string, error) {
	if len(rules) == 0 {
		return nil, "", nil
	}
	if len(rules) > 16 {
		return nil, "", fmt.Errorf("access_rules must not contain more than 16 intervals")
	}
	timezone = strings.TrimSpace(timezone)
	if timezone == "" {
		timezone = defaultAccessTimezone
	}
	if _, err := accessLocation(timezone); err != nil {
		return nil, "", fmt.Errorf("access_timezone %q is invalid", timezone)
	}
	normalized := make([]AccessRule, 0, len(rules))
	for index, rule := range rules {
		start, err := accessClockMinute(rule.Start)
		if err != nil {
			return nil, "", fmt.Errorf("access_rules[%d].start: %w", index, err)
		}
		end, err := accessClockMinute(rule.End)
		if err != nil {
			return nil, "", fmt.Errorf("access_rules[%d].end: %w", index, err)
		}
		if start == end {
			return nil, "", fmt.Errorf("access_rules[%d] start and end must differ", index)
		}
		seen := make(map[int]struct{}, len(rule.Weekdays))
		weekdays := make([]int, 0, len(rule.Weekdays))
		for _, weekday := range rule.Weekdays {
			if weekday < 1 || weekday > 7 {
				return nil, "", fmt.Errorf("access_rules[%d].weekdays must use Monday=1 through Sunday=7", index)
			}
			if _, exists := seen[weekday]; exists {
				continue
			}
			seen[weekday] = struct{}{}
			weekdays = append(weekdays, weekday)
		}
		if len(weekdays) == 0 {
			return nil, "", fmt.Errorf("access_rules[%d].weekdays is required", index)
		}
		sort.Ints(weekdays)
		normalized = append(normalized, AccessRule{
			Weekdays: weekdays,
			Start:    formatAccessClock(start),
			End:      formatAccessClock(end),
		})
	}
	return normalized, timezone, nil
}

// accessLocation avoids filesystem/zoneinfo work on the request hot path. A
// policy's timezone was already validated at save time; the cache merely
// resolves that immutable IANA identifier to the shared Go location object.
func accessLocation(timezone string) (*time.Location, error) {
	if cached, ok := accessLocationCache.Load(timezone); ok {
		return cached.(*time.Location), nil
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, err
	}
	actual, _ := accessLocationCache.LoadOrStore(timezone, location)
	return actual.(*time.Location), nil
}

func accessClockMinute(value string) (int, error) {
	parsed, err := time.Parse("15:04", strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("must be HH:MM")
	}
	return parsed.Hour()*60 + parsed.Minute(), nil
}

func formatAccessClock(minute int) string {
	return fmt.Sprintf("%02d:%02d", minute/60, minute%60)
}

func accessWeekday(value time.Time) int {
	if value.Weekday() == time.Sunday {
		return 7
	}
	return int(value.Weekday())
}

func accessRuleIncludesWeekday(rule AccessRule, weekday int) bool {
	for _, candidate := range rule.Weekdays {
		if candidate == weekday {
			return true
		}
	}
	return false
}

// AllowsAt evaluates only a policy's local recurring schedule. It never
// reads SQLite or CPA and is therefore safe on the scheduler hot path.
func (policy KeyPolicy) AllowsAt(now time.Time) bool {
	if len(policy.AccessRules) == 0 {
		return true
	}
	location, err := accessLocation(policy.AccessTimezone)
	if err != nil {
		// Invalid values cannot be saved by normalizePolicy. Fail closed if an
		// older/corrupted record reaches memory instead of silently bypassing a
		// configured access control.
		return false
	}
	local := now.In(location)
	minute := local.Hour()*60 + local.Minute()
	weekday := accessWeekday(local)
	previousWeekday := accessWeekday(local.AddDate(0, 0, -1))
	for _, rule := range policy.AccessRules {
		start, startErr := accessClockMinute(rule.Start)
		end, endErr := accessClockMinute(rule.End)
		if startErr != nil || endErr != nil || start == end {
			return false
		}
		if start < end {
			if accessRuleIncludesWeekday(rule, weekday) && minute >= start && minute < end {
				return true
			}
			continue
		}
		if (accessRuleIncludesWeekday(rule, weekday) && minute >= start) ||
			(accessRuleIncludesWeekday(rule, previousWeekday) && minute < end) {
			return true
		}
	}
	return false
}
