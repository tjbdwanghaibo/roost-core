package skill

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

func rejectDuplicateKeys(data []byte) error {
	return rejectDuplicateKeysWithLimits(data, DefaultParseLimits())
}

type jsonScanBudget struct {
	limits ParseLimits
	tokens int
}

func rejectDuplicateKeysWithLimits(data []byte, limits ParseLimits) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	budget := &jsonScanBudget{limits: limits}
	if err := scanJSONValue(decoder, "$", nil, 1, budget); err != nil {
		return err
	}
	token, err := decoder.Token()
	if err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("unexpected second JSON value starting with %v", token)
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, path string, first json.Token, depth int, budget *jsonScanBudget) error {
	if depth > budget.limits.MaxDepth {
		return fmt.Errorf("%w: depth %d > %d at %s", ErrParseLimitExceeded, depth, budget.limits.MaxDepth, path)
	}
	token := first
	var err error
	if token == nil {
		token, err = decoder.Token()
		if err != nil {
			return err
		}
	}
	budget.tokens++
	if budget.tokens > budget.limits.MaxTokens {
		return fmt.Errorf("%w: tokens %d > %d", ErrParseLimitExceeded, budget.tokens, budget.limits.MaxTokens)
	}
	if value, ok := token.(string); ok && len(value) > budget.limits.MaxStringBytes {
		return fmt.Errorf("%w: string bytes %d > %d at %s", ErrParseLimitExceeded, len(value), budget.limits.MaxStringBytes, path)
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		entries := 0
		for decoder.More() {
			entries++
			if entries > budget.limits.MaxContainerEntries {
				return fmt.Errorf("%w: object entries %d > %d at %s", ErrParseLimitExceeded, entries, budget.limits.MaxContainerEntries, path)
			}
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("%s: object key is not a string", path)
			}
			budget.tokens++
			if budget.tokens > budget.limits.MaxTokens || len(key) > budget.limits.MaxStringBytes {
				return fmt.Errorf("%w: object key or token budget exceeded at %s", ErrParseLimitExceeded, path)
			}
			childPath := jsonObjectPath(path, key)
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate key at %s", childPath)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder, childPath, nil, depth+1, budget); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("%s: malformed object", path)
		}
	case '[':
		for index := 0; decoder.More(); index++ {
			if index >= budget.limits.MaxContainerEntries {
				return fmt.Errorf("%w: array entries exceed %d at %s", ErrParseLimitExceeded, budget.limits.MaxContainerEntries, path)
			}
			if err := scanJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index), nil, depth+1, budget); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("%s: malformed array", path)
		}
	default:
		return fmt.Errorf("%s: unexpected delimiter %q", path, delimiter)
	}
	return nil
}

func jsonObjectPath(path, key string) string {
	if path == "$" {
		return "$." + key
	}
	return path + "." + key
}
