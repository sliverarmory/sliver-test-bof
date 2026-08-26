// Package manifest loads and validates sliver-bof-e2e/v1 manifests.
package manifest

import "time"

const (
	SchemaV1 = "sliver-bof-e2e/v1"

	ExecutorReflektor  = "reflektor"
	ExecutorCOFFLoader = "coff-loader"

	ModeSession = "session"
	ModeBeacon  = "beacon"
)

// Manifest is a fully validated manifest and its expanded execution plans.
// Paths in Config and Plans are absolute, symlink-resolved paths confined to
// Workspace.
type Manifest struct {
	Workspace string `json:"workspace"`
	Path      string `json:"path"`
	Config    Config `json:"config"`
	Plans     []Plan `json:"plans"`
}

// Config is the normalized user configuration. Defaults have been applied to
// every test and collection setting.
type Config struct {
	Schema   string       `json:"schema"`
	Sliver   SliverConfig `json:"sliver"`
	Defaults Settings     `json:"defaults"`
	Suites   []Suite      `json:"suites"`
}

type SliverConfig struct {
	Version string `json:"version"`
}

type Target struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// Key returns the stable target identifier used in reports and matrix output.
func (t Target) Key() string { return t.OS + "/" + t.Arch }

type Suite struct {
	Name        string       `json:"name"`
	Target      Target       `json:"target"`
	Tests       []Test       `json:"tests,omitempty"`
	Collections []Collection `json:"collections,omitempty"`
}

type Test struct {
	Name     string   `json:"name"`
	Object   string   `json:"object"`
	Settings Settings `json:"settings"`
}

// Collection records the normalized defaults and concrete object paths found
// for a collection. Each object also has a corresponding Plan.
type Collection struct {
	Name     string   `json:"name"`
	Glob     string   `json:"glob"`
	Settings Settings `json:"settings"`
	Objects  []string `json:"objects"`
}

type Settings struct {
	BOFExecutor    string        `json:"bof_executor"`
	Entrypoint     string        `json:"entrypoint"`
	Modes          []string      `json:"modes"`
	Repeat         int           `json:"repeat"`
	CommandTimeout time.Duration `json:"command_timeout"`
	Args           []Argument    `json:"args,omitempty"`
	RawArgs        []byte        `json:"args_raw,omitempty"`
	HasRawArgs     bool          `json:"-"`
	Expect         Expectation   `json:"expect"`
}

type Argument struct {
	Type    string `json:"type"`
	Integer uint32 `json:"integer,omitempty"`
	Short   uint16 `json:"short,omitempty"`
	String  string `json:"string,omitempty"`
	File    string `json:"file,omitempty"`
	Bytes   []byte `json:"bytes,omitempty"`
}

type Expectation struct {
	SuccessOnly bool              `json:"success_only"`
	Output      OutputExpectation `json:"output"`
}

type OutputExpectation struct {
	Equals       *string  `json:"equals,omitempty"`
	Contains     []string `json:"contains,omitempty"`
	NotContains  []string `json:"not_contains,omitempty"`
	Matches      []string `json:"matches,omitempty"`
	SHA256       string   `json:"sha256,omitempty"`
	EqualsBytes  []byte   `json:"equals_bytes,omitempty"`
	HasEqualsB64 bool     `json:"-"`
}

// Plan is one concrete BOF execution specification. Modes and Repeat describe
// the executions the harness must perform for this object.
type Plan struct {
	ID         string   `json:"id"`
	Suite      string   `json:"suite"`
	Name       string   `json:"name"`
	Collection string   `json:"collection,omitempty"`
	Object     string   `json:"object"`
	Target     Target   `json:"target"`
	Settings   Settings `json:"settings"`
}

type Matrix struct {
	Include []MatrixEntry `json:"include"`
}

type MatrixEntry struct {
	Target string `json:"target"`
	Runner string `json:"runner"`
	Key    string `json:"key"`
}
