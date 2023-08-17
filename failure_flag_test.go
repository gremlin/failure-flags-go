//nolint:errcheck
package golang

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// AfterSpy is a very simple, manually created spy object that will let us measure After invocations.
type AfterSpy struct {
	sync.Mutex
	CallDurations []time.Duration
}

func (spy *AfterSpy) Reset() {
	spy.Lock()
	defer spy.Unlock()
	spy.CallDurations = []time.Duration{}
}

// After fakes the delay typically associated with calls to time.After and keeps data about invocations.
func (spy *AfterSpy) After(d time.Duration) <-chan time.Time {
	spy.Lock()
	defer spy.Unlock()
	spy.CallDurations = append(spy.CallDurations, d)
	resultChan := make(chan time.Time)
	go func() {
		resultChan <- time.Now().Add(d)
		close(resultChan)
	}()
	return resultChan
}

// RequesterSpy is a very simple and manually created spy that will allow tests to inject experiments
// to FetchExperiment and measure those interactions without running a server.
type RequesterSpy struct {
	sync.Mutex
	Calls     []*http.Request
	Responses []Experiment
	Delay     time.Duration
}

func (spy *RequesterSpy) Reset() {
	spy.Lock()
	defer spy.Unlock()
	spy.Calls = []*http.Request{}
}

func (spy *RequesterSpy) Reconfigure(responses []Experiment, delay time.Duration) {
	spy.Lock()
	defer spy.Unlock()
	spy.Responses = responses
	spy.Delay = delay
}

// Responder constructs Requester functions that interact with the spy and respond with test-provided data.
func (spy *RequesterSpy) Requester(out chan []Experiment, in *http.Request, logf Logf) {
	spy.Lock()
	defer spy.Unlock()
	spy.Calls = append(spy.Calls, in)

	if spy.Delay > 0*time.Second {
		if logf != nil {
			logf(`RequesterSpy delaying: %v`, spy.Delay)
		}
		<-time.After(spy.Delay)
		if logf != nil {
			logf(`RequesterSpy delay complete`)
		}
	}
	if spy.Responses == nil {
		if logf != nil {
			logf(`RequesterSpy closing the channel without data`)
		}
		close(out)
	} else {
		if logf != nil {
			logf(`RequesterSpy closing the channel with data, %v`, spy.Responses)
		}
		out <- spy.Responses
		close(out)
	}
}

type MockTransport struct {
	sync.Mutex
	Calls    []*http.Request
	Response *bufio.Reader
	Delay    time.Duration
	Error    error
}

func (mt *MockTransport) Reset() {
	mt.Lock()
	defer mt.Unlock()
	mt.Calls = []*http.Request{}
}

func (mt *MockTransport) Reconfigure(response *bufio.Reader, delay time.Duration, err error) {
	mt.Lock()
	defer mt.Unlock()
	mt.Response = response
	mt.Delay = delay
	mt.Error = err
}

func (mt *MockTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	mt.Lock()
	defer mt.Unlock()
	mt.Calls = append(mt.Calls, r)

	if mt.Delay > 0*time.Second {
		<-time.After(mt.Delay)
	}
	if mt.Error != nil {
		return nil, mt.Error
	}
	return http.ReadResponse(mt.Response, nil)
}

func noopLogf(f string, c ...any) (int, error) { return 0, nil }

var (
	experiments = map[string]Experiment{
		`noExperiments`: Experiment{},
		`simpleLatencyNumber`: Experiment{
			Name:   `simpleLatencyNumber`,
			Effect: map[string]json.RawMessage{"latency": json.RawMessage([]byte("20"))},
		},
		`simpleLatencyString`: Experiment{
			Name:   `simpleLatencyString`,
			Effect: map[string]json.RawMessage{"latency": json.RawMessage([]byte("\"30\""))},
		},
		`simpleLatencyObject`: Experiment{
			Name:   `simpleLatencyObject`,
			Effect: map[string]json.RawMessage{"latency": json.RawMessage([]byte("{\"ms\":40}"))},
		},
		`shortLatency`: Experiment{
			Name:   `simpleLatencyNumber`,
			Effect: map[string]json.RawMessage{"latency": json.RawMessage([]byte("5"))},
		},
		`latencyPanic`: Experiment{
			Name:   `latencyPanic`,
			Effect: map[string]json.RawMessage{"latency": json.RawMessage([]byte("5")), "panic": json.RawMessage([]byte(`"cp provided panic"`))},
		},
		`customBehaviorOnly`: Experiment{
			Name:   `customBehaviorOnly`,
			Effect: map[string]json.RawMessage{"custom": json.RawMessage([]byte("5"))},
		},
	}

	rawExperimentPayload = `[{"id": "8675309", "name":"exp name", "protected": false, "startTime": "sometime", "rate": .50, "flag":{"name": "-", labels: {}}, "effect": {"latency": 5}}]`
	noExperimentPayload  = `[]`
	emptyPayload         = ``
	textPayload          = `hello world`

	responses = map[string]*bufio.Reader{
		"none":             bufio.NewReader(bytes.NewBuffer([]byte(fmt.Sprintf("HTTP/1.1 200 OK\nContent-Type: application/json\nContent-Length: %d\n\n%s", len([]byte(noExperimentPayload)), noExperimentPayload)))),
		"empty":            bufio.NewReader(bytes.NewBuffer([]byte(fmt.Sprintf("HTTP/1.1 200 OK\nContent-Type: application/json\nContent-Length: %d\n\n%s", len([]byte(emptyPayload)), emptyPayload)))),
		"wrongContentType": bufio.NewReader(bytes.NewBuffer([]byte(fmt.Sprintf("HTTP/1.1 200 OK\nContent-Type: text/html\nContent-Length: %d\n\n%s", len([]byte(textPayload)), textPayload)))),
		"simpleLatency":    bufio.NewReader(bytes.NewBuffer([]byte(fmt.Sprintf("HTTP/1.1 200 OK\nContent-Type: application/json\nContent-Length: %d\n\n%s", len([]byte(rawExperimentPayload)), rawExperimentPayload)))),
		"notFound":         bufio.NewReader(bytes.NewBuffer([]byte("HTTP/1.1 404 Not Found\n\n"))),
	}
)

//\\//\\//\\//\\//\\//\\//\\//\\//\\//\\
// Fetch Tests
//\\//\\//\\//\\//\\//\\//\\//\\//\\//\\
func TestFetchExperiment(t *testing.T) {
	originalRequest := request
	requesterSpy := &RequesterSpy{}
	request = requesterSpy.Requester
	defer func() {
		request = originalRequest
	}()

	cases := []struct {
		Name      string
		Flag      FailureFlag
		Responses []Experiment
		Delay     time.Duration

		ExpectedRequesterCalls int
		ExpectedErr            error
	}{
		{`No experiments`, FailureFlag{Logf: nil}, []Experiment{}, 0 * time.Second, 1, nil},
		{`Late response`, FailureFlag{Logf: nil}, []Experiment{}, 20 * time.Millisecond, 1, errors.New(`timeout while fetching experiments`)},
		{`Error retrieving experiments`, FailureFlag{Logf: nil}, nil, 0 * time.Second, 1,
			fmt.Errorf(`unable to retrieve experiments`)},
		{`One experiment`, FailureFlag{Name: "-", Logf: nil},
			[]Experiment{experiments[`simpleLatencyNumber`]}, 0 * time.Second, 1, nil},
		{`Two experiments`, FailureFlag{Name: "-", Logf: nil},
			[]Experiment{experiments[`simpleLatencyNumber`], experiments[`simpleLatencyString`]}, 0 * time.Second, 1, nil},
	}

	for _, c := range cases {
		requesterSpy.Reconfigure(c.Responses, c.Delay)
		requesterSpy.Reset()
		backoffUntil = time.Now().Add(time.Minute * -1)

		result, err := FetchExperiment(c.Flag)
		if err != nil && c.ExpectedErr == nil {
			t.Errorf(`case "%v" failed: unexpected error, %v`, c.Name, err)
		} else if err == nil && c.ExpectedErr != nil {
			t.Errorf(`case "%v" failed: missing expected error, %v`, c.Name, c.ExpectedErr)
		} else if err != nil && c.ExpectedErr != nil && err.Error() != c.ExpectedErr.Error() {
			t.Errorf(`case "%v" failed: incorrect expected error, expected "%v", actual "%v"`, c.Name, c.ExpectedErr, err)
		}
		requesterSpy.Lock()
		if c.ExpectedRequesterCalls != len(requesterSpy.Calls) {
			t.Errorf(`case "%v" failed: unexpected number of Requester calls, expected "%v", actual "%v"`,
				c.Name, c.ExpectedRequesterCalls, requesterSpy.Calls)
		}
		requesterSpy.Unlock()
		if len(result) != len(c.Responses) {
			t.Errorf(`case "%v" failed: unexpected result, expected "%v", actual "%v"`, c.Name, c.Responses, result)
		}
	}
}

func TestFullFetchExperiment(t *testing.T) {
	originalTransport := defaultTransport
	transport := &MockTransport{}
	defaultTransport = transport
	defer func() {
		defaultTransport = originalTransport
	}()

	cases := []struct {
		Name           string
		Flag           FailureFlag
		ServerResponse *bufio.Reader
		Delay          time.Duration
		Error          error

		ExpectedCallCount   int
		ExpectedErr         error
		ExpectedExperiments []Experiment
	}{
		{`no experiments`, FailureFlag{Name: `-`, Logf: noopLogf}, responses[`none`], 0 * time.Millisecond, nil, 1, nil, nil},
		{`wrong content type`, FailureFlag{Name: `-`, Logf: noopLogf}, responses[`wrongContentType`], 0 * time.Millisecond, nil, 1, errors.New(`unable to retrieve experiments`), nil},
		{`empty response`, FailureFlag{Name: `-`, Logf: noopLogf}, responses[`empty`], 0 * time.Millisecond, nil, 1, errors.New(`unable to retrieve experiments`), nil},
		{`not found`, FailureFlag{Name: `-`, Logf: noopLogf}, responses[`notFound`], 0 * time.Millisecond, nil, 1, errors.New(`unable to retrieve experiments`), nil},
		{`not responding`, FailureFlag{Name: `-`, Logf: noopLogf}, nil, 100 * time.Millisecond, errors.New(`unable to reach the sidecar`), 1, errors.New(`timeout while fetching experiments`), nil},
	}
	for _, c := range cases {
		transport.Reconfigure(c.ServerResponse, c.Delay, c.Error)
		transport.Reset()
		backoffUntil = time.Now().Add(time.Minute * -1)

		result, err := FetchExperiment(c.Flag)

		if err != nil && c.ExpectedErr == nil {
			t.Errorf(`case "%v" failed: unexpected error, %v`, c.Name, err)
		} else if err == nil && c.ExpectedErr != nil {
			t.Errorf(`case "%v" failed: missing expected error, %v`, c.Name, c.ExpectedErr)
		} else if err != nil && c.ExpectedErr != nil && err.Error() != c.ExpectedErr.Error() {
			t.Errorf(`case "%v" failed: incorrect expected error, expected "%v", actual "%v"`, c.Name, c.ExpectedErr, err)
		}
		transport.Lock()
		if c.ExpectedCallCount != len(transport.Calls) {
			t.Errorf(`case "%v" failed: unexpected number of transport calls, expected "%v", actual "%v"`,
				c.Name, c.ExpectedCallCount, transport.Calls)
		}
		transport.Unlock()
		if len(result) != len(c.ExpectedExperiments) {
			t.Errorf(`case "%v" failed: unexpected result, expected "%v", actual "%v"`, c.Name, c.ExpectedExperiments, result)
		}
	}
}

//\\//\\//\\//\\//\\//\\//\\//\\//\\//\\
// Invoke Tests
//\\//\\//\\//\\//\\//\\//\\//\\//\\//\\
func TestInvoke(t *testing.T) {
	requesterSpy := &RequesterSpy{}
	request = requesterSpy.Requester

	cases := []struct {
		Name      string
		Flag      FailureFlag
		Responses []Experiment
		Delay     time.Duration

		ExpectedRequesterCalls int
		ExpectedActive         bool
		ExpectedImpacted       bool
		ExpectedErr            error
		ExpectedPanic          bool
	}{
		{`No flag name`, FailureFlag{Logf: nil}, []Experiment{}, 0 * time.Second, 0, false, false, nil, false},
		{`No experiments`, FailureFlag{Name: "-", Logf: nil}, []Experiment{}, 0 * time.Second, 1, false, false, nil, false},
		{`No experiments with debug`, FailureFlag{Name: "-", Logf: noopLogf}, []Experiment{}, 0 * time.Second, 1, false, false, nil, false},
		{`Late response`, FailureFlag{Name: "-", Logf: nil}, []Experiment{}, 20 * time.Millisecond, 1, false, false, nil, false},
		{`Error retrieving experiments`, FailureFlag{Name: "-", Logf: nil}, nil, 0 * time.Second, 1, false, false, nil, false}, // Invoke only returns errors if the experiment describes them
		{`One experiment with debug`, FailureFlag{Name: "-", Logf: noopLogf},
			[]Experiment{experiments[`shortLatency`]}, 0 * time.Second, 1, true, true, nil, false},
		{`One experiment with debug and combination latency+panic`, FailureFlag{Name: "-", Logf: noopLogf},
			[]Experiment{experiments[`latencyPanic`]}, 0 * time.Second, 1, true, true, nil, true},
		{`One experiment for custom behavior, no impact`, FailureFlag{Name: "-", Logf: nil},
			[]Experiment{experiments[`onlyCustomBehavior`]}, 0 * time.Second, 1, true, false, nil, false},
		{`Two experiments`, FailureFlag{Name: "-", Logf: nil},
			[]Experiment{experiments[`shortLatency`], experiments[`shortLatency`]}, 0 * time.Second, 1, true, true, nil, false},
	}

	for _, c := range cases {
		// setup
		requesterSpy.Reconfigure(c.Responses, c.Delay)
		requesterSpy.Reset()
		backoffUntil = time.Now().Add(time.Minute * -1)

		// run
		if c.Flag.Logf != nil {
			c.Flag.Logf(`starting case %v`, c.Name)
		}
		paniced := false
		var active, impacted bool
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					paniced = true
					return
				}
				paniced = false
			}()
			active, impacted, err = Invoke(c.Flag)
		}()

		// validate
		if paniced != c.ExpectedPanic {
			t.Errorf(`case "%v" failed: panic, expected %v, actual %v`, c.Name, c.ExpectedPanic, paniced)
		}
		if c.ExpectedPanic {
			continue
		}
		if err != nil && c.ExpectedErr == nil {
			t.Errorf(`case "%v" failed: unexpected error, %v`, c.Name, err)
		} else if err == nil && c.ExpectedErr != nil {
			t.Errorf(`case "%v" failed: missing expected error, %v`, c.Name, c.ExpectedErr)
		} else if err != nil && c.ExpectedErr != nil && err.Error() != c.ExpectedErr.Error() {
			t.Errorf(`case "%v" failed: incorrect expected error, expected "%v", actual "%v"`, c.Name, c.ExpectedErr, err)
		}
		requesterSpy.Lock()
		if c.ExpectedRequesterCalls != len(requesterSpy.Calls) {
			t.Errorf(`case "%v" failed: unexpected number of Requester calls, expected "%v", actual "%v"`,
				c.Name, c.ExpectedRequesterCalls, requesterSpy.Calls)
		}
		requesterSpy.Unlock()
		if active != c.ExpectedActive {
			t.Errorf(`case "%v" failed: expected active %v, actual active %v`, c.Name, c.ExpectedActive, active)
		}
		if impacted != c.ExpectedImpacted {
			t.Errorf(`case "%v" failed: expected impacted %v, actual impacted %v`, c.Name, c.ExpectedImpacted, impacted)
		}
	}
}

//\\//\\//\\//\\//\\//\\//\\//\\//\\//\\
// Behavior Tests
//\\//\\//\\//\\//\\//\\//\\//\\//\\//\\

func TestLatency(t *testing.T) {
	afterSpy := AfterSpy{}
	after = afterSpy.After
	defer func() {
		after = time.After
	}()
	defer func() {
		if r := recover(); r != nil {
			t.Fatal(`unexpected panic`, r)
		}
	}()

	cases := []struct {
		Name          string
		Flag          FailureFlag
		Experiments   []Experiment
		ExpectedErr   error
		CallDurations []time.Duration
	}{
		{`none`, FailureFlag{Name: `noexperiments`}, []Experiment{}, nil, []time.Duration{}},
		{`oneAndDoneNumber`, FailureFlag{Name: `-`}, []Experiment{experiments[`simpleLatencyNumber`]},
			nil, []time.Duration{time.Duration(20) * time.Millisecond}},
		{`oneAndDoneString`, FailureFlag{Name: `-`}, []Experiment{experiments[`simpleLatencyString`]},
			nil, []time.Duration{time.Duration(30) * time.Millisecond}},
		{`oneAndDoneObject`, FailureFlag{Name: `-`}, []Experiment{experiments[`simpleLatencyObject`]},
			nil, []time.Duration{time.Duration(40) * time.Millisecond}},
		{`two`, FailureFlag{Name: `-`}, []Experiment{experiments[`simpleLatencyString`], experiments[`simpleLatencyNumber`]},
			nil, []time.Duration{time.Duration(30) * time.Millisecond, time.Duration(20) * time.Millisecond}},
	}

	for _, c := range cases {
		afterSpy.Reset()
		impacted, err := Latency(c.Flag, c.Experiments)
		if err != nil {
			t.Errorf(`case "%v" failed: unexpected error, %v`, c.Name, err)
		}
		if impacted != (len(c.CallDurations) > 0) {
			t.Errorf(`case "%v" failed: impact observed but not reported`, c.Name)
		}
		if len(afterSpy.CallDurations) != len(c.CallDurations) {
			t.Errorf(`case "%v" failed: called After %v times, expected %v`, c.Name, len(afterSpy.CallDurations), len(c.CallDurations))
			continue
		}
		for j, d := range c.CallDurations {
			if sd := afterSpy.CallDurations[j]; sd != d {
				t.Errorf(`case "%v" failed: unexpected After duration calls, expected %v, actual %v`, c.Name, afterSpy.CallDurations, c.CallDurations)
				break
			}
		}
	}
}

func TestException(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatal(`unexpected panic`, r)
		}
	}()

	cases := []struct {
		Name        string
		Flag        FailureFlag
		Experiments []Experiment
		ExpectedErr error
	}{
		{`Nothing expected, nothing specified`, FailureFlag{Name: `-`}, []Experiment{}, nil},
		{`Something expected, something specified`, FailureFlag{Name: `-`}, []Experiment{Experiment{
			Name:   `simpleExceptionString`,
			Effect: map[string]json.RawMessage{"exception": json.RawMessage([]byte("\"cp provided message\""))},
		}}, fmt.Errorf(`cp provided message`)},
	}
	for _, c := range cases {
		impacted, err := Exception(c.Flag, c.Experiments)
		if c.ExpectedErr == nil && err != nil {
			t.Errorf(`case "%v" failed: unexpected error, %v`, c.Name, err)
		} else if c.ExpectedErr != nil && err == nil {
			t.Errorf(`case "%v" failed: missing expected error, "%v"`, c.Name, c.ExpectedErr.Error())
		} else if (c.ExpectedErr != nil && err != nil) && err.Error() != c.ExpectedErr.Error() {
			t.Errorf(`case "%v" failed: incorrect error, expected "%v", actual "%v"`, c.Name, c.ExpectedErr.Error(), err.Error())
		}
		if err != nil && !impacted {
			t.Errorf(`case "%v" failed: not impacted but error returned`, c.Name)
		} else if err == nil && impacted {
			t.Errorf(`case "%v" failed: impacted but no error returned`, c.Name)
		}
	}
}

func TestShouldPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok {
				if err.Error() == "cp provided message" {
					return
				}
				t.Fatal(`panic had incorrect Error`)
				return
			}
			t.Fatal(`recover did not produce an error`)
		}
		t.Fatal(`should have paniced`)
	}()
	Panic(FailureFlag{Name: `-`}, []Experiment{Experiment{
		Name:   `simpleLatencyString`,
		Effect: map[string]json.RawMessage{"panic": json.RawMessage([]byte("\"cp provided message\""))},
	}})
}

func TestShouldNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatal(`unexpected panic`, r)
		}
	}()
	Panic(FailureFlag{Name: `-`}, []Experiment{Experiment{
		Name:   `simplePanicString`,
		Effect: map[string]json.RawMessage{"exception": json.RawMessage([]byte("\"cp provided message\""))},
	}})
}
