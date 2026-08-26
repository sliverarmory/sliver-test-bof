package harness

import (
	"encoding/hex"
	"testing"
)

func TestPackArguments(t *testing.T) {
	got, err := PackArguments([]Argument{
		{Type: "short", Uint64: 0x1234},
		{Type: "int", Uint64: 0x12345678},
		{Type: "string", String: "hi"},
		{Type: "wstring", String: "A"},
		{Type: "file", Data: []byte{1, 2}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "1b000000341278563412030000006869000400000041000000020000000102"
	if actual := hex.EncodeToString(got); actual != want {
		t.Fatalf("packed arguments = %s, want %s", actual, want)
	}
}

func TestPackEmptyArguments(t *testing.T) {
	got, err := PackArguments(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if actual := hex.EncodeToString(got); actual != "00000000" {
		t.Fatalf("empty arguments = %s", actual)
	}
}

func TestRawArgumentsAreUnchanged(t *testing.T) {
	want := []byte{4, 0, 0, 0, 1, 2, 3, 4}
	got, err := PackArguments(nil, want)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Fatalf("raw arguments changed")
	}
}

func TestExplicitEmptyRawArgumentsAreUnchanged(t *testing.T) {
	got, err := PackArguments(nil, []byte{})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("explicit empty raw arguments = %#v, want non-nil empty buffer", got)
	}
}

func TestPackCOFFLoaderArguments(t *testing.T) {
	got, err := PackCOFFLoaderArguments("go", []byte{1, 2}, []byte{0, 0, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	want := "1500000003000000676f000200000001020400000000000000"
	if actual := hex.EncodeToString(got); actual != want {
		t.Fatalf("outer arguments = %s, want %s", actual, want)
	}
}
