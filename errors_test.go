package orm

import (
	"errors"
	"fmt"
	"testing"
)

func TestKindOfBareKinds(t *testing.T) {
	for _, k := range []Kind{NotFound, MultipleFound, Conflict, Invalid, Unavailable} {
		if got := KindOf(k); got != k {
			t.Errorf("KindOf(%v) = %v", k, got)
		}
		if got := KindOf(fmt.Errorf("loading: %w", error(k))); got != k {
			t.Errorf("KindOf(wrapped %v) = %v", k, got)
		}
	}
	if got := KindOf(errors.New("driver")); got != 0 {
		t.Errorf("KindOf(driver error) = %v", got)
	}
}
