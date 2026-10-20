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
