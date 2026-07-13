package dataplane

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

func CanonicalJSON(value any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeCanonical(&buf, value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func ETag(value any) (string, error) {
	b, err := CanonicalJSON(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return `"` + base64.RawURLEncoding.EncodeToString(sum[:]) + `"`, nil
}

func writeCanonical(buf *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(v))
	case string:
		b, _ := json.Marshal(v)
		buf.Write(b)
	case json.Number:
		buf.WriteString(v.String())
	case float64:
		buf.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
	case float32:
		buf.WriteString(strconv.FormatFloat(float64(v), 'g', -1, 32))
	case int:
		buf.WriteString(strconv.Itoa(v))
	case int64:
		buf.WriteString(strconv.FormatInt(v, 10))
	case int32:
		buf.WriteString(strconv.FormatInt(int64(v), 10))
	case uint64:
		buf.WriteString(strconv.FormatUint(v, 10))
	case uint:
		buf.WriteString(strconv.FormatUint(uint64(v), 10))
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			keyJSON, _ := json.Marshal(key)
			buf.Write(keyJSON)
			buf.WriteByte(':')
			if err := writeCanonical(buf, v[key]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("canonical JSON: %w", err)
		}
		var normalized any
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()
		if err := decoder.Decode(&normalized); err != nil {
			return err
		}
		return writeCanonical(buf, normalized)
	}
	return nil
}
