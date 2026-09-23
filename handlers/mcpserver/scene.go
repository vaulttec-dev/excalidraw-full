package mcpserver

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"time"
)

// Element is an Excalidraw element as the editor stores it.
type Element = map[string]any

var defaults = Element{
	"strokeColor":     "#1e1e1e",
	"backgroundColor": "transparent",
	"fillStyle":       "solid",
	"strokeWidth":     2,
	"strokeStyle":     "solid",
	"roughness":       1,
	"opacity":         100,
	"angle":           0,
	"groupIds":        []any{},
	"frameId":         nil,
	"roundness":       nil,
	"boundElements":   nil,
	"link":            nil,
	"locked":          false,
	"isDeleted":       false,
}

const (
	fontFamilyHandDrawn = 1
	lineHeight          = 1.25
	// Rough advance width per character of the hand-drawn font. The editor
	// remeasures text when it is edited, but a stored element must already
	// have a real size: anything under half a pixel is dropped as invisible
	// when the scene is restored.
	characterWidthRatio = 0.55
)

func number(v any, fallback float64) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return fallback
}

func text(v any) string {
	s, _ := v.(string)
	return s
}

func randomID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 12)
	for i := range b {
		b[i] = alphabet[rand.Intn(len(alphabet))]
	}
	return string(b)
}

func randomInt() int { return rand.Intn(math.MaxInt32) }

// indexAfter returns the i-th fractional index placed after `after`, the
// greatest index already on the board. The editor orders elements by these
// keys; appending a fraction that ends in a letter keeps each key valid and
// greater than everything before it.
func indexAfter(after string, i int) string {
	if after == "" {
		after = "a0"
	}
	const letters = "abcdefghijklmnopqrstuvwxyz"
	suffix := string([]byte{letters[(i/26)%26], letters[i%26]})
	if i >= 26*26 {
		suffix = string(letters[(i/(26*26))%26]) + suffix
	}
	return after + "V" + suffix
}

func maxIndex(elements []Element) string {
	max := ""
	for _, el := range elements {
		if idx := text(el["index"]); idx > max {
			max = idx
		}
	}
	return max
}

func measure(s string, fontSize float64) (float64, float64) {
	lines := strings.Split(s, "\n")
	longest := 0
	for _, line := range lines {
		if n := len([]rune(line)); n > longest {
			longest = n
		}
	}
	width := math.Max(fontSize, math.Round(float64(longest)*fontSize*characterWidthRatio))
	height := math.Round(float64(len(lines)) * fontSize * lineHeight)
	return width, height
}

func base(input Element, index string) Element {
	el := Element{}
	for k, v := range defaults {
		el[k] = v
	}
	for k, v := range input {
		el[k] = v
	}
	if text(el["id"]) == "" {
		el["id"] = randomID()
	}
	el["x"] = number(input["x"], 0)
	el["y"] = number(input["y"], 0)
	el["width"] = number(input["width"], 0)
	el["height"] = number(input["height"], 0)
	el["seed"] = randomInt()
	el["version"] = 1
	el["versionNonce"] = randomInt()
	el["index"] = index
	el["updated"] = time.Now().UnixMilli()
	return el
}

func textElement(input Element, index string) Element {
	fontSize := number(input["fontSize"], 20)
	content := text(input["text"])
	width, height := measure(content, fontSize)

	el := base(input, index)
	el["type"] = "text"
	el["text"] = content
	if text(el["originalText"]) == "" {
		el["originalText"] = content
	}
	el["width"] = number(input["width"], width)
	el["height"] = number(input["height"], height)
	el["fontSize"] = fontSize
	if _, ok := input["fontFamily"]; !ok {
		el["fontFamily"] = fontFamilyHandDrawn
	}
	if _, ok := input["textAlign"]; !ok {
		el["textAlign"] = "left"
	}
	if _, ok := input["verticalAlign"]; !ok {
		el["verticalAlign"] = "top"
	}
	if _, ok := input["containerId"]; !ok {
		el["containerId"] = nil
	}
	el["lineHeight"] = lineHeight
	if _, ok := input["autoResize"]; !ok {
		el["autoResize"] = true
	}
	return el
}

// boundLabel turns a shape's label into a separate text element bound to it,
// which is how the editor stores container labels.
func boundLabel(container Element, label Element, index string) Element {
	fontSize := number(label["fontSize"], 20)
	content := text(label["text"])
	width, height := measure(content, fontSize)

	// Shapes clamp their label to the container, minus padding. Arrows and
	// lines carry it alongside the stroke, and a vertical arrow has no width
	// to clamp to.
	containerType := text(container["type"])
	if containerType != "arrow" && containerType != "line" {
		width = math.Min(math.Max(number(container["width"], 0)-16, fontSize), width)
	}

	strokeColor := text(label["strokeColor"])
	if strokeColor == "" {
		strokeColor = "#1e1e1e"
	}
	return textElement(Element{
		"id":            text(container["id"]) + "-label",
		"x":             number(container["x"], 0) + (number(container["width"], 0)-width)/2,
		"y":             number(container["y"], 0) + (number(container["height"], 0)-height)/2,
		"width":         width,
		"height":        height,
		"text":          content,
		"originalText":  content,
		"fontSize":      fontSize,
		"textAlign":     "center",
		"verticalAlign": "middle",
		"containerId":   container["id"],
		"strokeColor":   strokeColor,
		"autoResize":    false,
	}, index)
}

func linearElement(input Element, index string) Element {
	el := base(input, index)
	if _, ok := input["points"]; !ok {
		el["points"] = []any{[]any{0, 0}, []any{number(input["width"], 0), number(input["height"], 0)}}
	}
	el["lastCommittedPoint"] = nil
	for _, k := range []string{"startBinding", "endBinding", "startArrowhead"} {
		if _, ok := input[k]; !ok {
			el[k] = nil
		}
	}
	if _, ok := input["endArrowhead"]; !ok {
		if text(input["type"]) == "arrow" {
			el["endArrowhead"] = "arrow"
		} else {
			el["endArrowhead"] = nil
		}
	}
	if _, ok := input["elbowed"]; !ok {
		el["elbowed"] = false
	}
	return el
}

// buildScene expands the compact notation into full elements, placed after
// the index `after`.
func buildScene(inputs []Element, after string) ([]Element, error) {
	var out []Element
	next := func() string { return indexAfter(after, len(out)) }

	for i, input := range inputs {
		if input == nil {
			return nil, fmt.Errorf("element %d is empty", i)
		}
		kind := text(input["type"])
		element := Element{}
		var label Element
		for k, v := range input {
			if k == "label" {
				label, _ = v.(map[string]any)
				continue
			}
			element[k] = v
		}

		var built Element
		switch kind {
		case "text":
			built = textElement(element, next())
		case "arrow", "line":
			built = linearElement(element, next())
		case "rectangle", "ellipse", "diamond":
			built = base(element, next())
		case "":
			return nil, fmt.Errorf("element %d needs a type", i)
		default:
			return nil, fmt.Errorf("element %d: unsupported type %q (use rectangle, ellipse, diamond, text, arrow or line)", i, kind)
		}
		out = append(out, built)

		if label != nil && text(label["text"]) != "" {
			labelEl := boundLabel(built, label, next())
			bound, _ := built["boundElements"].([]any)
			built["boundElements"] = append(bound, map[string]any{"type": "text", "id": labelEl["id"]})
			out = append(out, labelEl)
		}
	}
	return out, nil
}

func sceneVersion(elements []Element) int {
	total := 0
	for _, el := range elements {
		total += int(number(el["version"], 1))
	}
	return total
}
