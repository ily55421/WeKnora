package types

import (
	"testing"
)

// SQLite (Lite mode) hands TEXT columns back as string while Postgres hands
// jsonb back as []byte. These arrays used to accept only []byte and silently
// return nil for anything else, so every JSON column read from SQLite came back
// empty with no error: the wiki cleanup path read source_refs, saw an empty
// list, concluded the page had no other sources and deleted it — deleting one
// knowledge file destroyed wiki pages other files still shared.
func TestStringArrayScanAcceptsStringAndBytes(t *testing.T) {
	const payload = `["doc-1|x","doc-2"]`

	for name, value := range map[string]interface{}{
		"[]byte (postgres)": []byte(payload),
		"string (sqlite)":   payload,
	} {
		t.Run(name, func(t *testing.T) {
			var got StringArray
			if err := got.Scan(value); err != nil {
				t.Fatalf("Scan(%T) error: %v", value, err)
			}
			if len(got) != 2 || got[0] != "doc-1|x" || got[1] != "doc-2" {
				t.Fatalf("Scan(%T) lost data: got %#v", value, got)
			}
		})
	}
}

// A value the driver cannot hand us as JSON text is a bug worth surfacing, not
// something to swallow — the silent version of this is what made the data loss
// invisible for so long.
func TestStringArrayScanRejectsUnexpectedType(t *testing.T) {
	var got StringArray
	if err := got.Scan(42); err == nil {
		t.Fatal("Scan(int) should report an error instead of silently returning nil")
	}
}

// nil and "" mean "no value stored"; they must stay a no-op rather than an
// unmarshal error, because pre-existing rows may hold either.
func TestStringArrayScanToleratesEmptyValues(t *testing.T) {
	for name, value := range map[string]interface{}{
		"nil":   nil,
		"empty": "",
	} {
		t.Run(name, func(t *testing.T) {
			got := StringArray{"stale"}
			if err := got.Scan(value); err != nil {
				t.Fatalf("Scan(%v) error: %v", value, err)
			}
		})
	}
}
