package yqlib

import (
	"bufio"
	"bytes"
	"container/list"
	"strings"
	"testing"

	"github.com/mikefarah/yq/v4/test"
)

func encodeYaml(t *testing.T, sampleYaml string) string {
	t.Helper()
	var output bytes.Buffer
	writer := bufio.NewWriter(&output)
	encoder := NewYamlEncoder(ConfiguredYamlPreferences)
	inputs, err := readDocuments(strings.NewReader(sampleYaml), "sample.yml", 0, NewYamlDecoder(ConfiguredYamlPreferences))
	if err != nil {
		t.Fatal(err)
	}
	node := inputs.Front().Value.(*CandidateNode)
	if err := encoder.Encode(writer, node); err != nil {
		t.Fatal(err)
	}
	writer.Flush()
	return output.String()
}

func encodeYamlAfterExpression(t *testing.T, sampleYaml string, expression string) string {
	t.Helper()
	var output bytes.Buffer
	writer := bufio.NewWriter(&output)
	encoder := NewYamlEncoder(ConfiguredYamlPreferences)
	inputs, err := readDocuments(strings.NewReader(sampleYaml), "sample.yml", 0, NewYamlDecoder(ConfiguredYamlPreferences))
	if err != nil {
		t.Fatal(err)
	}
	node := inputs.Front().Value.(*CandidateNode)
	evaluator := NewAllAtOnceEvaluator()
	inputList := list.New()
	inputList.PushBack(node)
	results, err := evaluator.EvaluateCandidateNodes(expression, inputList)
	if err != nil {
		t.Fatal(err)
	}
	resultNode := results.Front().Value.(*CandidateNode)
	if err := encoder.Encode(writer, resultNode); err != nil {
		t.Fatal(err)
	}
	writer.Flush()
	return output.String()
}

// Issue #1468: in-place updates must not rewrite unrelated multi-line strings.
func TestFoldedBlockPreservedOnUnrelatedUpdate(t *testing.T) {
	input := `value: one
description: >-
  This is a multi-line
  folded string that spans
  several lines of text.
`
	expected := `value: two
description: >-
  This is a multi-line
  folded string that spans
  several lines of text.
`
	test.AssertResult(t, expected, encodeYamlAfterExpression(t, input, `.value = "two"`))
}

func TestFoldedBlockPreservedOnRoundTrip(t *testing.T) {
	input := `description: >-
  hello
  world
`
	test.AssertResult(t, input, encodeYaml(t, input))
}

func TestFoldedKeepChompingPreserved(t *testing.T) {
	input := `keep: >+
  keep me
  please

value: one
`
	expected := `keep: >+
  keep me
  please

value: two
`
	actual := encodeYamlAfterExpression(t, input, `.value = "two"`)
	test.AssertResult(t, expected, actual)
	// value must be intact (two trailing newlines)
	if !strings.Contains(actual, "keep me\n  please\n\n") {
		t.Fatalf("expected original line breaks preserved, got:\n%s", actual)
	}
}

func TestFoldedWithTagAnchorPreserved(t *testing.T) {
	input := `tagged: !foo >-
  hello
  world
anchored: &a >-
  anchored
  text
value: one
`
	expected := `tagged: !foo >-
  hello
  world
anchored: &a >-
  anchored
  text
value: two
`
	test.AssertResult(t, expected, encodeYamlAfterExpression(t, input, `.value = "two"`))
}

func TestFoldedModifiedScalarNotPreserved(t *testing.T) {
	// Updating the folded scalar itself must use the new value.
	input := `description: >-
  original multi
  line text
value: one
`
	expected := `description: >-
  new value
value: one
`
	test.AssertResult(t, expected, encodeYamlAfterExpression(t, input, `.description = "new value"`))
}

func TestFoldedStyleChangeNotPreserved(t *testing.T) {
	// Changing the style must drop the original layout.
	input := `description: >-
  original multi
  line text
`
	expected := `description: "original multi line text"
`
	test.AssertResult(t, expected, encodeYamlAfterExpression(t, input, `.description style="double"`))
}

func TestFoldedBlockValueIntegrity(t *testing.T) {
	// The re-encoded document must parse back to identical values.
	input := `value: one
description: >-
  This is a multi-line
  folded string.
keep: >+
  keep me

`
	actual := encodeYamlAfterExpression(t, input, `.value = "two"`)
	decoded, err := readDocuments(strings.NewReader(actual), "out.yml", 0, NewYamlDecoder(ConfiguredYamlPreferences))
	if err != nil {
		t.Fatal(err)
	}
	node := decoded.Front().Value.(*CandidateNode)
	values := map[string]string{}
	for i := 0; i < len(node.Content); i += 2 {
		values[node.Content[i].Value] = node.Content[i+1].Value
	}
	if values["description"] != "This is a multi-line folded string." {
		t.Fatalf("description value corrupted: %q", values["description"])
	}
	if values["keep"] != "keep me\n\n" {
		t.Fatalf("keep value corrupted: %q", values["keep"])
	}
	if values["value"] != "two" {
		t.Fatalf("value not updated: %q", values["value"])
	}
}

// decodedMappingValue re-parses doc and returns the value of a top-level key,
// failing the test if the document does not parse.
func decodedMappingValue(t *testing.T, doc string, key string) string {
	t.Helper()
	decoded, err := readDocuments(strings.NewReader(doc), "out.yml", 0, NewYamlDecoder(ConfiguredYamlPreferences))
	if err != nil {
		t.Fatalf("output does not re-parse: %v\n%s", err, doc)
	}
	node := decoded.Front().Value.(*CandidateNode)
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1].Value
		}
	}
	t.Fatalf("key %q not found in re-parsed output:\n%s", key, doc)
	return ""
}

// encodeAllYamlAfterExpression applies expression to the first document and
// encodes every document, separating them with `---` like the CLI does.
func encodeAllYamlAfterExpression(t *testing.T, sampleYaml string, expression string) string {
	t.Helper()
	inputs, err := readDocuments(strings.NewReader(sampleYaml), "sample.yml", 0, NewYamlDecoder(ConfiguredYamlPreferences))
	if err != nil {
		t.Fatal(err)
	}
	first := inputs.Front().Value.(*CandidateNode)
	evaluator := NewAllAtOnceEvaluator()
	inputList := list.New()
	inputList.PushBack(first)
	results, err := evaluator.EvaluateCandidateNodes(expression, inputList)
	if err != nil {
		t.Fatal(err)
	}
	inputs.Front().Value = results.Front().Value.(*CandidateNode)

	var output bytes.Buffer
	writer := bufio.NewWriter(&output)
	encoder := NewYamlEncoder(ConfiguredYamlPreferences)
	for e, i := inputs.Front(), 0; e != nil; e, i = e.Next(), i+1 {
		if i > 0 {
			if err := encoder.PrintDocumentSeparator(writer); err != nil {
				t.Fatal(err)
			}
		}
		if err := encoder.Encode(writer, e.Value.(*CandidateNode)); err != nil {
			t.Fatal(err)
		}
	}
	writer.Flush()
	return output.String()
}

func encodeYamlWithPrefs(t *testing.T, sampleYaml string, prefs YamlPreferences) string {
	t.Helper()
	var output bytes.Buffer
	writer := bufio.NewWriter(&output)
	encoder := NewYamlEncoder(prefs)
	inputs, err := readDocuments(strings.NewReader(sampleYaml), "sample.yml", 0, NewYamlDecoder(prefs))
	if err != nil {
		t.Fatal(err)
	}
	node := inputs.Front().Value.(*CandidateNode)
	if err := encoder.Encode(writer, node); err != nil {
		t.Fatal(err)
	}
	writer.Flush()
	return output.String()
}

// A `...` document-end marker after a folded block must terminate the captured
// span: the marker is not scalar content. Capturing it corrupted the file
// (unparseable output). The marker itself is dropped, matching yq's
// behaviour without the fix.
func TestFoldedBlockWithDocumentEndMarker(t *testing.T) {
	input := `value: one
description: >-
  hello
  world
...
`
	expected := `value: two
description: >-
  hello
  world
`
	actual := encodeYamlAfterExpression(t, input, `.value = "two"`)
	test.AssertResult(t, expected, actual)
	if got := decodedMappingValue(t, actual, "description"); got != "hello world" {
		t.Fatalf("description value corrupted: %q", got)
	}
}

// Same as above, but the marker sits mid-stream between two documents.
func TestFoldedBlockWithDocumentEndMarkerMidStream(t *testing.T) {
	input := `value: one
description: >-
  hello
  world
...
---
other: doc
`
	expected := `value: two
description: >-
  hello
  world
---
other: doc
`
	actual := encodeAllYamlAfterExpression(t, input, `.value = "two"`)
	test.AssertResult(t, expected, actual)
	if got := decodedMappingValue(t, actual, "description"); got != "hello world" {
		t.Fatalf("description value corrupted: %q", got)
	}
}

// An indented `...` line is scalar content, not a document-end marker, and
// must be preserved as content.
func TestFoldedIndentedDocEndMarkerIsContent(t *testing.T) {
	input := `value: one
desc: >-
  hello
  ...
  world
`
	expected := `value: two
desc: >-
  hello
  ...
  world
`
	actual := encodeYamlAfterExpression(t, input, `.value = "two"`)
	test.AssertResult(t, expected, actual)
	if got := decodedMappingValue(t, actual, "desc"); got != "hello ... world" {
		t.Fatalf("desc value corrupted: %q", got)
	}
}

// Explicit indent indicators are preserved byte-for-byte when the node stays
// in place (nested block at the emitter's natural indent).
func TestFoldedExplicitIndentIndicatorPreserved(t *testing.T) {
	input := `value: one
nested:
  desc: >2-
    over
    indented
`
	expected := `value: two
nested:
  desc: >2-
    over
    indented
`
	test.AssertResult(t, expected, encodeYamlAfterExpression(t, input, `.value = "two"`))
}

// `>2-` with content indented past the indicator: the extra indent is
// significant (more-indented lines keep breaks and spaces), so the block must
// be spliced back verbatim, not normalised to `>-`.
func TestFoldedExplicitIndentOverIndentedPreserved(t *testing.T) {
	input := `value: one
desc: >2-
    over
    indented
`
	expected := `value: two
desc: >2-
    over
    indented
`
	actual := encodeYamlAfterExpression(t, input, `.value = "two"`)
	test.AssertResult(t, expected, actual)
	if got := decodedMappingValue(t, actual, "desc"); got != "  over\n  indented" {
		t.Fatalf("desc value corrupted: %q", got)
	}
}

// A trailing comment on the header disables preservation (the comment is
// stored separately); the scalar collapses as before, with the comment kept.
func TestFoldedHeaderCommentFallsBackToCollapse(t *testing.T) {
	input := `value: one
desc: >- # a comment
  hello
  world
`
	expected := `value: two
desc: >- # a comment
  hello world
`
	actual := encodeYamlAfterExpression(t, input, `.value = "two"`)
	test.AssertResult(t, expected, actual)
	if got := decodedMappingValue(t, actual, "desc"); got != "hello world" {
		t.Fatalf("desc value corrupted: %q", got)
	}
}

// When the indent preference moves the block, it is re-indented to fit and
// must still parse back to the same value.
func TestFoldedBlockReindentedWhenIndentPreferenceChanges(t *testing.T) {
	input := `a:
  desc: >-
    hello
    world
`
	prefs := ConfiguredYamlPreferences
	prefs.Indent = 4
	expected := `a:
    desc: >-
        hello
        world
`
	actual := encodeYamlWithPrefs(t, input, prefs)
	test.AssertResult(t, expected, actual)
	decoded, err := readDocuments(strings.NewReader(actual), "out.yml", 0, NewYamlDecoder(ConfiguredYamlPreferences))
	if err != nil {
		t.Fatalf("output does not re-parse: %v\n%s", err, actual)
	}
	aNode := decoded.Front().Value.(*CandidateNode)
	var descValue string
	for i := 0; i+1 < len(aNode.Content); i += 2 {
		if aNode.Content[i].Value == "a" {
			inner := aNode.Content[i+1]
			for j := 0; j+1 < len(inner.Content); j += 2 {
				if inner.Content[j].Value == "desc" {
					descValue = inner.Content[j+1].Value
				}
			}
		}
	}
	if descValue != "hello world" {
		t.Fatalf("desc value corrupted: %q", descValue)
	}
}
