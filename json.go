package orm

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"reflect"
)

// JSON is a column holding V as JSON, Django's JSONField: jsonb on
// Postgres, JSON on MySQL, TEXT on SQLite. Read and write V; the field
// marshals as V alone, so a response carries the value, not a wrapper.
// A nil V (slice, map or pointer) is NULL, as a nil []byte is.
//
//	type Letter struct {
//		ID           int64
//		PlacementIDs orm.JSON[[]int64]
//	}
//	letter.PlacementIDs.V = append(letter.PlacementIDs.V, 7)
type JSON[T any] struct{ V T }

// JSONOf is the JSON column holding v.
func JSONOf[T any](v T) JSON[T] { return JSON[T]{V: v} }

func (j JSON[T]) Value() (driver.Value, error) {
	if v := reflect.ValueOf(j.V); !v.IsValid() || nilable(v.Kind()) && v.IsNil() {
		return nil, nil
	}
	b, err := json.Marshal(j.V)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

func (j *JSON[T]) Scan(src any) error {
	var zero T
	j.V = zero
	switch s := src.(type) {
	case nil:
		return nil
	case []byte:
		if len(s) == 0 {
			return nil
		}
		return json.Unmarshal(s, &j.V)
	case string:
		if s == "" {
			return nil
		}
		return json.Unmarshal([]byte(s), &j.V)
	}
	return fmt.Errorf("orm: a JSON column holds text, not %T", src)
}

func (j JSON[T]) MarshalJSON() ([]byte, error) { return json.Marshal(j.V) }

func (j *JSON[T]) UnmarshalJSON(b []byte) error { return json.Unmarshal(b, &j.V) }

func (JSON[T]) jsonColumn() {}

func nilable(k reflect.Kind) bool {
	return k == reflect.Pointer || k == reflect.Slice || k == reflect.Map
}

var jsonColType = reflect.TypeFor[interface{ jsonColumn() }]()
