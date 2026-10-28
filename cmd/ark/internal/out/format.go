package out

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Bytes formats n in binary units with one decimal, bytes below 1 KiB as is.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Duration rounds d to a readable precision: milliseconds under a second,
// seconds under a minute, whole seconds beyond.
func Duration(d time.Duration) string {
	switch {
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// Time renders t in local time, minute precision, the table form.
func Time(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04")
}

// TimeLong renders t in local time with seconds, the inspect form.
func TimeLong(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04:05")
}

// Count renders n with thin-space thousands separators, as in "12 345".
func Count(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return "-" + Count(-n)
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ' ')
		}
		out = append(out, c)
	}
	return string(out)
}

// Ratio renders a library ratio (0 to 1, share saved) as a factor, "14.3x".
// A ratio of 1 or more, nothing stored, renders as "inf".
func Ratio(r float64) string {
	if r >= 1 {
		return "inf"
	}
	if r <= 0 {
		return "1.0x"
	}
	return fmt.Sprintf("%.1fx", 1/(1-r))
}

// ParseBytes reads a size such as "256KiB", "1M" or "4096". Units are
// binary whether written as K, KB or KiB; a bare number is bytes. The
// result is positive, so "0" and negatives are errors as well.
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	num, unit := s[:i], strings.ToLower(strings.TrimSpace(s[i:]))
	if num == "" {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	unit = strings.TrimSuffix(strings.TrimSuffix(unit, "b"), "i")
	exp := strings.Index("kmgtpe", unit)
	switch {
	case unit == "":
		exp = 0
	case len(unit) != 1 || exp < 0:
		return 0, fmt.Errorf("invalid size %q", s)
	default:
		exp++
	}
	n := int64(f * math.Pow(1024, float64(exp)))
	if n <= 0 {
		return 0, fmt.Errorf("size %q must be positive", s)
	}
	return n, nil
}

// ParseDuration reads a Go duration or a day count such as "30d"; days are
// 24 hours. The result is positive.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var d time.Duration
	var err error
	if days, ok := strings.CutSuffix(s, "d"); ok {
		var n float64
		n, err = strconv.ParseFloat(days, 64)
		d = time.Duration(n * float64(24*time.Hour))
	} else {
		d, err = time.ParseDuration(s)
	}
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration %q must be positive", s)
	}
	return d, nil
}
