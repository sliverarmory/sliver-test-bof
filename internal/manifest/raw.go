package manifest

import "gopkg.in/yaml.v3"

type rawConfig struct {
	Schema   string          `yaml:"schema"`
	Sliver   rawSliverConfig `yaml:"sliver"`
	Defaults rawDefaults     `yaml:"defaults"`
	Suites   []rawSuite      `yaml:"suites"`
}

type rawSliverConfig struct {
	Version string `yaml:"version"`
}

type rawDefaults struct {
	BOFExecutor    *string   `yaml:"bof_executor"`
	Entrypoint     *string   `yaml:"entrypoint"`
	Modes          *[]string `yaml:"modes"`
	Repeat         *int      `yaml:"repeat"`
	CommandTimeout *string   `yaml:"command_timeout"`
}

type rawSuite struct {
	Name        string          `yaml:"name"`
	Target      rawTarget       `yaml:"target"`
	Tests       []rawTest       `yaml:"tests"`
	Collections []rawCollection `yaml:"collections"`
}

type rawTarget struct {
	OS   string `yaml:"os"`
	Arch string `yaml:"arch"`
}

type rawTest struct {
	Name        string `yaml:"name"`
	Object      string `yaml:"object"`
	rawSettings `yaml:",inline"`
}

type rawCollection struct {
	Name        string                 `yaml:"name"`
	Glob        string                 `yaml:"glob"`
	Overrides   map[string]rawSettings `yaml:"overrides"`
	rawSettings `yaml:",inline"`
}

type rawSettings struct {
	BOFExecutor    *string         `yaml:"bof_executor"`
	Entrypoint     *string         `yaml:"entrypoint"`
	Modes          *[]string       `yaml:"modes"`
	Repeat         *int            `yaml:"repeat"`
	CommandTimeout *string         `yaml:"command_timeout"`
	Args           *[]rawArgument  `yaml:"args"`
	ArgsRawBase64  *string         `yaml:"args_raw_base64"`
	Expect         *rawExpectation `yaml:"expect"`
}

type rawArgument struct {
	Type  string    `yaml:"type"`
	Value yaml.Node `yaml:"value"`
}

type rawExpectation struct {
	SuccessOnly *bool                 `yaml:"success_only"`
	Output      *rawOutputExpectation `yaml:"output"`
}

type rawOutputExpectation struct {
	Equals       *string  `yaml:"equals"`
	Contains     []string `yaml:"contains"`
	NotContains  []string `yaml:"not_contains"`
	Matches      []string `yaml:"matches"`
	SHA256       string   `yaml:"sha256"`
	EqualsBase64 *string  `yaml:"equals_base64"`
}
