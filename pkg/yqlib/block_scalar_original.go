package yqlib

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
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
	// keyIndent is the indent of the header line at decode time; the block is
	// spliced back verbatim only when the placeholder sits at the same indent
	// (the node did not move), which guarantees the original bytes are still
	// valid in place.
	keyIndent int
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
	// Extract just the header token (e.g. ">-", ">+", ">2-"), not trailing comments.
	headerToken := extractHeaderToken(headerLine[blockStart:])
	// If the header line has a trailing comment, skip preservation: the
	// comment is stored separately as a line comment and would be emitted in
	// the wrong place after splicing.
	restOfHeader := headerLine[blockStart+len(headerToken):]
	if strings.Contains(restOfHeader, "#") {
		return nil
	}
	// The key indent anchors the block's position. An explicit indent
	// indicator (e.g. `>2-`) is relative to it, so validation must use the
	// same geometry or it falsely rejects blocks below the top level.
	keyIndent := leadingSpaceCount(headerLine)

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
	// Don't let the span cross a document separator or document-end marker.
	// A column-0 `...` ends the document; capturing it as scalar content
	// corrupts the block on re-indent (the marker is not content).
	for i, line := range contentLines {
		trimmed := strings.TrimSpace(line)
		if leadingSpaceCount(line) == 0 &&
			(trimmed == "---" || strings.HasPrefix(trimmed, "--- ") ||
				trimmed == "..." || strings.HasPrefix(trimmed, "... ")) {
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
		return validateAndBuildRaw(node, headerToken, nil, trailing, keyIndent)
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
	return validateAndBuildRaw(node, headerToken, trimmed, trailing, keyIndent)
}

// extractHeaderToken pulls the block scalar header (e.g. ">-", ">+", ">2-")
// from the start of s, stopping before whitespace or comments.
func extractHeaderToken(s string) string {
	i := 1 // skip '>' or '|'
	for i < len(s) {
		c := s[i]
		if c == '-' || c == '+' || (c >= '0' && c <= '9') {
			i++
		} else {
			break
		}
	}
	return s[:i]
}

// validateAndBuildRaw constructs the raw block text from the header token and
// content lines (kept verbatim, original indent), appending trailing newlines
// per the parsed value, then validates it re-parses to the same value.
// keyIndent is the header line's indent; the check mirrors it because an
// explicit indent indicator is relative to the key. The spliced output adds a
// newline after the block, so we include one fewer trailing newline here than
// the value holds.
func validateAndBuildRaw(node *CandidateNode, headerToken string, contentLines []string, trailing int, keyIndent int) *blockScalarOriginal {
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

	if !rawBlockRoundTripsAtIndent(rawBlock, node.Value, keyIndent) {
		return nil
	}
	return &blockScalarOriginal{
		rawBlock:  rawBlock,
		value:     node.Value,
		style:     node.Style,
		tag:       node.Tag,
		anchor:    node.Anchor,
		keyIndent: keyIndent,
	}
}

// rawBlockRoundTripsAtIndent checks that rawBlock (a block header plus
// content lines) parses as a block scalar with the expected value, with the
// key placed at keyIndent to mirror the block's actual position.
func rawBlockRoundTripsAtIndent(rawBlock, expectedValue string, keyIndent int) bool {
	doc := strings.Repeat(" ", keyIndent) + "key: " + rawBlock + "\n"
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
// (ruling out collisions with user data). The block is re-indented to fit the
// placeholder's position, preserving the original line breaks.
func spliceFoldPreserves(output []byte, preserves []*blockScalarOriginal, indentPref int) []byte {
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
		newIndent := lineIndent + indentPref
		var replacement string
		if lineIndent == p.keyIndent {
			// The node did not move: the original bytes parsed successfully at
			// this indent, so splicing them back verbatim is guaranteed valid
			// and value-identical. This preserves explicit indent indicators
			// (e.g. `>2-`) and the original content indent byte-for-byte.
			replacement = p.rawBlock
		} else {
			// The node moved (or the indent preference changed): re-indent the
			// block to fit its new position.
			replacement = reindentBlock(p.rawBlock, newIndent)
		}
		// Never trust the spliced text blindly: it must re-parse to the same
		// scalar value at its actual position. If it does not, fall back to a
		// safely-quoted rendering of the folded value rather than risk
		// emitting a corrupt document.
		if !rawBlockRoundTripsAtIndent(replacement, p.value, lineIndent) {
			replacement = safeScalarFallback(p.value)
		}
		rb := []byte(replacement)
		merged := make([]byte, 0, len(result)-len(ph)+len(rb))
		merged = append(merged, result[:idx]...)
		merged = append(merged, rb...)
		merged = append(merged, result[idx+len(ph):]...)
		result = merged
	}
	return result
}

// safeScalarFallback renders value as a YAML scalar guaranteed to parse back
// to the same value. It is only used when a spliced block fails re-validation
// (defence in depth): the folded value survives even though the original line
// breaks are lost.
func safeScalarFallback(value string) string {
	out, err := yaml.Marshal(value)
	if err != nil {
		return strconv.Quote(value)
	}
	return strings.TrimSuffix(string(out), "\n")
}

// baseIndentOf returns the minimum indent of the non-blank content lines of
// a raw block (its original base indent), or -1 when there are none.
func baseIndentOf(rawBlock string) int {
	lines := strings.Split(rawBlock, "\n")
	baseIndent := -1
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) != "" {
			if ind := leadingSpaceCount(line); baseIndent == -1 || ind < baseIndent {
				baseIndent = ind
			}
		}
	}
	return baseIndent
}

// reindentBlock shifts a raw block's content lines to newIndent, preserving
// relative indentation and line breaks. The header keeps its chomping
// indicator but drops any explicit indent indicator (which may not suit the
// new position).
func reindentBlock(rawBlock string, newIndent int) string {
	lines := strings.Split(rawBlock, "\n")
	if len(lines) == 0 {
		return rawBlock
	}
	header := lines[0]
	// Extract chomping indicator (-, +, or none); drop explicit indent digits.
	chomping := ""
	if strings.Contains(header, "-") {
		chomping = "-"
	} else if strings.Contains(header, "+") {
		chomping = "+"
	}
	header = ">" + chomping
	if len(lines) <= 1 {
		return header
	}
	// Find the original base indent (minimum indent of non-blank content lines).
	baseIndent := baseIndentOf(rawBlock)
	if baseIndent == -1 {
		return header
	}
	var sb strings.Builder
	sb.WriteString(header)
	for _, line := range lines[1:] {
		sb.WriteString("\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Shift: strip original base indent, apply new indent.
		content := line
		if len(line) >= baseIndent {
			content = line[baseIndent:]
		} else {
			content = strings.TrimLeft(line, " ")
		}
		sb.WriteString(strings.Repeat(" ", newIndent))
		sb.WriteString(content)
	}
	return sb.String()
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
