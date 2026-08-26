package manifest

import (
	"encoding/binary"
	"strings"
	"testing"
)

func TestValidateObjectFormats(t *testing.T) {
	workspace := t.TempDir()
	coff386 := writeCOFFObject(t, workspace, "windows-386.o", 0x014c)
	elfAMD64 := writeELFObject(t, workspace, "linux-amd64.o", 2, 62)
	machoARM64 := writeMachOObject(t, workspace, "darwin-arm64.o", 0x0100000c)

	tests := []struct {
		name   string
		path   string
		target Target
	}{
		{"COFF 386", coff386, Target{OS: "windows", Arch: "386"}},
		{"ELF amd64", elfAMD64, Target{OS: "linux", Arch: "amd64"}},
		{"legacy Darwin ELF", elfAMD64, Target{OS: "darwin", Arch: "amd64"}},
		{"Mach-O arm64", machoARM64, Target{OS: "darwin", Arch: "arm64"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateObject(test.path, test.target); err != nil {
				t.Fatalf("validateObject() error = %v", err)
			}
		})
	}
}

func TestValidateObjectRejectsWrongFormatAndArchitecture(t *testing.T) {
	workspace := t.TempDir()
	elfAMD64 := writeELFObject(t, workspace, "linux-amd64.o", 2, 62)
	machoARM64 := writeMachOObject(t, workspace, "darwin-arm64.o", 0x0100000c)
	tests := []struct {
		name   string
		path   string
		target Target
		want   string
	}{
		{"wrong ELF architecture", elfAMD64, Target{OS: "linux", Arch: "arm64"}, "ELF architecture"},
		{"ELF is not COFF", elfAMD64, Target{OS: "windows", Arch: "amd64"}, "parse COFF object"},
		{"Mach-O is not ELF", machoARM64, Target{OS: "linux", Arch: "arm64"}, "expected an ELF relocatable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateObject(test.path, test.target)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateObject() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func writeELFObject(t *testing.T, workspace, relative string, class byte, machine uint16) string {
	t.Helper()
	size := 64
	if class == 1 {
		size = 52
	}
	header := make([]byte, size)
	copy(header[0:4], []byte{0x7f, 'E', 'L', 'F'})
	header[4] = class
	header[5] = 1                                   // little endian
	header[6] = 1                                   // ELF version
	binary.LittleEndian.PutUint16(header[16:18], 1) // ET_REL
	binary.LittleEndian.PutUint16(header[18:20], machine)
	binary.LittleEndian.PutUint32(header[20:24], 1)
	if class == 1 {
		binary.LittleEndian.PutUint16(header[40:42], uint16(size))
	} else {
		binary.LittleEndian.PutUint16(header[52:54], uint16(size))
	}
	return writeFile(t, workspace, relative, header)
}

func writeMachOObject(t *testing.T, workspace, relative string, cpu uint32) string {
	t.Helper()
	header := make([]byte, 32)
	binary.LittleEndian.PutUint32(header[0:4], 0xfeedfacf)
	binary.LittleEndian.PutUint32(header[4:8], cpu)
	binary.LittleEndian.PutUint32(header[8:12], 0)
	binary.LittleEndian.PutUint32(header[12:16], 1) // MH_OBJECT
	return writeFile(t, workspace, relative, header)
}
