package orm

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// CSVElem is what a CSV column's list may hold.
type CSVElem interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 | ~uint | ~uint16 | ~uint32 | ~uint64 | ~string
}

// CSV is a column holding a list as comma-separated text ("7,9,11"), for
// legacy tables that keep lists in a string: read and write V; the field
// marshals as the list. A nil V is NULL, an empty non-nil V ""; reading
// trims spaces around the commas.
//
//	type Letter struct {
//		ID           int64
//		PlacementIDs orm.CSV[int64]
//	}
type CSV[E CSVElem] struct{ V []E }

// CSVOf is the CSV column holding v.
func CSVOf[E CSVElem](v ...E) CSV[E] { return CSV[E]{V: v} }

func (c CSV[E]) Value() (driver.Value, error) {
	if c.V == nil {
		return nil, nil
	}
	parts := make([]string, len(c.V))
	for i, e := range c.V {
		v := reflect.ValueOf(e)
		switch {
		case v.Kind() == reflect.String:
			s := v.String()
			if strings.ContainsRune(s, ',') {
				return nil, fmt.Errorf("orm: a CSV element can't hold a comma: %q", s)
			}
			parts[i] = s
		case v.CanInt():
			parts[i] = strconv.FormatInt(v.Int(), 10)
		default:
			parts[i] = strconv.FormatUint(v.Uint(), 10)
		}
	}
	return strings.Join(parts, ","), nil
}

func (c *CSV[E]) Scan(src any) error {
	c.V = nil
	var s string
	switch x := src.(type) {
	case nil:
		return nil
	case []byte:
		s = string(x)
	case string:
		s = x
	default:
		return fmt.Errorf("orm: a CSV column holds text, not %T", src)
	}
	c.V = []E{}
	et := reflect.TypeFor[E]()
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		e := reflect.New(et).Elem()
		if et.Kind() == reflect.String {
			e.SetString(part)
		} else {
			n, err := strconv.ParseInt(part, 10, 64)
			if err != nil {
				return fmt.Errorf("orm: a CSV of %s holds %q", et, part)
			}
			if e.CanInt() {
				e.SetInt(n)
			} else {
				e.SetUint(uint64(n))
			}
		}
		c.V = append(c.V, e.Interface().(E))
	}
	return nil
}

func (c CSV[E]) MarshalJSON() ([]byte, error) { return json.Marshal(c.V) }

func (c *CSV[E]) UnmarshalJSON(b []byte) error { return json.Unmarshal(b, &c.V) }

func (CSV[E]) csvColumn() {}

var csvColType = reflect.TypeFor[interface{ csvColumn() }]()
