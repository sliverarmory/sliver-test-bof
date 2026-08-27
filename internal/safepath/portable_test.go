package safepath

import "testing"

func TestValidatePortableRelative(t *testing.T) {
	for _, value := range []string{"object.o", ".objects/linux/386/test.o", "nested/marker.txt", "unicode/café.txt"} {
		if err := ValidatePortableRelative(value); err != nil {
			t.Errorf("ValidatePortableRelative(%q) = %v", value, err)
		}
	}
	for _, value := range []string{
		"", "/absolute", "../escape", "nested/../escape", "nested\\escape", "C:/escape",
		"bad?.txt", "bad*.txt", "bad\".txt", "bad|.txt", "bad<.txt", "bad>.txt",
		"control\x01.txt", "CON.o", "nested/prn.txt", "dir./file.o", "trailing /file.o",
	} {
		if err := ValidatePortableRelative(value); err == nil {
			t.Errorf("ValidatePortableRelative(%q) unexpectedly succeeded", value)
		}
	}
}
