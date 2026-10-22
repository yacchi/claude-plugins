package plan

import (
	"reflect"
	"testing"
)

// est_seconds counts when every package on the dependent paths has one; a
// standalone package without one (on no path) must not force counts.
func TestCriticalPathSumsWhenEveryPathIsTimed(t *testing.T) {
	ps := []Package{P("C", nil, nil, 0), P("D", nil, nil, 0), P("X", nil, nil, 0),
		P("E", []string{"D"}, nil, 0), P("H", []string{"C"}, nil, 0), P("I", []string{"H"}, nil, 0)}
	for i := range ps {
		ps[i].EstSeconds = 10
	}
	ps[3].EstSeconds = 100 // D's path: 100; C's path: 20
	ps[2].EstSeconds = 0   // X has no dependents: it is on no path
	if got := Select(mk(ps...), nil, 10); !reflect.DeepEqual(got, []string{"D", "C", "X"}) {
		t.Errorf("got %v want [D C X]", got)
	}
}
