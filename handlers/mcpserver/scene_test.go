package mcpserver

import (
	"sort"
	"testing"
)

func TestIndexAfterKeepsOrder(t *testing.T) {
	const after = "a1"
	var keys []string
	for i := 0; i < 2000; i++ {
		key := indexAfter(after, i)
		if key <= after {
			t.Fatalf("index %q is not after %q", key, after)
		}
		keys = append(keys, key)
	}
	if !sort.StringsAreSorted(keys) {
		t.Fatal("indexes do not sort in the order they were issued")
	}
}

func TestBuildSceneLabelsAndIndexes(t *testing.T) {
	out, err := buildScene([]Element{
		{"type": "rectangle", "id": "box", "x": 10.0, "y": 20.0, "width": 200.0, "height": 80.0,
			"label": map[string]any{"text": "Hello"}},
		{"type": "arrow", "x": 0.0, "y": 0.0, "width": 100.0, "height": 0.0},
	}, "a5")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d elements, want 3 (shape, label, arrow)", len(out))
	}

	label := out[1]
	if label["containerId"] != "box" || label["text"] != "Hello" {
		t.Fatalf("label not bound to its shape: %v", label)
	}
	bound, _ := out[0]["boundElements"].([]any)
	if len(bound) != 1 {
		t.Fatalf("shape does not list its label: %v", out[0]["boundElements"])
	}
	if out[2]["endArrowhead"] != "arrow" {
		t.Fatalf("arrow has no arrowhead: %v", out[2]["endArrowhead"])
	}

	prev := "a5"
	for _, el := range out {
		idx := el["index"].(string)
		if idx <= prev {
			t.Fatalf("index %q does not follow %q", idx, prev)
		}
		prev = idx
	}
}

func TestBuildSceneRejectsUnknownTypes(t *testing.T) {
	for _, input := range []Element{{"type": "star"}, {"x": 1.0}} {
		if _, err := buildScene([]Element{input}, ""); err == nil {
			t.Fatalf("accepted %v", input)
		}
	}
}
