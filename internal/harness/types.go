package harness

import (
	"context"
	"time"

	"github.com/sliverarmory/sliver-test-bof/internal/results"
)

const (
	ExecutorReflektor  = "reflektor"
	ExecutorCOFFLoader = "coff-loader"
)

type Argument struct {
	Type   string
	String string
	Uint64 uint64
	Data   []byte
}

type OutputExpectation struct {
	Equals          *string
	Contains        []string
	NotContains     []string
	Matches         []string
	SHA256          string
	EqualsBase64    string
	HasEqualsBase64 bool
}

type Expectation struct {
	SuccessOnly bool
	Output      OutputExpectation
}

type Case struct {
	ID               string
	Suite            string
	Name             string
	Collection       string
	ObjectPath       string
	ObjectReportPath string
	ObjectData       []byte
	ObjectSHA256     string
	Entrypoint       string
	Executor         string
	Modes            []string
	Repeat           int
	CommandTimeout   time.Duration
	Arguments        []Argument
	RawArguments     []byte
	HasRawArguments  bool
	Expectation      Expectation
}

type Target struct {
	OS   string
	Arch string
}

func (target Target) String() string { return target.OS + "/" + target.Arch }

type Loader struct {
	Data   []byte
	Init   string
	Export string
	Digest string
}

type Options struct {
	Workspace      string
	ServerPath     string
	Target         Target
	Cases          []Case
	ResultsDir     string
	StateFile      string
	Sliver         results.Provenance
	ActionVersion  string
	OverallTimeout time.Duration
	StartupTimeout time.Duration
	ConnectTimeout time.Duration
	BeaconInterval time.Duration
	Loader         *Loader
	Logf           func(string, ...any)
}

type Runner interface {
	Run(context.Context, Options) (*results.Report, error)
}
