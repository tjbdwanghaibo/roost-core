package dataengine

import (
	"strconv"
	"strings"
)

// ResolveDatabaseScope keeps database scoping optional on DAO types while
// making Data Engine the owner of the generated persistence contract.
func ResolveDatabaseScope(value any) DatabaseScope {
	if scoped, ok := value.(interface{ DbScope() DatabaseScope }); ok {
		return scoped.DbScope()
	}
	return DatabaseGlobal
}

// MapPatchPath builds a Mongo-safe field/key path. Keys that cannot be
// represented safely force codegen to emit a full-field patch instead.
func MapPatchPath(field string, key any) (string, bool) {
	if field == "" {
		return "", false
	}
	keyText, ok := mapPatchKeyString(key)
	if !ok || keyText == "" || strings.ContainsAny(keyText, ".\x00") || strings.HasPrefix(keyText, "$") {
		return "", false
	}
	return field + "." + keyText, true
}

func mapPatchKeyString(key any) (string, bool) {
	switch value := key.(type) {
	case string:
		return value, value != ""
	case int:
		return strconv.FormatInt(int64(value), 10), true
	case int8:
		return strconv.FormatInt(int64(value), 10), true
	case int16:
		return strconv.FormatInt(int64(value), 10), true
	case int32:
		return strconv.FormatInt(int64(value), 10), true
	case int64:
		return strconv.FormatInt(value, 10), true
	case uint:
		return strconv.FormatUint(uint64(value), 10), true
	case uint8:
		return strconv.FormatUint(uint64(value), 10), true
	case uint16:
		return strconv.FormatUint(uint64(value), 10), true
	case uint32:
		return strconv.FormatUint(uint64(value), 10), true
	case uint64:
		return strconv.FormatUint(value, 10), true
	default:
		return "", false
	}
}
