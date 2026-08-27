package results

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestReportAddAndFailed(t *testing.T) {
	report := &Report{Schema: Schema, Target: "windows/amd64"}
	if !report.Failed() {
		t.Fatal("an empty report must fail")
	}

	report.Add(CaseResult{
		Suite: "suite", Test: "pass", Mode: "session", Executor: "reflektor",
		Repeat: 1, Duration: 1500 * time.Millisecond,
	})
	if report.Cases[0].Status != "pass" {
		t.Fatalf("Add() default status = %q, want pass", report.Cases[0].Status)
	}
	if report.Cases[0].DurationMS != 1500 {
		t.Fatalf("Add() duration_ms = %d, want 1500", report.Cases[0].DurationMS)
	}
	if report.Failed() {
		t.Fatal("all-pass report unexpectedly failed")
	}

	report.InfrastructureError = "listener failed"
	if !report.Failed() {
		t.Fatal("infrastructure-failed report unexpectedly passed")
	}
	report.InfrastructureError = ""
	report.Add(CaseResult{Status: "fail"})
	if !report.Failed() {
		t.Fatal("case-failed report unexpectedly passed")
	}
}

func TestReportWriteProducesJSONJUnitAndMarkdown(t *testing.T) {
	dir := t.TempDir()
	started := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	finished := started.Add(2 * time.Second)
	report := &Report{
		Schema:        Schema,
		Target:        "windows/amd64",
		ActionVersion: "sliverarmory/sliver-test-bof@abc",
		Sliver: Provenance{
			Selector: "master", Resolved: "0123456789abcdef", SHA256: "cafebabe", Verified: true,
		},
		StartedAt:  started,
		FinishedAt: finished,
	}
	report.Add(CaseResult{
		Suite: "core", Test: "whoami", Object: "build/whoami.o", ObjectSHA256: "deadbeef",
		Mode: "session", Executor: "reflektor", Repeat: 1, Duration: 1250 * time.Millisecond,
		OutputFile: "output/whoami.bin",
	})
	report.Add(CaseResult{
		Suite: "core", Test: "process", Object: "build/process.o", Mode: "beacon",
		Executor: "coff-loader", Repeat: 2, Status: "fail", Stage: "assert-output",
		DurationMS: 500, Error: `wanted "ok" & got <bad>`,
	})

	if err := report.Write(dir); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	for _, name := range []string{"report.json", "junit.xml", "summary.md"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("output %s missing or not regular: info=%v err=%v", name, info, err)
		}
	}

	jsonData := readFile(t, filepath.Join(dir, "report.json"))
	if strings.Contains(string(jsonData), `"Duration"`) || strings.Contains(string(jsonData), `"duration"`) {
		t.Fatalf("report JSON leaked internal Duration field:\n%s", jsonData)
	}
	var decoded Report
	if err := json.Unmarshal(jsonData, &decoded); err != nil {
		t.Fatalf("decode report.json: %v", err)
	}
	if decoded.Schema != Schema || decoded.Target != report.Target || decoded.ActionVersion != report.ActionVersion {
		t.Fatalf("decoded report metadata = %#v", decoded)
	}
	if !decoded.StartedAt.Equal(started) || !decoded.FinishedAt.Equal(finished) {
		t.Fatalf("decoded timestamps = %s to %s", decoded.StartedAt, decoded.FinishedAt)
	}
	if len(decoded.Cases) != 2 || decoded.Cases[0].DurationMS != 1250 || decoded.Cases[1].Stage != "assert-output" {
		t.Fatalf("decoded cases = %#v", decoded.Cases)
	}

	var junit parsedJUnitSuite
	if err := xml.Unmarshal(readFile(t, filepath.Join(dir, "junit.xml")), &junit); err != nil {
		t.Fatalf("decode junit.xml: %v", err)
	}
	if junit.Name != "sliver-bof-e2e windows/amd64" || junit.Tests != 2 || junit.Failures != 1 || junit.Time != "1.750" {
		t.Fatalf("JUnit suite = %#v", junit)
	}
	if junit.Cases[0].Name != "whoami [session/reflektor #1]" || junit.Cases[0].Classname != "core.windows.amd64" {
		t.Fatalf("JUnit passing case = %#v", junit.Cases[0])
	}
	if junit.Cases[1].Failure == nil || junit.Cases[1].Failure.Message != "assert-output" || junit.Cases[1].Failure.Body != report.Cases[1].Error {
		t.Fatalf("JUnit failing case = %#v", junit.Cases[1])
	}

	markdown := string(readFile(t, filepath.Join(dir, "summary.md")))
	for _, want := range []string{
		"## Sliver BOF E2E: windows/amd64 (FAIL)",
		"Sliver: `0123456789abcdef` (`cafebabe`)",
		"| core | whoami | session | reflektor | 1 | PASS | 1250ms |",
		"wanted &#34;ok&#34; &amp; got &lt;bad&gt;",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("Markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestJUnitRepresentsInfrastructureAndEmptyReportFailures(t *testing.T) {
	t.Run("infrastructure", func(t *testing.T) {
		report := &Report{
			Schema: Schema, Target: "linux/arm64", InfrastructureError: "server <exited> & unavailable",
			Cases: []CaseResult{{Suite: "suite", Test: "started", Mode: "session", Executor: "reflektor", Repeat: 1, Status: "pass"}},
		}
		var junit parsedJUnitSuite
		if err := xml.Unmarshal([]byte(report.JUnit()), &junit); err != nil {
			t.Fatalf("decode JUnit: %v", err)
		}
		if junit.Tests != 2 || junit.Failures != 1 {
			t.Fatalf("JUnit counts = tests %d failures %d", junit.Tests, junit.Failures)
		}
		if len(junit.Cases) != 2 || junit.Cases[0].Name != "infrastructure" || junit.Cases[0].Failure == nil {
			t.Fatalf("infrastructure JUnit case = %#v", junit.Cases)
		}
		if junit.Cases[0].Failure.Body != report.InfrastructureError {
			t.Fatalf("infrastructure failure body = %q", junit.Cases[0].Failure.Body)
		}
	})

	t.Run("empty", func(t *testing.T) {
		report := &Report{Schema: Schema, Target: "darwin/arm64"}
		var junit parsedJUnitSuite
		if err := xml.Unmarshal([]byte(report.JUnit()), &junit); err != nil {
			t.Fatalf("decode JUnit: %v", err)
		}
		if junit.Tests != 1 || junit.Failures != 1 || len(junit.Cases) != 1 || junit.Cases[0].Name != "no results" {
			t.Fatalf("empty-report JUnit = %#v", junit)
		}
		if markdown := report.Markdown(); !strings.Contains(markdown, "No case results were recorded.") {
			t.Fatalf("empty-report Markdown lacks diagnostic:\n%s", markdown)
		}
	})
}

func TestMarkdownEscapesTableCellsAndInfrastructureError(t *testing.T) {
	report := &Report{
		Schema: Schema, Target: "windows/amd64", InfrastructureError: "server <down> & gone",
		Cases: []CaseResult{{
			Suite: "suite|one", Test: "<test>", Mode: "session", Executor: "reflektor",
			Repeat: 1, Status: "fail", Error: "bad <output> & state",
		}},
	}
	markdown := report.Markdown()
	for _, want := range []string{
		"Infrastructure failure: `server &lt;down&gt; &amp; gone`",
		"| suite\\|one | &lt;test&gt; |",
		"bad &lt;output&gt; &amp; state",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("Markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestReportWriteNormalizesInvalidUTF8(t *testing.T) {
	dir := t.TempDir()
	invalid := "before" + string([]byte{0xff}) + "<after>&"
	report := &Report{
		Schema:              Schema,
		Target:              "windows/386",
		InfrastructureError: invalid,
		Cases: []CaseResult{{
			Suite: "suite", Test: "test", Mode: "session", Executor: "reflektor",
			Repeat: 1, Status: "fail", Stage: "execute", Error: invalid,
		}},
	}
	if err := report.Write(dir); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	jsonData := readFile(t, filepath.Join(dir, "report.json"))
	junitData := readFile(t, filepath.Join(dir, "junit.xml"))
	markdownData := readFile(t, filepath.Join(dir, "summary.md"))
	for name, data := range map[string][]byte{
		"report.json": jsonData,
		"junit.xml":   junitData,
		"summary.md":  markdownData,
	} {
		if !utf8.Valid(data) {
			t.Errorf("%s is not valid UTF-8", name)
		}
		if strings.IndexByte(string(data), 0xff) >= 0 {
			t.Errorf("%s contains the invalid source byte", name)
		}
	}

	var decoded Report
	if err := json.Unmarshal(jsonData, &decoded); err != nil {
		t.Fatalf("decode report.json: %v", err)
	}
	if !strings.Contains(decoded.InfrastructureError, "\uFFFD") || !strings.Contains(decoded.Cases[0].Error, "\uFFFD") {
		t.Fatalf("report.json did not normalize invalid UTF-8: %#v", decoded)
	}

	var junit parsedJUnitSuite
	if err := xml.Unmarshal(junitData, &junit); err != nil {
		t.Fatalf("decode junit.xml: %v", err)
	}
	if len(junit.Cases) != 2 || junit.Cases[0].Failure == nil || junit.Cases[1].Failure == nil {
		t.Fatalf("JUnit cases = %#v", junit.Cases)
	}
	if !strings.Contains(junit.Cases[0].Failure.Body, "\uFFFD") || !strings.Contains(junit.Cases[1].Failure.Body, "\uFFFD") {
		t.Fatalf("junit.xml did not normalize invalid UTF-8: %#v", junit.Cases)
	}

	markdown := string(markdownData)
	for _, want := range []string{"before\uFFFD&lt;after&gt;&amp;", "Infrastructure failure:"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("summary.md missing %q:\n%s", want, markdown)
		}
	}
}

func TestAggregateSuccessAndFailure(t *testing.T) {
	t.Run("success is deterministic", func(t *testing.T) {
		root := t.TempDir()
		writeTestReport(t, filepath.Join(root, "z-windows"), passingReport("windows/amd64"))
		writeTestReport(t, filepath.Join(root, "a-linux"), passingReport("linux/arm64"))

		reports, summary, err := Aggregate(root)
		if err != nil {
			t.Fatalf("Aggregate() error = %v", err)
		}
		if len(reports) != 2 || reports[0].Target != "linux/arm64" || reports[1].Target != "windows/amd64" {
			t.Fatalf("Aggregate() reports = %#v", reports)
		}
		want := "# Sliver BOF E2E aggregate\n\n- `linux/arm64`: PASS (1 cases)\n- `windows/amd64`: PASS (1 cases)\n"
		if summary != want {
			t.Fatalf("Aggregate() summary = %q, want %q", summary, want)
		}
	})

	t.Run("case failure returns reports and summary", func(t *testing.T) {
		root := t.TempDir()
		passing := passingReport("linux/arm64")
		failing := passingReport("windows/amd64")
		failing.Cases[0].Status = "fail"
		failing.Cases[0].Stage = "execute"
		failing.Cases[0].Error = "BOF failed"
		writeTestReport(t, filepath.Join(root, "linux"), passing)
		writeTestReport(t, filepath.Join(root, "windows"), failing)

		reports, summary, err := Aggregate(root)
		if err == nil || !strings.Contains(err.Error(), "one or more target reports failed") {
			t.Fatalf("Aggregate() error = %v", err)
		}
		if len(reports) != 2 || !strings.Contains(summary, "`windows/amd64`: FAIL") {
			t.Fatalf("Aggregate() reports/summary = %#v / %q", reports, summary)
		}
	})

	t.Run("empty report fails", func(t *testing.T) {
		root := t.TempDir()
		writeTestReport(t, filepath.Join(root, "empty"), Report{Schema: Schema, Target: "darwin/arm64"})
		_, summary, err := Aggregate(root)
		if err == nil || !strings.Contains(summary, "`darwin/arm64`: FAIL (0 cases)") {
			t.Fatalf("Aggregate(empty) = summary %q, error %v", summary, err)
		}
	})
}

func TestAggregateRejectsMissingMalformedAndUnsupportedReports(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		_, _, err := Aggregate(t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "no report.json files found") {
			t.Fatalf("Aggregate() error = %v", err)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		root := t.TempDir()
		writeRawReport(t, root, []byte(`{"schema":`))
		_, _, err := Aggregate(root)
		if err == nil || !strings.Contains(err.Error(), "decode") {
			t.Fatalf("Aggregate() error = %v", err)
		}
	})

	t.Run("unsupported schema", func(t *testing.T) {
		root := t.TempDir()
		data, err := json.Marshal(Report{Schema: "sliver-bof-e2e-results/v2", Target: "linux/amd64"})
		if err != nil {
			t.Fatal(err)
		}
		writeRawReport(t, root, data)
		_, _, err = Aggregate(root)
		if err == nil || !strings.Contains(err.Error(), `unsupported schema "sliver-bof-e2e-results/v2"`) {
			t.Fatalf("Aggregate() error = %v", err)
		}
	})

	t.Run("unknown fields", func(t *testing.T) {
		root := t.TempDir()
		writeRawReport(t, root, []byte(`{"schema":"sliver-bof-e2e-results/v1","target":"linux/amd64","sliver":{"verified":true},"started_at":"0001-01-01T00:00:00Z","finished_at":"0001-01-01T00:00:00Z","cases":[],"untrusted":true}`))
		_, _, err := Aggregate(root)
		if err == nil || !strings.Contains(err.Error(), `unknown field "untrusted"`) {
			t.Fatalf("Aggregate() error = %v", err)
		}
	})

	t.Run("trailing JSON", func(t *testing.T) {
		root := t.TempDir()
		data, err := json.Marshal(Report{Schema: Schema, Target: "linux/amd64"})
		if err != nil {
			t.Fatal(err)
		}
		writeRawReport(t, root, append(data, []byte(` {}`)...))
		_, _, err = Aggregate(root)
		if err == nil || !strings.Contains(err.Error(), "trailing JSON data") {
			t.Fatalf("Aggregate() error = %v", err)
		}
	})

	t.Run("symlinked report", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "outside.json")
		if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		linkDir := filepath.Join(root, "artifact")
		if err := os.Mkdir(linkDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(linkDir, "report.json")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		_, _, err := Aggregate(root)
		if err == nil || !strings.Contains(err.Error(), "refuse symlinked report") {
			t.Fatalf("Aggregate() error = %v", err)
		}
	})
}

type parsedJUnitSuite struct {
	Name     string            `xml:"name,attr"`
	Tests    int               `xml:"tests,attr"`
	Failures int               `xml:"failures,attr"`
	Time     string            `xml:"time,attr"`
	Cases    []parsedJUnitCase `xml:"testcase"`
}

type parsedJUnitCase struct {
	Name      string              `xml:"name,attr"`
	Classname string              `xml:"classname,attr"`
	Time      string              `xml:"time,attr"`
	Failure   *parsedJUnitFailure `xml:"failure"`
}

type parsedJUnitFailure struct {
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

func passingReport(target string) Report {
	return Report{
		Schema: Schema, Target: target,
		StartedAt:  time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2026, 8, 25, 12, 0, 1, 0, time.UTC),
		Cases: []CaseResult{{
			Suite: "suite", Test: "smoke", Object: "smoke.o", Mode: "session",
			Executor: "reflektor", Repeat: 1, Status: "pass", DurationMS: 10,
		}},
	}
}

func writeTestReport(t *testing.T, dir string, report Report) {
	t.Helper()
	if err := report.Write(dir); err != nil {
		t.Fatalf("Write(%q) error = %v", dir, err)
	}
}

func writeRawReport(t *testing.T, root string, data []byte) {
	t.Helper()
	path := filepath.Join(root, "report.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	return data
}
