package syncertest

import (
	"reflect"
	"testing"
)

// probeValue returns a distinctive non-zero value for a field the guard can
// drive, and reports whether the kind is one it knows how to drive at all.
//
// Bools and string slices are the two shapes every request-edge scope and
// policy field uses today. Kinds outside that set are skipped rather than
// guessed at: a false pass is worse than an uncovered field, and an unsupported
// kind shows up as a missing subtest rather than as a green assertion.
func probeValue(ft reflect.Type) (reflect.Value, bool) {
	if ft.Kind() == reflect.Bool {
		return reflect.ValueOf(true), true
	}
	if ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.String {
		probe := reflect.MakeSlice(ft, 1, 1)
		probe.Index(0).SetString("refs/heads/threading-probe")
		return probe, true
	}
	return reflect.Value{}, false
}

// AssertFieldsThreaded drives a reflection-based "no field silently dropped"
// check over a request-edge struct.
//
// An enumerated list of assertions only ever covers what someone remembered to
// add to it, so the failure mode is a newly declared scope or policy field that
// the API accepts and then ignores, with no test going red. Reflection inverts
// that: a new field is covered the moment it is declared, and this fails until
// it is threaded. Scope.ExcludeRefs was dropped by three of unstable's config
// builders for exactly as long as the guard only walked policy bools.
//
// T is the request-side struct (gitsync.SyncPolicy, gitsync.RefScope), inferred
// from build. For each exported field of a supported kind, build is called with
// that one field set and must return the syncer.Config the request produced;
// the config is then required to carry a same-named field holding the same
// value.
//
// Matching is by identical field name, which is the convention every scope and
// policy field follows today. A future field whose config counterpart is
// deliberately named differently — or deliberately absent — fails here and
// belongs in skip with a reason, rather than being renamed to satisfy a test.
func AssertFieldsThreaded[T any](t *testing.T, skip map[string]string, build func(*testing.T, T) any) {
	t.Helper()
	var zero T
	inType := reflect.TypeOf(zero)
	for i := range inType.NumField() {
		field := inType.Field(i)
		// Unexported fields cannot be set through reflection: enumerating one
		// panics inside Set rather than reporting anything useful, and no field
		// a caller can populate at the request edge is unexported anyway.
		if !field.IsExported() {
			continue
		}
		probe, drivable := probeValue(field.Type)
		if !drivable {
			continue
		}
		if reason, skipped := skip[field.Name]; skipped {
			t.Logf("skipping %s: %s", field.Name, reason)
			continue
		}
		t.Run(field.Name, func(t *testing.T) {
			// One field at a time, so a failure names the culprit and no
			// mutually-exclusive pair is ever set together.
			var populated T
			reflect.ValueOf(&populated).Elem().FieldByName(field.Name).Set(probe)

			cfg := reflect.ValueOf(build(t, populated))
			got := cfg.FieldByName(field.Name)
			if !got.IsValid() {
				t.Fatalf("syncer.Config has no %s field; thread it, or add it to skip with a reason", field.Name)
			}
			// A same-named field of a different kind is reported rather than
			// panicked through: that is itself the silent-drop case this guard
			// exists to name, and a reflect panic names nothing.
			if got.Kind() != probe.Kind() {
				t.Fatalf("%s.%s is %s but syncer.Config.%s is %s; thread it explicitly, or add it to skip with a reason",
					inType.Name(), field.Name, probe.Kind(), field.Name, got.Kind())
			}
			if !reflect.DeepEqual(got.Interface(), probe.Interface()) {
				t.Errorf("%s.%s = %v was dropped by the config builder (got %v)",
					inType.Name(), field.Name, probe.Interface(), got.Interface())
			}
		})
	}
}
