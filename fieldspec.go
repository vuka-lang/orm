package orm

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vuka-lang/vuka"
)

// A field's Go type may say how its values reach the database and come
// back, beside database/sql's Scanner and Valuer, with any of these
// methods; each is optional and independent of the others. Where a type
// has both forms, the ORM's methods win: the ORM calls DBValue rather
// than Value and FromDB rather than Scan (database/sql's stay for code
// handing the type to the driver itself, such as Raw's arguments).
//
//	type Phone string
//
//	func (p Phone) DBValue(d orm.Dialect) (any, error) { return strings.ReplaceAll(string(p), " ", ""), nil }
//	func (p *Phone) FromDB(src any) error             { *p = Phone(fmt.Sprint(src)); return nil }
//	func (Phone) ColumnType(d orm.Dialect) string     { return "VARCHAR(16)" }
//	func (p Phone) Validate() error {
//		if !strings.HasPrefix(string(p), "+") {
//			return errors.New("must start with a country code")
//		}
//		return nil
//	}
type (
	// DBValuer gives the value written to the column for the dialect d
	// (d.Name() is "postgres", "mysql" or "sqlite"): on Create, Save and
	// Update, and for the conditions comparing the field (Eq, In, Gt, …).
	DBValuer interface {
		DBValue(d Dialect) (any, error)
	}
	// DBScanner reads the column's value, as the driver gives it (nil for
	// NULL unless the field is a pointer or an Option, which are nil and
	// None then).
	DBScanner interface {
		FromDB(src any) error
	}
	// ColumnTyper is the column's database type on dialect d, as
	// migrations make it: "" leaves the type the ORM would choose.
	ColumnTyper interface {
		ColumnType(d Dialect) string
	}
	// Validator checks a value before it is written: a non-nil error
	// refuses the write with an Invalid error naming the column.
	Validator interface {
		Validate() error
	}
)

var (
	dbValuerType    = reflect.TypeFor[DBValuer]()
	dbScannerType   = reflect.TypeFor[DBScanner]()
	columnTyperType = reflect.TypeFor[ColumnTyper]()
	validatorType   = reflect.TypeFor[Validator]()
)

// implements is whether t, or a pointer to it, has the methods of iface.
func implements(t, iface reflect.Type) bool {
	return t.Implements(iface) || t.Kind() != reflect.Pointer && reflect.PointerTo(t).Implements(iface)
}

// customValue is whether t carries its values itself the ORM's way.
func customValue(t reflect.Type) bool {
	return implements(t, dbValuerType) || implements(t, dbScannerType)
}

// methodsOf is v as an iface, through a pointer to a copy when the
// method has a pointer receiver; ok false when it has none.
func methodsOf[I any](v reflect.Value) (I, bool) {
	if x, ok := v.Interface().(I); ok {
		return x, true
	}
	p := reflect.New(v.Type())
	p.Elem().Set(v)
	x, ok := p.Interface().(I)
	return x, ok
}

// conv is what happens to a field's values on their way to the column.
type conv uint8

const (
	convSchema conv = 1 << iota // a SchemaValuer: its value for the names set
	convDB                      // a DBValuer: its value for the dialect
	convTime                    // UTC, Precision or Date: the time adjusted
)

// fieldSpec is what a field's options say beyond its column's name and
// keys: its column's details, the checks and normalizing of its writes.
type fieldSpec struct {
	comment, collation, label, help string
	precision                       int
	size                            IntSize
	unsigned, utc, date             bool
	manual, uuid                    bool
	initial                         any
	choices                         []any // normalized (see choiceKey)
	minLen, maxLen                  int
	min, max                        vuka.Option[int64]
	pattern                         *regexp.Regexp
	trim, lower                     bool
	checks                          []func(any) error
	validates                       bool // the field's type is a Validator
	check                           bool // a write has something to check
}

// valueType is the type a field holds a value of: t without its pointer
// or Option.
func valueType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if isOption(t) {
		return valueType(optionElem(t))
	}
	return t
}

func isInt(k reflect.Kind) bool {
	return k >= reflect.Int && k <= reflect.Uint64
}

// settle gives f what its options say beyond its tags: its spec, the
// conversions of its values, how its cells read.
func (f *field) settle(m *model, o FieldOptions) error {
	base := valueType(f.Type)
	s := &fieldSpec{
		comment: o.Comment, collation: o.Collation, label: o.Label, help: o.Help,
		precision: o.Precision, size: o.Size, unsigned: o.Unsigned, utc: o.UTC, date: o.Date,
		manual: o.Manual, uuid: o.UUID, initial: o.Initial, minLen: o.MinLength,
		min: o.Min, max: o.Max, trim: o.Trim, lower: o.Lower, checks: o.Checks,
	}
	bad := func(what, want string) error {
		return fmt.Errorf("orm: %s.%s: %s is for %s, not %s", m.Name, f.Name, what, want, f.Type)
	}
	if o.err != nil {
		return fmt.Errorf("orm: %s.%s: %w", m.Name, f.Name, o.err)
	}
	isTime := base == timeType
	switch {
	case f.fk:
	case (o.Date || o.UTC || o.Precision > 0) && !isTime:
		return bad("Date, Datetime's Precision and UTC", "a time.Time field")
	case o.Precision > 6:
		return fmt.Errorf("orm: %s.%s: Precision is 0 to 6 fractional digits, not %d", m.Name, f.Name, o.Precision)
	case (o.Size != 0 || o.Unsigned) && !isInt(base.Kind()):
		return bad("an integer's Size and Unsigned", "an integer field")
	case o.UUID && base.Kind() != reflect.String:
		return bad("a UUID key", "a string field")
	case (o.Trim || o.Lower || o.Pattern != "" || o.Collation != "" || o.MinLength > 0) && base.Kind() != reflect.String:
		return bad("Trim, Lower, Pattern, Collation and Min length", "a text field")
	}
	if o.Pattern != "" {
		re, err := regexp.Compile(o.Pattern)
		if err != nil {
			return fmt.Errorf("orm: %s.%s: Pattern: %w", m.Name, f.Name, err)
		}
		s.pattern = re
	}
	if o.Unsigned && (s.min.IsNone() || s.min.Value() < 0) {
		s.min = vuka.Some[int64](0)
	}
	if base.Kind() == reflect.String {
		s.maxLen = f.Size
	}
	for _, c := range o.Choices {
		s.choices = append(s.choices, choiceKey(reflect.ValueOf(c)))
	}
	if o.Choices != nil && s.choices == nil {
		s.choices = []any{}
	}
	s.validates = implements(base, validatorType)
	s.check = s.maxLen > 0 || s.minLen > 0 || s.pattern != nil || s.min.IsSome() || s.max.IsSome() ||
		s.choices != nil || len(s.checks) > 0 || s.validates
	f.spec = s

	if schemaValued(f) != nil {
		f.conv |= convSchema
	}
	if implements(base, dbValuerType) {
		f.conv |= convDB
	}
	if isTime && (s.utc || s.date || s.precision > 0) {
		f.conv |= convTime
	}
	if s.utc && f.fast != scanSchema {
		f.fast = scanUTC
	}
	return nil
}

// choiceKey is a value as choices compare it: an integer of any width
// as an int64, a text of any string type as a string.
func choiceKey(v reflect.Value) any {
	switch k := v.Kind(); {
	case k == reflect.String:
		return v.String()
	case k >= reflect.Int && k <= reflect.Int64:
		return v.Int()
	case k >= reflect.Uint && k <= reflect.Uint64:
		return int64(v.Uint())
	case k == reflect.Float32 || k == reflect.Float64:
		return v.Float()
	case k == reflect.Bool:
		return v.Bool()
	}
	return v.Interface()
}

// derefValue is x without its pointers and Option: ok false for nil and
// None, which every check lets through (NULL).
func derefValue(x any) (reflect.Value, bool) {
	v := reflect.ValueOf(x)
	for v.IsValid() {
		switch {
		case v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface:
			if v.IsNil() {
				return v, false
			}
			v = v.Elem()
		case isOption(v.Type()):
			val, ok := optionParts(v)
			if !ok.Bool() {
				return v, false
			}
			v = val
		default:
			return v, true
		}
	}
	return v, false
}

// problems is what x, a value written to f, breaks of f's checks.
func (f *field) problems(x any) []string {
	s := f.spec
	if s == nil || !s.check {
		return nil
	}
	if _, ok := x.(Expr); ok {
		return nil
	}
	if _, ok := x.(subquerier); ok {
		return nil
	}
	v, ok := derefValue(x)
	if !ok {
		return nil
	}
	var out []string
	bounds := func(n int64) {
		if lo, ok := s.min.Get(); ok && n < lo {
			out = append(out, fmt.Sprintf("must be at least %d", lo))
		}
		if hi, ok := s.max.Get(); ok && n > hi {
			out = append(out, fmt.Sprintf("must be at most %d", hi))
		}
	}
	switch k := v.Kind(); {
	case k == reflect.String:
		str := v.String()
		n := utf8.RuneCountInString(str)
		if s.maxLen > 0 && n > s.maxLen {
			out = append(out, fmt.Sprintf("must be at most %d characters", s.maxLen))
		}
		if s.minLen > 0 && n < s.minLen {
			out = append(out, fmt.Sprintf("must be at least %d characters", s.minLen))
		}
		if s.pattern != nil && !s.pattern.MatchString(str) {
			out = append(out, "must match "+s.pattern.String())
		}
	case k >= reflect.Int && k <= reflect.Int64:
		bounds(v.Int())
	case k >= reflect.Uint && k <= reflect.Uint64:
		bounds(int64(v.Uint()))
	case k == reflect.Float32 || k == reflect.Float64:
		n := v.Float()
		if lo, ok := s.min.Get(); ok && n < float64(lo) {
			out = append(out, fmt.Sprintf("must be at least %d", lo))
		}
		if hi, ok := s.max.Get(); ok && n > float64(hi) {
			out = append(out, fmt.Sprintf("must be at most %d", hi))
		}
	}
	if s.choices != nil && !slices.Contains(s.choices, choiceKey(v)) {
		names := make([]string, len(s.choices))
		for i, c := range s.choices {
			names[i] = fmt.Sprint(c)
		}
		out = append(out, "must be one of "+strings.Join(names, ", "))
	}
	if s.validates {
		if vd, ok := methodsOf[Validator](v); ok {
			if err := vd.Validate(); err != nil {
				out = append(out, err.Error())
			}
		}
	}
	for _, c := range s.checks {
		if err := c(v.Interface()); err != nil {
			out = append(out, err.Error())
		}
	}
	return out
}

// invalid is the Invalid error of the messages by column, nil for none;
// its message names the first column of the model's order.
func (m *model) invalid(byCol map[string][]string) error {
	if len(byCol) == 0 {
		return nil
	}
	first := ""
	for _, f := range m.Fields {
		if msgs, ok := byCol[f.Column]; ok {
			first = f.Column + " " + msgs[0]
			break
		}
	}
	e := errorf(Invalid, m.Name, "%s is invalid: %s", m.Name, first)
	e.Fields = byCol
	return e
}

// checkRow is the Invalid error of row's fields (those of only, every
// one for nil) breaking their checks.
func (m *model) checkRow(row reflect.Value, only []*field) error {
	var byCol map[string][]string
	for _, f := range m.Fields {
		if f.spec == nil || !f.spec.check || only != nil && !slices.Contains(only, f) {
			continue
		}
		if msgs := f.problems(value(peek(row, f.Index, f.Type))); len(msgs) > 0 {
			if byCol == nil {
				byCol = map[string][]string{}
			}
			byCol[f.Column] = append(byCol[f.Column], msgs...)
		}
	}
	return m.invalid(byCol)
}

// normalize applies f's Trim and Lower to its text in row, and its UTC
// and Precision to its time: the row holds what the column will.
func (f *field) normalize(row reflect.Value) {
	if f.spec == nil || !f.spec.trim && !f.spec.lower && (f.conv&convTime == 0 || f.spec.date) {
		return
	}
	dst := fieldOf(row, f.Index)
	for dst.Kind() == reflect.Pointer {
		if dst.IsNil() {
			return
		}
		dst = dst.Elem()
	}
	if isOption(dst.Type()) {
		val, ok := optionParts(dst)
		if !ok.Bool() {
			return
		}
		dst = val
	}
	switch {
	case !dst.CanSet():
	case dst.Kind() == reflect.String:
		dst.SetString(f.spec.norm(dst.String()))
	case dst.Type() == timeType && !dst.IsZero():
		dst.Set(reflect.ValueOf(f.spec.timeOut(nil, dst.Interface().(time.Time))))
	}
}

func (s *fieldSpec) norm(str string) string {
	if s.trim {
		str = strings.TrimSpace(str)
	}
	if s.lower {
		str = strings.ToLower(str)
	}
	return str
}

// normValue is x, a value Update sets f to, with f's Trim and Lower
// applied when it is a text.
func (f *field) normValue(x any) any {
	if f.spec == nil || !f.spec.trim && !f.spec.lower || x == nil {
		return x
	}
	v := reflect.ValueOf(x)
	if v.Kind() != reflect.String {
		return x
	}
	n := reflect.New(v.Type()).Elem()
	n.SetString(f.spec.norm(v.String()))
	return n.Interface()
}

// prepareFields sets row's fields as a create (or, with create false, a
// save of only, every field for nil) writes them: a zero field its
// Initial, an empty UUID key a new one, texts normalized.
func (m *model) prepareFields(row reflect.Value, create bool, only []*field) error {
	for _, f := range m.Fields {
		s := f.spec
		if s == nil || only != nil && !slices.Contains(only, f) {
			continue
		}
		if create && (s.initial != nil || s.uuid) {
			dst := fieldOf(row, f.Index)
			if dst.IsZero() {
				var v any = s.initial
				if s.uuid {
					v = newUUID()
				}
				if err := setField(dst, v); err != nil {
					return fmt.Errorf("orm: %s.%s's default: %w", m.Name, f.Name, err)
				}
			}
		}
		f.normalize(row)
	}
	return nil
}

// newUUID is a random (version 4) UUID.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// convert is v, a value of f, as f's column takes it on d: a DBValuer's
// value, a time adjusted. nil and None are nil.
func (f *field) convert(m *model, d Dialect, v any) (any, error) {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, nil
		}
		if f.conv&convDB != 0 {
			if dv, ok := rv.Interface().(DBValuer); ok {
				return dbValue(m, f, dv, d)
			}
		}
		rv = rv.Elem()
	}
	if isOption(rv.Type()) {
		val, ok := optionParts(rv)
		if !ok.Bool() {
			return nil, nil
		}
		rv = val
	}
	if f.conv&convDB != 0 {
		if dv, ok := methodsOf[DBValuer](rv); ok {
			return dbValue(m, f, dv, d)
		}
	}
	if f.conv&convTime != 0 && rv.Type() == timeType {
		return f.spec.timeOut(d, rv.Interface().(time.Time)), nil
	}
	return v, nil
}

func dbValue(m *model, f *field, dv DBValuer, d Dialect) (any, error) {
	if d == nil {
		d = sqlite{}
	}
	x, err := dv.DBValue(d)
	if err != nil {
		return nil, fmt.Errorf("orm: %s.%s: %w", m.Name, f.Name, err)
	}
	return x, nil
}

// timeOut is t as a column of s takes it: in UTC, cut to its precision,
// or its day alone.
func (s *fieldSpec) timeOut(d Dialect, t time.Time) any {
	if s.utc {
		t = t.UTC()
	}
	if s.date {
		y, mo, day := t.Date()
		if d != nil && d.Name() == "postgres" {
			return time.Date(y, mo, day, 0, 0, 0, 0, time.UTC)
		}
		return fmt.Sprintf("%04d-%02d-%02d", y, mo, day)
	}
	if s.precision > 0 {
		step := time.Duration(1)
		for range 9 - s.precision {
			step *= 10
		}
		t = t.Truncate(step)
	}
	return t
}

// inUTC sets a time read into dst (a time.Time, or a pointer or Option
// holding one) to UTC.
func inUTC(dst reflect.Value) {
	for dst.Kind() == reflect.Pointer {
		if dst.IsNil() {
			return
		}
		dst = dst.Elem()
	}
	if isOption(dst.Type()) {
		val, ok := optionParts(dst)
		if !ok.Bool() {
			return
		}
		dst = val
	}
	if dst.Type() == timeType && dst.CanSet() {
		dst.Set(reflect.ValueOf(dst.Interface().(time.Time).UTC()))
	}
}

// FieldInfo is a model's column as forms, admin pages and tools read it:
// what its field attributes, tags and SetFieldOptions declare.
type FieldInfo struct {
	Name   string // the Go field's name
	Column string
	Type   reflect.Type
	PK     bool
	Null   bool // the column takes NULL
	Unique bool
	Index  bool
	// MaxLength and MinLength bound a text's characters (0: no bound);
	// Min and Max a number; Choices is the values it may take (nil: any);
	// Pattern the regular expression a text matches ("" for none).
	MaxLength, MinLength int
	Min, Max             vuka.Option[int64]
	Choices              []any
	Pattern              string
	// Default is the column's SQL default ("" for none); Initial its value
	// on Create when left empty.
	Default              string
	Initial              any
	Label, Help, Comment string
	AutoNow, AutoNowAdd  bool
}

// Fields is the model's columns, in field order, as forms and tools read
// them.
func (m *Manager[T]) Fields() []FieldInfo {
	if m.meta == nil {
		return nil
	}
	out := make([]FieldInfo, 0, len(m.meta.Fields))
	for _, f := range m.meta.Fields {
		c := columnOf(f)
		fi := FieldInfo{Name: f.Name, Column: f.Column, Type: f.Type, PK: f.PK, Null: c.Nullable, Unique: f.Unique, Index: f.Indexed,
			Default: f.Default, AutoNow: f.AutoNow, AutoNowAdd: f.AutoNowAdd}
		if s := f.spec; s != nil {
			fi.MaxLength, fi.MinLength, fi.Min, fi.Max = s.maxLen, s.minLen, s.min, s.max
			fi.Choices, fi.Initial = slices.Clone(s.choices), s.initial
			fi.Label, fi.Help, fi.Comment = s.label, s.help, s.comment
			if s.pattern != nil {
				fi.Pattern = s.pattern.String()
			}
		}
		out = append(out, fi)
	}
	return out
}

// counted is whether the database gives m's rows their keys: an integer
// key not Manual.
func (m *model) counted() bool {
	pk := m.PK
	if pk == nil {
		return false
	}
	if s := pk.spec; s != nil && (s.manual || s.uuid) {
		return false
	}
	return true
}
