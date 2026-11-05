package orm

import (
	"reflect"
	"testing"
)

// The bases have the declaration hooks, declaring nothing.
var (
	_ IndexesDeclarer   = Model[hkBase]{}
	_ ChecksDeclarer    = Model[hkBase]{}
	_ GeneratedDeclarer = Model[hkBase]{}
	_ IndexesDeclarer   = Base[hkBase]{}
	_ ChecksDeclarer    = Base[hkBase]{}
	_ GeneratedDeclarer = Base[hkBase]{}
)

type hkPlain struct {
	ID   int64
	Name string
}

type hkBase struct {
	Model[hkBase]
	ID   int64
	Name string
}

type hkValue struct {
	Model[hkValue]
	ID   int64
	Name string
	Low  string `orm:"generated"`
}

func (hkValue) Indexes() []Index { return []Index{IndexOf("name")} }
func (hkValue) Checks() []RowCheck {
	return []RowCheck{CheckOf(Q{"name__isnull": false})}
}
func (hkValue) Generated() map[string]Expr { return map[string]Expr{"Low": Lower.Of(F("name"))} }

type hkPointer struct {
	Model[hkPointer]
	ID   int64
	Name string
	Low  string `orm:"generated"`
}

func (*hkPointer) Indexes() []Index { return []Index{IndexOf("name")} }
func (*hkPointer) Checks() []RowCheck {
	return []RowCheck{CheckOf(Q{"name__isnull": false})}
}
func (*hkPointer) Generated() map[string]Expr { return map[string]Expr{"Low": Lower.Of(F("name"))} }

type hkMeta struct {
	Model[hkMeta]
	ID   int64
	Name string
}

func (*hkMeta) Schema(s *Schema) { s.Model(Meta{Indexes: []Index{IndexOf("id", "name")}}) }
func (hkMeta) Indexes() []Index  { return []Index{IndexOf("name")} }

// hkStamps and hkAudit are structs models embed for their indexes.
type (
	hkStamps  struct{ Name string }
	hkAudit   struct{ Note string }
	hkWrapped struct{ hkStamps }
)

func (hkStamps) Indexes() []Index { return []Index{IndexOf("name")} }
func (hkAudit) Indexes() []Index  { return []Index{IndexOf("note")} }

type hkStamped struct {
	hkStamps
	ID int64
}

type hkBeside struct {
	Model[hkBeside]
	hkStamps
	ID int64
}

type hkBelow struct {
	Model[hkBelow]
	hkWrapped
	ID int64
}

type hkTwo struct {
	Model[hkTwo]
	hkStamps
	hkAudit
	ID int64
}

type hkOwn struct {
	Model[hkOwn]
	hkStamps
	ID int64
}

func (hkOwn) Indexes() []Index { return nil }

func hkModel[T any](t *testing.T) *model {
	t.Helper()
	m, err := buildModel(reflect.TypeFor[T](), "", "")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestDeclarationHooks: a model without the methods declares nothing, with
// Model's defaults or without Model; one with them declares theirs, with
// either receiver; @orm.Meta's indexes win over Indexes.
func TestDeclarationHooks(t *testing.T) {
	for _, m := range []*model{hkModel[hkPlain](t), hkModel[hkBase](t)} {
		if m.indexes != nil || m.checks != nil {
			t.Errorf("%s declares %v and %v, want nothing", m.Name, m.indexes, m.checks)
		}
	}
	for _, m := range []*model{hkModel[hkValue](t), hkModel[hkPointer](t)} {
		if len(m.indexes) != 1 || len(m.checks) != 1 {
			t.Errorf("%s declares %d indexes and %d checks, want 1 and 1", m.Name, len(m.indexes), len(m.checks))
		}
		if f, _ := m.field("Low"); f == nil || f.Gen == nil {
			t.Errorf("%s.Low has no generated expression", m.Name)
		}
	}
	if m := hkModel[hkMeta](t); len(m.indexes) != 1 || len(m.indexes[0].keys) != 2 {
		t.Errorf("hkMeta's indexes are %v, want Meta's", m.indexes)
	}
}

// TestDeclarationHooksEmbedded: an embedded struct's Indexes is the
// model's as Go promoted it before the bases had one — beside Model or
// deeper — unless two at one depth have it; the model's own wins.
func TestDeclarationHooksEmbedded(t *testing.T) {
	for _, m := range []*model{hkModel[hkStamped](t), hkModel[hkBeside](t), hkModel[hkBelow](t)} {
		if len(m.indexes) != 1 {
			t.Errorf("%s declares %v, want hkStamps's index", m.Name, m.indexes)
		}
	}
	for _, m := range []*model{hkModel[hkTwo](t), hkModel[hkOwn](t)} {
		if len(m.indexes) != 0 {
			t.Errorf("%s declares %v, want none", m.Name, m.indexes)
		}
	}
}
