package manifest

import (
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"fmt"
	"io"
	"os"
)

func validateObject(path string, target Target) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open object: %w", err)
	}
	defer f.Close()

	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return fmt.Errorf("object header is truncated: %w", err)
	}

	switch target.OS {
	case "windows":
		if magic[0] == 'M' && magic[1] == 'Z' {
			return fmt.Errorf("expected a COFF relocatable object, got a PE executable")
		}
		return validateCOFFObject(f, target.Arch)
	case "linux":
		if magic != [4]byte{0x7f, 'E', 'L', 'F'} {
			return fmt.Errorf("expected an ELF relocatable object for %s", target.Key())
		}
		return validateELFObject(f, target.Arch)
	case "darwin":
		if magic == [4]byte{0x7f, 'E', 'L', 'F'} {
			// Sliver's Darwin BOF implementation also accepts legacy ELF
			// relocatables. Keep that compatibility explicit.
			return validateELFObject(f, target.Arch)
		}
		return validateMachOObject(f, target.Arch)
	default:
		return fmt.Errorf("unsupported target OS %q", target.OS)
	}
}

func validateELFObject(f *os.File, arch string) error {
	object, err := elf.NewFile(f)
	if err != nil {
		return fmt.Errorf("parse ELF object: %w", err)
	}
	defer object.Close()
	if object.Type != elf.ET_REL {
		return fmt.Errorf("ELF file type is %s, expected ET_REL", object.Type)
	}
	want, ok := map[string]elf.Machine{
		"386":   elf.EM_386,
		"amd64": elf.EM_X86_64,
		"arm64": elf.EM_AARCH64,
	}[arch]
	if !ok {
		return fmt.Errorf("unsupported ELF architecture %q", arch)
	}
	if object.Machine != want {
		return fmt.Errorf("ELF architecture is %s, expected %s", object.Machine, want)
	}
	return nil
}

func validateMachOObject(f *os.File, arch string) error {
	object, err := macho.NewFile(f)
	if err != nil {
		return fmt.Errorf("parse Mach-O object: %w", err)
	}
	defer object.Close()
	if object.Type != macho.TypeObj {
		return fmt.Errorf("Mach-O file type is %s, expected object", object.Type)
	}
	want, ok := map[string]macho.Cpu{
		"amd64": macho.CpuAmd64,
		"arm64": macho.CpuArm64,
	}[arch]
	if !ok {
		return fmt.Errorf("unsupported Mach-O architecture %q", arch)
	}
	if object.Cpu != want {
		return fmt.Errorf("Mach-O architecture is %s, expected %s", object.Cpu, want)
	}
	return nil
}

func validateCOFFObject(f *os.File, arch string) error {
	object, err := pe.NewFile(f)
	if err != nil {
		return fmt.Errorf("parse COFF object: %w", err)
	}
	defer object.Close()
	if object.OptionalHeader != nil {
		return fmt.Errorf("expected a COFF relocatable object, got an image with an optional header")
	}
	want, ok := map[string]uint16{
		"386":   pe.IMAGE_FILE_MACHINE_I386,
		"amd64": pe.IMAGE_FILE_MACHINE_AMD64,
		"arm64": pe.IMAGE_FILE_MACHINE_ARM64,
	}[arch]
	if !ok {
		return fmt.Errorf("unsupported COFF architecture %q", arch)
	}
	if object.Machine != want {
		return fmt.Errorf("COFF machine is 0x%04x, expected 0x%04x", object.Machine, want)
	}
	return nil
}
