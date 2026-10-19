package orm

import (
	"os"
	"reflect"
	"testing"
)

// ORM_REFLECT=1 runs the suite with the generated scanners ignored, as a
// program without orm generate's files reads rows.
func TestMain(m *testing.M) {
	if os.Getenv("ORM_REFLECT") != "" {
		setReflectOnly(true)
	}
	os.Exit(m.Run())
}

func setReflectOnly(on bool) {
	reflectOnly.Store(on)
	scannerVer.Add(1)
}

// TestForRegisters: a model's For registers it, as orm generate's init
// would, so Check knows it without the generated files.
func TestForRegisters(t *testing.T) {
	type note struct {
		Model[note]
		ID   int64
		Text string
	}
	type plain struct{ ID int64 }
	For[note]()
	For[plain]()
	if _, ok := registry.done.Load(reflect.TypeFor[note]()); !ok {
		t.Fatal("the model isn't registered")
	}
	if _, ok := registry.done.Load(reflect.TypeFor[plain]()); ok {
		t.Fatal("a type not embedding orm.Model is registered")
	}
}
