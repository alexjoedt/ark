package out

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/alexjoedt/ark"
)

// Bytes formats n in binary units with one decimal, bytes below 1 KiB as is.
func Bytes(n int64) string {
	div, unit := byteUnit(n)
	if div == 1 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), unit)
}

// BytesOf formats done and total in the unit of total, as in "3.1 / 10.0 GiB".
func BytesOf(done, total int64) string {
	div, unit := byteUnit(total)
	if div == 1 {
		return fmt.Sprintf("%d / %d B", done, total)
	}
	return fmt.Sprintf("%.1f / %.1f %s", float64(done)/float64(div), float64(total)/float64(div), unit)
}

func byteUnit(n int64) (int64, string) {
	const unit = 1024
	if n < unit {
		return 1, "B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return div, string("KMGTPE"[exp]) + "iB"
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

// ShortID cuts a snapshot ID to the 8 characters the tables show. Every
// command taking an ID resolves that prefix; one that two snapshots share
// fails with the ambiguous-ID error.
func ShortID(id ark.SnapshotID) string {
	if len(id) > 8 {
		return string(id[:8])
	}
	return string(id)
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
