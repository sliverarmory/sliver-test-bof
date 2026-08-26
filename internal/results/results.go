package results

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const Schema = "sliver-bof-e2e-results/v1"

type Provenance struct {
	Selector    string `json:"selector,omitempty"`
	Resolved    string `json:"resolved,omitempty"`
	ResolvedTag string `json:"resolved_tag,omitempty"`
	ResolvedSHA string `json:"resolved_sha,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	SourceBuilt bool   `json:"source_built,omitempty"`
	CacheHit    bool   `json:"cache_hit,omitempty"`
	Verified    bool   `json:"verified"`
	Source      string `json:"source,omitempty"`
	Toolchain   string `json:"toolchain,omitempty"`
}

type Report struct {
	Schema              string       `json:"schema"`
	Target              string       `json:"target"`
	ActionVersion       string       `json:"action_version,omitempty"`
	Sliver              Provenance   `json:"sliver"`
	StartedAt           time.Time    `json:"started_at"`
	FinishedAt          time.Time    `json:"finished_at"`
	InfrastructureError string       `json:"infrastructure_error,omitempty"`
	Cases               []CaseResult `json:"cases"`
}

type CaseResult struct {
	ID           string        `json:"id,omitempty"`
	Suite        string        `json:"suite"`
	Test         string        `json:"test"`
	Collection   string        `json:"collection,omitempty"`
	Object       string        `json:"object"`
	ObjectSHA256 string        `json:"object_sha256,omitempty"`
	Mode         string        `json:"mode"`
	Executor     string        `json:"executor"`
	Repeat       int           `json:"repeat"`
	Status       string        `json:"status"`
	Stage        string        `json:"stage,omitempty"`
	Duration     time.Duration `json:"-"`
	DurationMS   int64         `json:"duration_ms"`
	OutputFile   string        `json:"output_file,omitempty"`
	Error        string        `json:"error,omitempty"`
}

func New(target string) *Report {
	return &Report{Schema: Schema, Target: target, StartedAt: time.Now().UTC()}
}

func (r *Report) Failed() bool {
	if r.InfrastructureError != "" || len(r.Cases) == 0 {
		return true
	}
	for _, result := range r.Cases {
		if result.Status != "pass" {
			return true
		}
	}
	return false
}

func (r *Report) Add(result CaseResult) {
	if result.Status == "" {
		result.Status = "pass"
	}
	if result.DurationMS == 0 && result.Duration != 0 {
		result.DurationMS = result.Duration.Milliseconds()
	}
	r.Cases = append(r.Cases, result)
}

func (r *Report) Write(dir string) error {
	if r.Schema == "" {
		r.Schema = Schema
	}
	if r.FinishedAt.IsZero() {
		r.FinishedAt = time.Now().UTC()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "junit.xml"), []byte(r.JUnit()), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "summary.md"), []byte(r.Markdown()), 0o600)
}

type junitSuite struct {
	XMLName  xml.Name    `xml:"testsuite"`
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Time     string      `xml:"time,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

func (r *Report) JUnit() string {
	suite := junitSuite{Name: "sliver-bof-e2e " + r.Target, Tests: len(r.Cases)}
	if r.InfrastructureError != "" {
		suite.Tests++
		suite.Failures++
		suite.Cases = append(suite.Cases, junitCase{
			Name:      "infrastructure",
			Classname: "sliver-bof-e2e." + strings.ReplaceAll(r.Target, "/", "."),
			Time:      "0.000",
			Failure: &junitFailure{
				Message: "infrastructure",
				Body:    r.InfrastructureError,
			},
		})
	} else if len(r.Cases) == 0 {
		suite.Tests++
		suite.Failures++
		suite.Cases = append(suite.Cases, junitCase{
			Name:      "no results",
			Classname: "sliver-bof-e2e." + strings.ReplaceAll(r.Target, "/", "."),
			Time:      "0.000",
			Failure: &junitFailure{
				Message: "no results",
				Body:    "target report contains no case results",
			},
		})
	}
	for _, result := range r.Cases {
		name := fmt.Sprintf("%s [%s/%s #%d]", caseDisplayName(result), result.Mode, result.Executor, result.Repeat)
		item := junitCase{
			Name:      name,
			Classname: result.Suite + "." + strings.ReplaceAll(r.Target, "/", "."),
			Time:      fmt.Sprintf("%.3f", float64(result.DurationMS)/1000),
		}
		if result.Status != "pass" {
			suite.Failures++
			item.Failure = &junitFailure{Message: result.Stage, Body: result.Error}
		}
		suite.Cases = append(suite.Cases, item)
		suite.Time = fmt.Sprintf("%.3f", parseSeconds(suite.Time)+float64(result.DurationMS)/1000)
	}
	data, _ := xml.MarshalIndent(suite, "", "  ")
	return xml.Header + string(data) + "\n"
}

func parseSeconds(value string) float64 {
	var result float64
	_, _ = fmt.Sscanf(value, "%f", &result)
	return result
}

func (r *Report) Markdown() string {
	status := "PASS"
	if r.Failed() {
		status = "FAIL"
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "## Sliver BOF E2E: %s (%s)\n\n", r.Target, status)
	if r.Sliver.Resolved != "" {
		fmt.Fprintf(&builder, "Sliver: `%s` (`%s`)  \n", html.EscapeString(r.Sliver.Resolved), html.EscapeString(r.Sliver.SHA256))
	}
	if r.InfrastructureError != "" {
		fmt.Fprintf(&builder, "\nInfrastructure failure: `%s`\n", html.EscapeString(r.InfrastructureError))
	} else if len(r.Cases) == 0 {
		builder.WriteString("\nNo case results were recorded.\n")
	}
	builder.WriteString("\n| Suite | Test | Mode | Executor | Repeat | Status | Duration |\n")
	builder.WriteString("|---|---|---|---|---:|---|---:|\n")
	for _, result := range r.Cases {
		fmt.Fprintf(&builder, "| %s | %s | %s | %s | %d | %s | %dms |\n",
			escapeCell(result.Suite), escapeCell(caseDisplayName(result)), escapeCell(result.Mode),
			escapeCell(result.Executor), result.Repeat, strings.ToUpper(result.Status), result.DurationMS)
		if result.Error != "" {
			fmt.Fprintf(&builder, "\n`%s`: %s\n", escapeCell(caseDisplayName(result)), html.EscapeString(result.Error))
		}
	}
	return builder.String()
}

func caseDisplayName(result CaseResult) string {
	if result.ID != "" {
		return result.ID
	}
	if result.Collection != "" {
		return result.Collection + "/" + result.Test
	}
	return result.Test
}

func escapeCell(value string) string {
	return strings.ReplaceAll(html.EscapeString(value), "|", "\\|")
}

func Aggregate(root string) ([]Report, string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Name() == "report.json" && entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refuse symlinked report %s", path)
		}
		if !entry.IsDir() && entry.Type().IsRegular() && entry.Name() == "report.json" {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if len(paths) == 0 {
		return nil, "", errors.New("no report.json files found")
	}
	sort.Strings(paths)
	reports := make([]Report, 0, len(paths))
	var summary strings.Builder
	summary.WriteString("# Sliver BOF E2E aggregate\n\n")
	failed := false
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, "", err
		}
		var report Report
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&report); err != nil {
			return nil, "", fmt.Errorf("decode %s: %w", path, err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return nil, "", fmt.Errorf("decode %s: trailing JSON data", path)
		}
		if report.Schema != Schema {
			return nil, "", fmt.Errorf("%s has unsupported schema %q", path, report.Schema)
		}
		failed = failed || report.Failed()
		reports = append(reports, report)
		fmt.Fprintf(&summary, "- `%s`: %s (%d cases)\n", report.Target, map[bool]string{true: "FAIL", false: "PASS"}[report.Failed()], len(report.Cases))
	}
	if failed {
		return reports, summary.String(), errors.New("one or more target reports failed")
	}
	return reports, summary.String(), nil
}
