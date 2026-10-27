package mangrove

import (
	"fmt"
	"strconv"
	"strings"
)

// CacheResolutionError reports a cache resolution that can't be built or
// interpreted.
type CacheResolutionError struct {
	Message string
}

func (e *CacheResolutionError) Error() string { return e.Message }

/*
 * Cache resolution aliases encode the data coordinates they should be merged
 * back into, e.g. `Query.dashboard.my_field` -> `_ENTITY_dashboard__my_1field_i0`.
 *
 * Every underscore emitted by the encoder is the first half of a pair:
 *   `__`  -> `.`
 *   `_1`  -> a literal `_`
 *   `_i<n>` -> the (unique) resolution index, always last
 * which makes the encoding reversible for any valid GraphQL name.
 */

func encodeCoordinates(coordinates string) string {
	var out strings.Builder
	for _, char := range coordinates {
		switch char {
		case '_':
			out.WriteString("_1")
		case '.':
			out.WriteString("__")
		default:
			out.WriteRune(char)
		}
	}
	return out.String()
}

func getCacheResolverAlias(coordinates string, index int) string {
	relative := strings.TrimPrefix(coordinates, rootQuery+".")
	return PartialCacheAliasPrefix + encodeCoordinates(relative) + "_i" + strconv.Itoa(index)
}

func isCacheAliasName(name string) bool {
	return strings.HasPrefix(name, PartialCacheAliasPrefix)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseCacheResolverAlias returns the absolute coordinates (`Query.…`)
// encoded in a cache resolver alias.
func parseCacheResolverAlias(alias string) (string, error) {
	encoded := alias[len(PartialCacheAliasPrefix):]
	var out strings.Builder

	for i := 0; i < len(encoded); i++ {
		char := encoded[i]

		if char != '_' {
			out.WriteByte(char)
			continue
		}

		var next byte
		if i+1 < len(encoded) {
			next = encoded[i+1]
		}

		switch {
		case next == '_':
			out.WriteByte('.')
			i++
		case next == '1':
			out.WriteByte('_')
			i++
		case next == 'i' && isDigits(encoded[i+2:]):
			return rootQuery + "." + out.String(), nil
		default:
			return "", &CacheResolutionError{Message: fmt.Sprintf("Malformed cache resolver alias: %s", alias)}
		}
	}

	return rootQuery + "." + out.String(), nil
}
