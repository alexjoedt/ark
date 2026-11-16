package ark

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ReservedLabelPrefix starts the label keys ark's own sources record. A
// caller's labels must not use it.
const ReservedLabelPrefix = "ark/"

const (
	maxLabelPrefix = 253
	maxLabelName   = 63
	maxLabelValue  = 1024
)

var (
	labelPrefixRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*$`)
	labelNameRE   = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
)

// ValidateLabels checks labels a caller sets with [WithLabels], the rules
// Backup applies before it writes anything. A key follows the Kubernetes
// label key syntax: an optional prefix, a lower-case DNS subdomain of at most
// 253 characters with dot-separated parts of at most 63, and a slash, then a name of 1 to 63 characters out of
// letters, digits, '-', '_' and '.', starting and ending with a letter or
// digit. A key must not start with [ReservedLabelPrefix]. A value is UTF-8 of
// at most 1 KiB without control characters, and may be empty. Errors wrap
// [ErrInvalidLabel].
func ValidateLabels(labels map[string]string) error {
	return checkBackupLabels(nil, labels)
}

func validateLabel(key, value string) error {
	if strings.HasPrefix(key, ReservedLabelPrefix) {
		return fmt.Errorf("%w: key %q: prefix %q is reserved for ark", ErrInvalidLabel, key, ReservedLabelPrefix)
	}
	name := key
	if prefix, rest, ok := strings.Cut(key, "/"); ok {
		if len(prefix) > maxLabelPrefix || !labelPrefixRE.MatchString(prefix) {
			return fmt.Errorf("%w: key %q: prefix must be a lower-case DNS subdomain of at most %d characters, "+
				"parts of at most 63", ErrInvalidLabel, key, maxLabelPrefix)
		}
		name = rest
	}
	if len(name) > maxLabelName || !labelNameRE.MatchString(name) {
		return fmt.Errorf("%w: key %q: name must be 1 to %d letters, digits, '-', '_' or '.', "+
			"starting and ending with a letter or digit", ErrInvalidLabel, key, maxLabelName)
	}
	if len(value) > maxLabelValue {
		return fmt.Errorf("%w: key %q: value is longer than %d bytes", ErrInvalidLabel, key, maxLabelValue)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: key %q: value is not valid UTF-8", ErrInvalidLabel, key)
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: key %q: value holds a control character", ErrInvalidLabel, key)
	}
	return nil
}

// ValidateBackupOptions checks the labels opts set with [ValidateLabels], the
// check Backup runs before it writes anything, short of the keys the source
// sets as well. A caller that does work before Backup, such as stopping the
// containers of a volume, runs it first.
func ValidateBackupOptions(opts ...BackupOption) error {
	o := &backupOptions{}
	for _, opt := range opts {
		opt(o)
	}
	return ValidateLabels(o.labels)
}

// checkBackupLabels validates the caller's labels and refuses a key the
// source set as well. The source's own labels are not validated: a
// third-party source may record any key.
func checkBackupLabels(source, caller map[string]string) error {
	for _, k := range slices.Sorted(maps.Keys(caller)) {
		if err := validateLabel(k, caller[k]); err != nil {
			return err
		}
		if _, ok := source[k]; ok {
			return fmt.Errorf("%w: key %q: the source sets it", ErrInvalidLabel, k)
		}
	}
	return nil
}

// mergeLabels returns the source's labels with the caller's added, nil when
// both are empty.
func mergeLabels(source, caller map[string]string) map[string]string {
	if len(source)+len(caller) == 0 {
		return nil
	}
	merged := make(map[string]string, len(source)+len(caller))
	maps.Copy(merged, source)
	maps.Copy(merged, caller)
	return merged
}
