//nolint:unused
package golang

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"time"
	"unicode"
)

// AfterProxy is defined here and used in this package to allow for greater testability
type AfterProxy func(time.Duration) <-chan time.Time
type Requester func(chan []Experiment, *http.Request, Logf)

// exported variables
var (
	Version           = `v0.0.1`
	VersionIdentifier = `go-` + Version
	LookupTimeout     = 2 * time.Millisecond
	LookupBackoff     = 5 * time.Minute
)

const (
	sdkLabelKey   = `failure-flags-sdk-version`
	envEnabledKey = `FAILURE_FLAGS_ENABLED`
)

func isEnabled() bool {
	if v, ok := os.LookupEnv(envEnabledKey); ok && (v == `1` || v == `true` || v == `True` || v == `TRUE`) {
		return true
	}
	return false
}

// Behavior functions
type Behavior func(ff FailureFlag, experiments []Experiment) (impacted bool, err error)
type Logf func(format string, a ...any) (n int, err error)

// internal package state
var (
	// opportunity to inject spies, etc. during testing
	after   = time.After
	request = doRequest

	defaultTransport http.RoundTripper = &http.Transport{
		MaxIdleConns:          2, // keep resource overhead as low as possible
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 2 * time.Millisecond, // keep short timeouts
		DisableCompression:    true,                 // no compression is used
		Proxy:                 nil,                  // explicitly ignore proxy configuration in environment
	}

	backoffUntil time.Time
)

func DefaultLogf(format string, p ...any) (n int, err error) {
	return fmt.Fprintf(os.Stderr, fmt.Sprintf("[failure-flags-go] %v %v\n", time.Now(), format), p...)
}

type Experiment struct {
	// ID property is set by the control plane when the experiment is created. That
	// is used to centrally track experiment impact.
	ID string `json:"id"`

	// The experiment name as specified by the user in the Gremlin control plane.
	Name string `json:"name"`

	Protected bool `json:"protected"`

	FailureFlag Selector `json:"flag"`

	StartTime string `json:"startTime"`

	// Rate is a probability that the effect will be applied.
	Rate float64 `json:"rate"`

	// Effect is a config map specifying parameters for the test effect. The keys
	// and values carry no meaning in the experimentation platform. Interpretation is
	// an exercise left to the effect function.
	Effect map[string]json.RawMessage `json:"effect"`
}

type Selector struct {
	// Name property identifies the codepoint being labeled. Use this property to
	// quickly identify HTTP ingress, HTTP egress, or dependencies.
	Name string `json:"name"`

	// Labels is a map of qualifying attributes and values. An experiment should apply
	// if and only if all selectors match a code point.
	Labels map[string][]string
}

type FailureFlag struct {
	// Used by the interaces to the SDK as well as provided via control plane
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`

	// Used by the interfaces to the SDK
	Behavior      Behavior    `json:"-"`
	DataReference interface{} `json:"-"`

	// For debugging the SDK
	Logf Logf `json:"-"`
}

func Invoke(ff FailureFlag) (active bool, impacted bool, err error) {
	return ff.Invoke()
}

// Invoke fetches any experiments targeting this FailureFlag and executes the
// configured behavior chain. This function will propagate panics as the
// behaviors may be designed to induce panics.
func (ff FailureFlag) Invoke() (active bool, impacted bool, err error) {
	if !isEnabled() {
		if ff.Logf != nil {
			ff.Logf(`FAILURE_FLAGS_ENABLED is unset, or false`) //nolint
		}
		return
	}
	if ff.Logf != nil {
		ff.Logf(`Invoke, %s, %v, %v`, ff.Name, ff.Labels, ff.DataReference != nil) //nolint
	}

	if len(ff.Name) <= 0 {
		if ff.Logf != nil {
			ff.Logf(`FailureFlag names must be non-empty`) //nolint
		}
		active = false
		impacted = false
		err = nil
		return
	}

	var behavior = ff.Behavior
	if ff.Behavior == nil {
		behavior = DelayedPanicOrError
	}

	// fetch the experiments
	experiments, ferr := FetchExperiment(ff)
	if ferr != nil || len(experiments) <= 0 {
		active = false
		impacted = false
		err = nil
		return
	}

	// if there is at least one experiments
	active = true
	impacted, err = behavior(ff, experiments)
	return
}

// FetchExperiment makes a synchronous call to the sidecar service to retrieve any
// experiments targeting the provided FailureFlag. This function should never be
// allowed to propagate panics to the caller.
func FetchExperiment(ff FailureFlag) (result []Experiment, rerr error) {
	if !isEnabled() {
		if ff.Logf != nil {
			ff.Logf(`FAILURE_FLAGS_ENABLED is unset, or false`) //nolint
		}
		return
	}
	defer func() {
		if r := recover(); r != nil {
			// swallow the panic and return cleanly
			if err, ok := r.(error); ok {
				rerr = err
				return
			}
			rerr = fmt.Errorf(`panic: %v`, r)
			return
		}
	}()
	// TODO abort if FAILURE_FLAGS_ENABLED is not set

	// decorate the labels with the SDK version
	if ff.Labels == nil {
		ff.Labels = map[string]string{}
	}
	ff.Labels[sdkLabelKey] = VersionIdentifier

	if ff.Logf != nil {
		ff.Logf(`FetchExperiment invoked`) //nolint
	}
	if !backoffUntil.IsZero() && backoffUntil.After(time.Now()) {
		if ff.Logf != nil {
			ff.Logf(`skipping until %v due to cooldown`, backoffUntil) //nolint
		}
	}
	body, err := json.Marshal(&ff)
	if err != nil {
		// errors here indicate an issue in the SDK itself and the FailureFlag structure
		if ff.Logf != nil {
			ff.Logf(`unable to marshal FailureFlag and retrieve experiments, %v`, err) //nolint
		}
		return nil, err
	}
	req, err := http.NewRequest(`POST`, `http://localhost:5032/experiment`, bytes.NewBuffer(body))
	if err != nil {
		// again, errors here indicate an issue in the SDK itself specifically the method name
		if ff.Logf != nil {
			ff.Logf(`unable to construct a request to retrieve experiments, %v`, err) //nolint
		}
		return nil, err
	}
	req.Header.Add("Content-Type", `application/json`)

	responseChan := make(chan []Experiment)
	go request(responseChan, req, ff.Logf)

	// get the experiments or timeout
	select {
	case experiments, ok := <-responseChan:
		if !ok {
			return nil, fmt.Errorf(`unable to retrieve experiments`)
		}
		return experiments, nil
	case <-after(LookupTimeout):
		if ff.Logf != nil {
			ff.Logf(`unable to retrieve experiments in under %v`, LookupTimeout) //nolint
			ff.Logf(`backing off for %v`, LookupBackoff)                         //nolint
		}
		// There was a timeout waiting for the sidecar. In order not to impact
		// the caller we will just return, but someone needs to hang around and
		// wait for the Requester to write to or close the channel. We risk
		// either leaking channels or goroutines. So, the best course of action
		// is to leave behind one reader, and disable / backoff on further
		// calls to retrieve experiments.
		go func() { <-responseChan }()
		// TODO actually set backoffUntil
		// TODO setup mutex for backoffUntil
		// just leave experiments nil
		return nil, fmt.Errorf(`timeout while fetching experiments`)
	}
}

func doRequest(responseChan chan []Experiment, req *http.Request, logf Logf) {
	client := &http.Client{Transport: defaultTransport}
	response, err := client.Do(req)
	if err != nil {
		if logf != nil {
			logf(`unable to fetch experiments, %v`, err) //nolint
		}
		close(responseChan)
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		if logf != nil {
			logf(`unable to fetch experiments, code: %v`, response.StatusCode) //nolint
		}
		close(responseChan)
		return
	}
	if response.ContentLength == 0 {
		close(responseChan)
		return
	}

	if t, ok := response.Header["Content-Type"]; ok && len(t) == 1 && t[0] != `application/json` {
		if logf != nil {
			logf(`experiments returned in improper content-type, code: %v`, response.StatusCode) //nolint
		}
		close(responseChan)
		return
	}

	data, err := io.ReadAll(response.Body)
	if err != nil {
		if logf != nil {
			logf(`unable to read fetched experiment data, %v`, err) //nolint
		}
		close(responseChan)
		return
	}

	var rawExperiments json.RawMessage
	err = json.Unmarshal(data, &rawExperiments)
	if err != nil {
		if logf != nil {
			logf(`unable to unmarshal raw fetched experiment data, %v`, err) //nolint
		}
		close(responseChan)
		return
	}

	var experiments []Experiment
	if isFieldObject(rawExperiments) {
		var experiment Experiment
		err = json.Unmarshal(data, &experiment)
		if err != nil {
			if logf != nil {
				logf(`unable to unmarshal fetched experiment data, %v`, err) //nolint
			}
			close(responseChan)
			return
		}
		experiments = []Experiment{experiment}
	} else if isFieldArray(rawExperiments) {
		err = json.Unmarshal(data, &experiments)
		if err != nil {
			if logf != nil {
				logf(`unable to unmarshal fetched experiment data, %v`, err) //nolint
			}
			close(responseChan)
			return
		}
	} else {
		if logf != nil {
			logf(`unrecognized response shape in fetched experiment data`) //nolint
		}
		close(responseChan)
		return
	}

	responseChan <- experiments
	close(responseChan)
}

// JSON inspection functions for dynamic unmarshaling

// trimLeftSpace can be used to make sure that the left-most rune in a json.RawMessage will reliably hint at the record type.
func trimLeftSpace(s []byte) []byte {
	return bytes.TrimLeftFunc(s, unicode.IsSpace)
}

// isFieldBool inspects the first non-whitespace character in a json byte sequence to determine if that record might be a valid boolean.
func isFieldBool(f json.RawMessage) bool {
	if trimmed := trimLeftSpace([]byte(f)); trimmed != nil && (trimmed[0] == byte('t') ||
		trimmed[0] == byte('f')) {
		return true
	}
	return false
}

// isFieldString inspects the first non-whitespace character in a json byte sequence to determine if that record might be a valid string.
func isFieldString(f json.RawMessage) bool {
	if trimmed := trimLeftSpace([]byte(f)); trimmed != nil && trimmed[0] == byte('"') {
		return true
	}
	return false
}

// isFieldNumber inspects the first non-whitespace character in a json byte sequence to determine if that record might be a valid number.
func isFieldNumber(f json.RawMessage) bool {
	if trimmed := trimLeftSpace([]byte(f)); trimmed != nil && unicode.IsNumber(rune(trimmed[0])) {
		return true
	}
	return false
}

// isFieldObject inspects the first non-whitespace character in a json byte sequence to determine if that record might be a valid object.
func isFieldObject(f json.RawMessage) bool {
	if trimmed := trimLeftSpace([]byte(f)); trimmed != nil && trimmed[0] == byte('{') {
		return true
	}
	return false
}

// isFieldArray inspects the first non-whitespace character in a json byte sequence to determine if that record might be a valid array.
func isFieldArray(f json.RawMessage) bool {
	if trimmed := trimLeftSpace([]byte(f)); trimmed != nil && trimmed[0] == byte('[') {
		return true
	}
	return false
}

// Behaviors

const EffectLatency = `latency`
const EffectPanic = `panic`
const EffectException = `exception`
const EffectData = `data`

type LatencySpec struct {
	MS     int `json:"ms"`
	Jitter int `json:"jitter"`
}

func Latency(ff FailureFlag, experiments []Experiment) (impacted bool, rerr error) {
	defer func() {
		if r := recover(); r != nil {
			// swallow the panic and return cleanly
			impacted = false
			rerr = nil
			return
		}
	}()
	if ff.Logf != nil {
		ff.Logf(`latency processing %v experiments`, len(experiments)) //nolint
	}
	for _, e := range experiments {
		entry, ok := e.Effect[EffectLatency]
		if !ok {
			if ff.Logf != nil {
				ff.Logf(`experiment %v has no latency clause`, e.Name) //nolint
			}
			continue
		}
		var err error
		latency := 0 // in milliseconds
		if isFieldNumber(entry) {
			if ff.Logf != nil {
				ff.Logf(`experiment %v has number latency clause`, e.Name) //nolint
			}
			if err = json.Unmarshal(entry, &latency); err != nil {
				// not actually a number, this is a bug in the SDK
				if ff.Logf != nil {
					ff.Logf(`experiment %v did not really have a number, %v`, e.Name, err) //nolint
				}
				continue
			}
		} else if isFieldString(entry) {
			if ff.Logf != nil {
				ff.Logf(`experiment %v has string latency clause`, e.Name) //nolint
			}
			latencyString := ""
			if err = json.Unmarshal(entry, &latencyString); err != nil {
				// not actually a string, this is a bug in the SDK
				if ff.Logf != nil {
					ff.Logf(`experiment %v did not really have a string, %v`, e.Name, err) //nolint
				}
				continue
			}
			latency, err = strconv.Atoi(latencyString)
			if err != nil {
				// turns out it wasn't actually a parsable integer
				if ff.Logf != nil {
					ff.Logf(`experiment %v did not contain a parsable integer, %v`, e.Name, err) //nolint
				}
				continue
			}
		} else if isFieldObject(entry) {
			if ff.Logf != nil {
				ff.Logf(`experiment %v has object latency clause`, e.Name) //nolint
			}
			spec := LatencySpec{}
			if err := json.Unmarshal(entry, &spec); err != nil {
				// not the kind of object we can work with
				if ff.Logf != nil {
					ff.Logf(`experiment %v did not really have an object, %v`, e.Name, err) //nolint
				}
				continue
			}
			latency = spec.MS + int(rand.Float64()*float64(spec.Jitter))
		}
		if ff.Logf != nil {
			ff.Logf(`experiment %v specified %v milliseconds`, e.Name, latency) //nolint
		}
		if latency > 0 {
			impacted = true
			<-after(time.Duration(latency) * time.Millisecond)
		}
	}
	return
}

func Exception(ff FailureFlag, experiments []Experiment) (impacted bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			// swallow the panic and return cleanly
			impacted = false
			err = nil
			return
		}
	}()
	for _, e := range experiments {
		entry, ok := e.Effect[EffectException]
		if !ok {
			continue
		}

		if isFieldString(entry) {
			message := ""
			if err = json.Unmarshal(entry, &message); err != nil {
				// not actually a string, this is a bug in the SDK
				continue
			}
			if len(message) > 0 {
				impacted = true
				err = fmt.Errorf(message)
			}
		}
	}
	return
}

func Panic(ff FailureFlag, experiments []Experiment) (bool, error) {
	for _, e := range experiments {
		entry, ok := e.Effect[EffectPanic]
		if !ok {
			continue
		}
		if isFieldString(entry) {
			message := ""
			if err := json.Unmarshal(entry, &message); err != nil {
				// not actually a string, this is a bug in the SDK
				continue
			}
			if len(message) > 0 {
				panic(fmt.Errorf(message))
			}
		}
	}
	return false, nil
}

func Data(ff FailureFlag, experiments []Experiment) (impacted bool, err error) {
	// TODO weave all the stuff in
	return false, nil
}

func DelayedPanicOrError(ff FailureFlag, experiments []Experiment) (bool, error) {
	latencyImpact, _ := Latency(ff, experiments)
	panicImpact, _ := Panic(ff, experiments)
	exceptionImpact, err := Exception(ff, experiments)
	return latencyImpact || panicImpact || exceptionImpact, err
}

func DelayedDataOrError(ff FailureFlag, experiments []Experiment) (bool, error) {
	latencyImpact, _ := Latency(ff, experiments)
	_, err := Exception(ff, experiments)
	if err != nil {
		return true, err
	}
	dataImpact, _ := Data(ff, experiments)
	return latencyImpact || dataImpact, nil
}
