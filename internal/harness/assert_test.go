package harness

import "testing"

func TestAssertOutputTextMatchers(t *testing.T) {
	if err := AssertOutput([]byte("hello\nworld\n"), Expectation{Output: OutputExpectation{
		Contains: []string{"hello"}, NotContains: []string{"error"}, Matches: []string{`(?m)^world$`},
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestAssertOutputContainsMatchesBytesInInvalidUTF8(t *testing.T) {
	output := append([]byte("Printing modules of process ID: 1234\nMicrosoft"), 0xae)
	output = append(output, []byte(" C Runtime Library\ncaf\u00e9")...)

	if err := AssertOutput(output, Expectation{Output: OutputExpectation{
		Contains: []string{"Printing modules of process ID:", "C Runtime Library", "caf\u00e9"},
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestAssertOutputContainsReportsAbsentNeedleInInvalidUTF8(t *testing.T) {
	output := []byte{'p', 'r', 'e', 's', 'e', 'n', 't', 0xff}
	err := AssertOutput(output, Expectation{Output: OutputExpectation{Contains: []string{"missing"}}})
	if err == nil || err.Error() != `output does not contain "missing"` {
		t.Fatalf("AssertOutput() error = %v, want absent-needle failure", err)
	}
}

func TestAssertOutputContainsRequiresEveryNeedle(t *testing.T) {
	output := []byte{'a', 'l', 'p', 'h', 'a', 0xff, 'o', 'm', 'e', 'g', 'a'}
	err := AssertOutput(output, Expectation{Output: OutputExpectation{
		Contains: []string{"alpha", "missing", "omega"},
	}})
	if err == nil || err.Error() != `output does not contain "missing"` {
		t.Fatalf("AssertOutput() error = %v, want failure for the first absent needle", err)
	}
}

func TestAssertOutputNotContainsMatchesBytesInInvalidUTF8(t *testing.T) {
	output := []byte{'a', 'l', 'p', 'h', 'a', 0xff, 'o', 'm', 'e', 'g', 'a'}
	if err := AssertOutput(output, Expectation{Output: OutputExpectation{
		NotContains: []string{"missing"},
	}}); err != nil {
		t.Fatal(err)
	}
	err := AssertOutput(output, Expectation{Output: OutputExpectation{
		NotContains: []string{"omega"},
	}})
	if err == nil || err.Error() != `output unexpectedly contains "omega"` {
		t.Fatalf("AssertOutput() error = %v, want present-needle failure", err)
	}
}

func TestAssertOutputRegexRejectsInvalidUTF8(t *testing.T) {
	err := AssertOutput([]byte{'a', 'l', 'p', 'h', 'a', 0xff}, Expectation{Output: OutputExpectation{
		Matches: []string{"alpha"},
	}})
	if err == nil || err.Error() != "output is not valid UTF-8; use sha256 or equals_base64" {
		t.Fatalf("AssertOutput() error = %v, want invalid UTF-8 failure", err)
	}
}

func TestAssertOutputSHA256(t *testing.T) {
	if err := AssertOutput([]byte("hello"), Expectation{Output: OutputExpectation{
		SHA256: "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestAssertOutputCanRequireEmptyBinaryOutput(t *testing.T) {
	if err := AssertOutput(nil, Expectation{Output: OutputExpectation{
		HasEqualsBase64: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := AssertOutput([]byte("not empty"), Expectation{Output: OutputExpectation{
		HasEqualsBase64: true,
	}}); err == nil {
		t.Fatal("expected an exact empty-output mismatch")
	}
}
