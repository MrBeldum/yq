package yqlib

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"go.yaml.in/yaml/v4"
)

type yamlEncoder struct {
	prefs YamlPreferences
}

func NewYamlEncoder(prefs YamlPreferences) Encoder {
	return &yamlEncoder{prefs}
}

func (ye *yamlEncoder) CanHandleAliases() bool {
	return true
}

func (ye *yamlEncoder) PrintDocumentSeparator(writer io.Writer) error {
	return PrintYAMLDocumentSeparator(writer, ye.prefs.PrintDocSeparators)
}

func (ye *yamlEncoder) PrintLeadingContent(writer io.Writer, content string) error {
	return PrintYAMLLeadingContent(writer, content, ye.prefs.PrintDocSeparators, ye.prefs.ColorsEnabled)
}

// canUnwrapScalar reports whether writing the scalar's raw value produces
// YAML that reads back as the same scalar. The unwrap fast path writes
// node.Value verbatim; for values that cannot be represented as a plain
// scalar (e.g. an empty quoted string, a lone quote, or a string like
// 'true' that would resolve to a non-string) that would corrupt the output,
// so the encoder falls back to normal styled output instead.
func canUnwrapScalar(node *CandidateNode) bool {
	// Only scalars that were explicitly quoted in the source document can lose
	// information when unwrapped; plain and generated scalars keep the
	// long-standing unwrap behaviour.
	if node.Style&(SingleQuotedStyle|DoubleQuotedStyle) == 0 {
		return true
	}
	value := node.Value
	if strings.Contains(value, "\n") {
		// multi-line values are written out raw, preserving long-standing behaviour
		return true
	}
	if value == "" {
		// an empty plain scalar reads back as null, not as an empty string
		return false
	}
	var decoded yaml.Node
	if err := yaml.Unmarshal([]byte(value), &decoded); err != nil {
		return false
	}
	if len(decoded.Content) != 1 {
		return false
	}
	content := decoded.Content[0]
	return content.Kind == yaml.ScalarNode && content.Tag == node.Tag && content.Value == value
}

func (ye *yamlEncoder) Encode(writer io.Writer, node *CandidateNode) error {
	log.Debugf("encoderYaml - going to print %v", NodeToString(node))
	// Detect line ending style from LeadingContent
	lineEnding := "\n"
	if strings.Contains(node.LeadingContent, "\r\n") {
		lineEnding = "\r\n"
	}
	if node.Kind == ScalarNode && ye.prefs.UnwrapScalar && canUnwrapScalar(node) {
		valueToPrint := node.Value
		if node.LeadingContent == "" || valueToPrint != "" {
			valueToPrint = valueToPrint + lineEnding
		}
		return writeString(writer, valueToPrint)
	}

	destination := writer
	tempBuffer := bytes.NewBuffer(nil)
	if ye.prefs.ColorsEnabled {
		destination = tempBuffer
	}

	indent := ye.prefs.Indent
	if indent < 2 {
		indent = 2
	} else if indent > 9 {
		indent = 9
	}

	dumper, err := yaml.NewDumper(destination,
		yaml.WithV3Defaults(),
		yaml.WithIndent(indent),
		yaml.WithCompactSeqIndent(ye.prefs.CompactSequenceIndent),
		yaml.WithLineWidth(-1),
	)
	if err != nil {
		return fmt.Errorf("configure YAML encoding: %w", err)
	}

	target, err := node.MarshalYAML()
	if err != nil {
		_ = dumper.Close()
		return err
	}

	trailingContent := target.FootComment
	target.FootComment = ""

	err = dumper.Dump(target)
	if closeErr := dumper.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}

	if err := ye.PrintLeadingContent(destination, trailingContent); err != nil {
		return err
	}

	if ye.prefs.ColorsEnabled {
		return colorizeAndPrint(tempBuffer.Bytes(), writer)
	}
	return nil
}
