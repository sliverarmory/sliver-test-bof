package harness

import "testing"

func TestAssertOutputTextMatchers(t *testing.T) {
	if err := AssertOutput([]byte("hello\nworld\n"), Expectation{Output: OutputExpectation{
		Contains: []string{"hello"}, NotContains: []string{"error"}, Matches: []string{`(?m)^world$`},
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestAssertOutputRejectsInvalidUTF8ForText(t *testing.T) {
	if err := AssertOutput([]byte{0xff}, Expectation{Output: OutputExpectation{Contains: []string{"x"}}}); err == nil {
		t.Fatal("expected invalid UTF-8 error")
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
