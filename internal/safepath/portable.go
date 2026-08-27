// Package safepath validates caller-declared paths that must behave
// consistently on every supported GitHub-hosted runner.
package safepath

import (
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// ValidatePortableRelative accepts a clean, forward-slash relative path whose
// components are valid on POSIX and Windows filesystems.
func ValidatePortableRelative(value string) error {
	if value == "" || !utf8.ValidString(value) || strings.HasPrefix(value, "/") {
		return fmt.Errorf("must be a non-empty portable relative path using forward slashes")
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f || strings.ContainsRune(`\<>:"|?*`, character) {
			return fmt.Errorf("contains character %q that is not portable across supported filesystems", character)
		}
	}
	clean := path.Clean(value)
	if clean != value || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("must be a clean relative path without . or .. components")
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return fmt.Errorf("contains invalid path component %q", component)
		}
		base := component
		if index := strings.IndexByte(base, '.'); index >= 0 {
			base = base[:index]
		}
		switch strings.ToUpper(base) {
		case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
			return fmt.Errorf("contains Windows-reserved path component %q", component)
		}
	}
	return nil
}
