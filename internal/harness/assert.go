package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"unicode/utf8"
)

func AssertOutput(output []byte, expectation Expectation) error {
	match := expectation.Output
	if match.SHA256 != "" {
		digest := sha256.Sum256(output)
		actual := hex.EncodeToString(digest[:])
		if actual != match.SHA256 {
			return fmt.Errorf("output SHA-256 %s, want %s", actual, match.SHA256)
		}
	}
	if match.HasEqualsBase64 || match.EqualsBase64 != "" {
		want, err := base64.StdEncoding.DecodeString(match.EqualsBase64)
		if err != nil {
			return fmt.Errorf("decode expected base64 output: %w", err)
		}
		if !bytes.Equal(output, want) {
			return fmt.Errorf("binary output mismatch: got %d bytes, want %d", len(output), len(want))
		}
	}
	needsText := match.Equals != nil || len(match.Contains) != 0 || len(match.NotContains) != 0 || len(match.Matches) != 0
	if !needsText {
		return nil
	}
	if !utf8.Valid(output) {
		return fmt.Errorf("output is not valid UTF-8; use sha256 or equals_base64")
	}
	text := string(output)
	if match.Equals != nil && text != *match.Equals {
		return fmt.Errorf("output did not equal expected text (got %d bytes, want %d)", len(output), len(*match.Equals))
	}
	for _, value := range match.Contains {
		if !bytes.Contains(output, []byte(value)) {
			return fmt.Errorf("output does not contain %q", value)
		}
	}
	for _, value := range match.NotContains {
		if bytes.Contains(output, []byte(value)) {
			return fmt.Errorf("output unexpectedly contains %q", value)
		}
	}
	for _, expression := range match.Matches {
		compiled, err := regexp.Compile(expression)
		if err != nil {
			return fmt.Errorf("compile output regexp %q: %w", expression, err)
		}
		if !compiled.MatchString(text) {
			return fmt.Errorf("output does not match %q", expression)
		}
	}
	return nil
}
