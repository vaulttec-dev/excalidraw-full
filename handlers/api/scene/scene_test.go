package scene

import "testing"

func TestReplaceWith(t *testing.T) {
	current := []Element{
		{"id": "kept", "version": 3.0},
		{"id": "dropped", "version": 5.0},
		{"id": "gone", "version": 2.0, "isDeleted": true},
	}
	target := []Element{
		{"id": "kept", "version": 1, "x": 42.0},
		{"id": "new", "version": 1},
	}

	next, changed := ReplaceWith(current, target)

	byID := map[string]Element{}
	for _, el := range next {
		byID[idOf(el)] = el
	}
	if len(next) != 4 {
		t.Fatalf("next has %d elements, want 4", len(next))
	}
	if v := intOf(byID["kept"]["version"], 0); v != 4 {
		t.Fatalf("kept element has version %d, want one above the stored 3", v)
	}
	if byID["kept"]["x"] != 42.0 {
		t.Fatal("kept element did not take the target's state")
	}
	if deleted, _ := byID["dropped"]["isDeleted"].(bool); !deleted {
		t.Fatal("element missing from target was not deleted")
	}
	if v := intOf(byID["dropped"]["version"], 0); v != 6 {
		t.Fatalf("deleted element has version %d, want 6", v)
	}

	// The already deleted element is carried along but not announced again.
	ids := map[string]bool{}
	for _, el := range changed {
		ids[idOf(el)] = true
	}
	if !ids["kept"] || !ids["new"] || !ids["dropped"] || ids["gone"] {
		t.Fatalf("changed = %v", ids)
	}
	if len(Visible(next)) != 2 {
		t.Fatalf("visible = %d, want 2", len(Visible(next)))
	}
}

func TestVersionSumsElements(t *testing.T) {
	if v := Version([]Element{{"version": 2.0}, {"version": 3}, {}}); v != 6 {
		t.Fatalf("Version = %d, want 6", v)
	}
}
