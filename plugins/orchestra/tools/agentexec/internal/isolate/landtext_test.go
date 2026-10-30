package isolate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLandResultCreatedJSON(t *testing.T) {
	landed := LandResult{Status: "landed", Task: "t1", Branch: "feature/x", Commit: "1234567890ab", Created: true}
	if got := marshalMap(t, landed)["created"]; got != true {
		t.Fatalf("created = %v, want true", got)
	}
	landed.Created = false
	if _, ok := marshalMap(t, landed)["created"]; ok {
		t.Fatal("created present when Created is false")
	}
}

func TestMergeResultLandJSON(t *testing.T) {
	if _, ok := marshalMap(t, MergeResult{Status: "merged", Task: "t1"})["land"]; ok {
		t.Fatal("land present when Land is nil")
	}
	m := marshalMap(t, MergeResult{Status: "merged", Task: "t1", Land: &LandResult{Status: "ask", Task: "t1"}})
	land, ok := m["land"].(map[string]any)
	if !ok || land["status"] != "ask" {
		t.Fatalf("land = %v, want object with status ask", m["land"])
	}
}

func TestFormatLandTextNewBranch(t *testing.T) {
	got := FormatLandText(LandResult{Status: "landed", Task: "t1", Branch: "feature/x", Commit: "1234567890ab", Created: true})
	if !strings.Contains(got, "on feature/x (new branch): ") {
		t.Fatalf("got %q", got)
	}
}

func TestFormatMergeTextLand(t *testing.T) {
	merge := MergeResult{Status: "merged", Task: "t1", Assembly: "a1", Commit: "1234567890ab"}
	if got := FormatMergeText(merge); strings.Contains(got, "; ") {
		t.Fatalf("no land: got %q", got)
	}
	land := LandResult{Status: "ask", Task: "t1", Reason: "isolation.land is ask"}
	merge.Land = &land
	if got, want := FormatMergeText(merge), FormatMergeText(MergeResult{Status: "merged", Task: "t1", Assembly: "a1", Commit: "1234567890ab"})+"; "+FormatLandText(land); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func marshalMap(t *testing.T, v json.Marshaler) map[string]any {
	t.Helper()
	b, err := v.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
