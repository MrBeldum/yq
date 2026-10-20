package yqlib

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v4"
)

// blockScalarOriginal records the source text of a folded block scalar at
// decode time, so the yaml encoder can reproduce the original line breaks
// when re-encoding an unmodified node (issue #1468). The yaml parser folds
// block scalars, destroying the original break positions, and the emitter
// then collapses the scalar to a single line. Splicing the captured text
// back avoids rewriting multi-line strings that an expression never touched.
type blockScalarOriginal struct {
	rawBlock string // exact source text from the block header to end of content
	value    string // parsed scalar value at decode time
	style    Style
	tag      string
	anchor   string
	// placeholder is assigned at encode time; the encoder swaps it back for
	// rawBlock after dumping.
	placeholder string
}

func (b *blockScalarOriginal) matches(node *CandidateNode) bool {
	return b != nil &&
		node.Value == b.value &&
		node.Style == b.style &&
		node.Tag == b.tag &&
		node.Anchor == b.anchor
}

// captureFoldedScalarOriginals walks a decoded candidate tree and records the
// original text of every folded block scalar. sourceLines must be the decoded
// stream split into lines, so 1-based node line numbers index into it.
func captureFoldedScalarOriginals(root *CandidateNode, sourceLines []string) {
	if len(sourceLines) == 0 {
		return
	}
	var lines []int
	collectNodeLines(root, &lines)
	sort.Ints(lines)
	captureFoldedScalarWalk(root, sourceLines, lines)
}

func collectNodeLines(node *CandidateNode, lines *[]int) {
	if node.Line > 0 {
		*lines = append(*lines, node.Line)
	}
	for _, child := range node.Content {
		collectNodeLines(child, lines)
	}
}

func captureFoldedScalarWalk(node *CandidateNode, sourceLines []string, nodeLines []int) {
	if node.Kind == ScalarNode && node.Style&FoldedStyle != 0 && node.Line > 0 {
		if original := captureFoldedScalar(node, sourceLines, nodeLines); original != nil {
			node.blockScalarOriginal = original
		}
	}
	for _, child := range node.Content {
		captureFoldedScalarWalk(child, sourceLines, nodeLines)
	}
}

func captureFoldedScalar(node *CandidateNode, sourceLines []string, nodeLines []int) *blockScalarOriginal {
	headerIdx := node.Line - 1
	if headerIdx < 0 || headerIdx >= len(sourceLines) {
		return nil
	}
	headerLine := sourceLines[headerIdx]
	// The node's column may point at a tag or anchor preceding the block
	// header (e.g. `!foo >-`); find the actual `>` token.
	searchFrom := node.Column - 1
	if searchFrom < 0 {
		searchFrom = 0
	}
	if searchFrom >= len(headerLine) {
		return nil
	}
	relIdx := strings.Index(headerLine[searchFrom:], ">")
	if relIdx == -1 {
		return nil
	}
	blockStart := searchFrom + relIdx

	// The scalar's extent runs to the line before the next token in the
	// stream. Block scalar content lines hold no tokens themselves.
	endIdx := len(sourceLines)
	for _, line := range nodeLines {
		if line > node.Line {
			endIdx = line - 1
			break
		}
	}
	if endIdx < headerIdx+1 {
		endIdx = headerIdx + 1
	}
	contentLines := sourceLines[headerIdx+1 : endIdx]
	// Don't let the span cross a document separator.
	for i, line := range contentLines {
		trimmed := strings.TrimSpace(line)
		if (trimmed == "---" || strings.HasPrefix(trimmed, "--- ")) && leadingSpaceCount(line) == 0 {
			contentLines = contentLines[:i]
			break
		}
	}

	// The block indent is set by the first non-blank content line.
	baseIndent := -1
	for _, line := range contentLines {
		if strings.TrimSpace(line) != "" {
			baseIndent = leadingSpaceCount(line)
			break
		}
	}
	trailing := trailingNewlineCount(node.Value)
	if baseIndent == -1 {
		// Empty (or all-blank) block scalar.
		return validateAndBuildRaw(node, headerLine[blockStart:], nil, trailing)
	}
	// Trim trailing blank lines and trailing comment lines that sit outside
	// the scalar (indented less than the block indent). An indented `#` line
	// is scalar content, not a comment. Trailing newlines are re-derived from
	// the parsed value below, so dropping the raw trailing blanks is safe.
	trimmed := contentLines
	for len(trimmed) > 0 {
		last := trimmed[len(trimmed)-1]
		if strings.TrimSpace(last) == "" {
			trimmed = trimmed[:len(trimmed)-1]
			continue
		}
		if isCommentLine(last) && leadingSpaceCount(last) < baseIndent {
			trimmed = trimmed[:len(trimmed)-1]
			continue
		}
		break
	}
	return validateAndBuildRaw(node, headerLine[blockStart:], trimmed, trailing)
}

// validateAndBuildRaw constructs the raw block text from the header token and
// content lines (kept verbatim, original indent), appending trailing newlines
// per the parsed value, then validates it re-parses to the same value.
// The spliced output adds a newline after the block, so we include one fewer
// trailing newline here than the value holds.
func validateAndBuildRaw(node *CandidateNode, headerToken string, contentLines []string, trailing int) *blockScalarOriginal {
	var sb strings.Builder
	sb.WriteString(headerToken)
	for _, line := range contentLines {
		sb.WriteString("\n")
		sb.WriteString(line)
	}
	if trailing > 0 {
		sb.WriteString(strings.Repeat("\n", trailing-1))
	}
	rawBlock := sb.String()

	if !rawBlockRoundTrips(rawBlock, node.Value) {
		return nil
	}
	return &blockScalarOriginal{
		rawBlock: rawBlock,
		value:    node.Value,
		style:    node.Style,
		tag:      node.Tag,
		anchor:   node.Anchor,
	}
}

// rawBlockRoundTrips checks that rawBlock (a block header plus content lines)
// parses as a block scalar with the expected value.
func rawBlockRoundTrips(rawBlock, expectedValue string) bool {
	doc := "key: " + rawBlock + "\n"
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
		return false
	}
	if len(n.Content) == 0 || len(n.Content[0].Content) < 2 {
		return false
	}
	return n.Content[0].Content[1].Value == expectedValue
}

// collectFoldPreserves gathers the blockScalarOriginals of unmodified folded
// scalars in the tree and assigns each a unique placeholder. The placeholder
// includes randomness so it cannot collide with user data; the trailing
// underscore ensures one placeholder can never be a substring of another.
func collectFoldPreserves(node *CandidateNode, preserves *[]*blockScalarOriginal) {
	var randBytes [8]byte
	_, _ = rand.Read(randBytes[:])
	prefix := "YqFoldPreserve_" + hex.EncodeToString(randBytes[:])
	collectFoldPreservesWithPrefix(node, preserves, prefix)
}

func collectFoldPreservesWithPrefix(node *CandidateNode, preserves *[]*blockScalarOriginal, prefix string) {
	if node.blockScalarOriginal.matches(node) {
		b := node.blockScalarOriginal
		b.placeholder = fmt.Sprintf("%s_%d_", prefix, len(*preserves))
		*preserves = append(*preserves, b)
		return
	}
	for _, child := range node.Content {
		collectFoldPreservesWithPrefix(child, preserves, prefix)
	}
}

// spliceFoldPreserves replaces placeholders in dumped output with the original
// block text. A placeholder is only replaced when it appears exactly once
// (ruling out collisions with user data) and the captured block's content is
// more indented than the placeholder's line (keeping the splice valid YAML).
func spliceFoldPreserves(output []byte, preserves []*blockScalarOriginal) []byte {
	result := output
	for _, p := range preserves {
		if p.placeholder == "" {
			continue
		}
		ph := []byte(p.placeholder)
		if bytes.Count(result, ph) != 1 {
			continue
		}
		idx := bytes.Index(result, ph)
		// Indent of the line holding the placeholder.
		lineStart := bytes.LastIndex(result[:idx], []byte("\n")) + 1
		lineIndent := 0
		for lineStart+lineIndent < idx && result[lineStart+lineIndent] == ' ' {
			lineIndent++
		}
		if !blockFitsIndent(p.rawBlock, lineIndent) {
			continue
		}
		replacement := []byte(p.rawBlock)
		merged := make([]byte, 0, len(result)-len(ph)+len(replacement))
		merged = append(merged, result[:idx]...)
		merged = append(merged, replacement...)
		merged = append(merged, result[idx+len(ph):]...)
		result = merged
	}
	return result
}

// blockFitsIndent reports whether rawBlock's content lines are all more
// indented than indent, so splicing it at that indent stays valid YAML.
func blockFitsIndent(rawBlock string, indent int) bool {
	lines := strings.Split(rawBlock, "\n")
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if leadingSpaceCount(line) <= indent {
			return false
		}
	}
	return true
}

func leadingSpaceCount(line string) int {
	n := 0
	for n < len(line) && line[n] == ' ' {
		n++
	}
	return n
}

func isCommentLine(line string) bool {
	return strings.HasPrefix(strings.TrimLeft(line, " "), "#")
}

func trailingNewlineCount(value string) int {
	return len(value) - len(strings.TrimRight(value, "\n"))
}
