package xregexp

import (
	"fmt"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/looplj/axonhub/internal/pkg/xmap"
)

// searchMatchTimeout caps a single non-anchored search match. Search patterns
// come from user configuration and run on every matching request, so a
// catastrophic backtracking pattern must not stall that request. The timeout is
// checked by regexp2's shared clock, so it adds no per-call goroutine.
const searchMatchTimeout = 50 * time.Millisecond

// searchPatternCache is the prepared form of one non-anchored search pattern.
// literal marks patterns without regex metacharacters, which are matched with a
// plain substring search instead of a compiled expression.
type searchPatternCache struct {
	regex      *regexp2.Regexp
	literal    bool
	compileErr bool
	matchAll   bool
}

var globalSearchCache = xmap.New[string, *searchPatternCache]()

/**
 * SearchString reports whether pattern matches anywhere inside str, using
 * non-anchored "search" semantics. This is the counterpart of MatchString,
 * which anchors the pattern to the whole string.
 *
 * It keeps the MatchString conventions: "*" matches everything, patterns
 * without regex metacharacters are matched literally (here as a substring
 * search), and an invalid pattern reports false instead of panicking. A match
 * that exceeds searchMatchTimeout is also reported as false.
 */
func SearchString(pattern string, str string) bool {
	cached := getOrCreateSearchPattern(pattern)

	if cached.compileErr {
		return false
	}

	if cached.matchAll {
		return true
	}

	if cached.literal {
		return strings.Contains(str, pattern)
	}

	match, err := cached.regex.MatchString(str)
	if err != nil {
		// The only expected error is a match timeout; treat it as "no match".
		return false
	}

	return match
}

/**
 * ValidateSearchRegex reports whether pattern is usable by SearchString. An
 * empty pattern is accepted here (matching ValidateRegex) because callers that
 * require a value validate it separately.
 */
func ValidateSearchRegex(pattern string) error {
	if pattern == "" {
		return nil
	}

	if cached := getOrCreateSearchPattern(pattern); cached.compileErr {
		return fmt.Errorf("invalid regex pattern: %s", pattern)
	}

	return nil
}

/**
 * getOrCreateSearchPattern returns the cached compiled form of pattern,
 * compiling it at most once per pattern for the process lifetime.
 */
func getOrCreateSearchPattern(pattern string) *searchPatternCache {
	if cached, ok := globalSearchCache.Load(pattern); ok {
		return cached
	}

	cached := &searchPatternCache{}

	switch {
	case pattern == "*":
		cached.matchAll = true
	case !containsRegexChars(pattern):
		cached.literal = true
	default:
		compiled, err := regexp2.Compile(pattern, regexp2.None)
		if err != nil {
			cached.compileErr = true
		} else {
			// Set before the pattern is shared: Regexp.MatchTimeout is read by
			// concurrent matches and must not change afterwards.
			compiled.MatchTimeout = searchMatchTimeout
			cached.regex = compiled
		}
	}

	globalSearchCache.Store(pattern, cached)

	return cached
}
