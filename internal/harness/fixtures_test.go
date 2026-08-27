package harness

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPrepareFixturesConfinesPortableFileAndDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "case", "fixtures")
	variables, err := prepareFixtures(root, Fixtures{
		File: &FileFixture{Variable: "FIXTURE_FILE", ContentsUTF8: "file contents\n"},
		Directory: &DirectoryFixture{Variable: "FIXTURE_DIR", Files: map[string]string{
			"marker.txt":       "marker\n",
			"nested/child.txt": "child\n",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	file := variables["FIXTURE_FILE"]
	directory := variables["FIXTURE_DIR"]
	if !filepath.IsAbs(file) || !filepath.IsAbs(directory) || !pathWithin(root, file) || !pathWithin(root, directory) {
		t.Fatalf("fixture variables escaped root: %#v", variables)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "file contents\n" {
		t.Fatalf("file fixture = %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(directory, "nested", "child.txt")); err != nil || string(data) != "child\n" {
		t.Fatalf("directory fixture child = %q, %v", data, err)
	}
	if runtime.GOOS != "windows" {
		fileInfo, _ := os.Stat(file)
		directoryInfo, _ := os.Stat(directory)
		if fileInfo.Mode().Perm() != 0o600 || directoryInfo.Mode().Perm() != 0o700 {
			t.Fatalf("fixture modes = file %o directory %o", fileInfo.Mode().Perm(), directoryInfo.Mode().Perm())
		}
	}
}

func TestSubstituteFixtureArgumentsIsStrictAndDoesNotMutateInput(t *testing.T) {
	original := []Argument{
		{Type: "string", String: "${FIXTURE_FILE}"},
		{Type: "wstring", String: "${FIXTURE_DIR}"},
		{Type: "string", String: "literal"},
		{Type: "file", Data: []byte{1, 2, 3}},
	}
	resolved, err := substituteFixtureArguments(original, map[string]string{
		"FIXTURE_FILE": "/private/file",
		"FIXTURE_DIR":  "/private/directory",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved[0].String != "/private/file" || resolved[1].String != "/private/directory" || resolved[2].String != "literal" {
		t.Fatalf("resolved arguments = %#v", resolved)
	}
	resolved[3].Data[0] = 9
	if original[0].String != "${FIXTURE_FILE}" || original[3].Data[0] != 1 {
		t.Fatalf("input arguments were mutated: %#v", original)
	}

	for _, value := range []string{"${UNKNOWN}", "prefix-${FIXTURE_FILE}", "${FIXTURE_FILE}/.."} {
		_, err := substituteFixtureArguments([]Argument{{Type: "string", String: value}}, map[string]string{"FIXTURE_FILE": "/private/file"})
		if err == nil {
			t.Fatalf("substituteFixtureArguments(%q) unexpectedly succeeded", value)
		}
	}
}

func TestLoopbackTCPFixtureAcceptsConnectionsAndOverridesPort(t *testing.T) {
	fixture, err := startLoopbackTCPFixture()
	if err != nil {
		t.Fatal(err)
	}
	port := fixture.Port()
	if port < 1 || port > 65535 {
		t.Fatalf("port = %d", port)
	}
	arguments := []Argument{{Type: "string", String: "127.0.0.1"}, {Type: "int", Uint64: 1}}
	resolved, err := applyTCPListenerArgument(arguments, &TCPListener{PortArgument: 1}, port)
	if err != nil {
		t.Fatal(err)
	}
	if resolved[1].Uint64 != uint64(port) || arguments[1].Uint64 != 1 {
		t.Fatalf("resolved port = %d; original = %d", resolved[1].Uint64, arguments[1].Uint64)
	}
	connection, err := net.DialTimeout("tcp4", fixture.listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	if err := fixture.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
	if _, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strings.TrimPrefix(fixture.listener.Addr().String(), "127.0.0.1:")), 100*time.Millisecond); err == nil {
		t.Fatal("closed TCP fixture still accepted a connection")
	}
}

func TestTCPListenerArgumentRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name      string
		arguments []Argument
		listener  *TCPListener
		port      int
	}{
		{"bad port", []Argument{{Type: "int"}}, &TCPListener{PortArgument: 0}, 0},
		{"bad index", []Argument{{Type: "int"}}, &TCPListener{PortArgument: 1}, 1234},
		{"bad type", []Argument{{Type: "short"}}, &TCPListener{PortArgument: 0}, 1234},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := applyTCPListenerArgument(test.arguments, test.listener, test.port); err == nil {
				t.Fatal("applyTCPListenerArgument() unexpectedly succeeded")
			}
		})
	}
}
