# failure-flags-go

Failure Flags is a Go SDK for building application-level chaos experiments and reliability tests using the Gremlin Fault Injection platform. This library works in concert with Gremlin-Lambda, a Lambda Extension; or Gremlin-Sidecar, a container sidecar agent. This architecture minimizes the impact to your application code, simplifies configuration, and makes adoption painless.

Just like feature flags, Failure Flags are safe to add to and leave in your application. Failure Flags will always fail safe if it cannot communicate with its sidecar or its sidecar is misconfigured.

Take three steps to run an application-level experiment with Failure Flags:

1. Instrument your code with this SDK
2. Configure and deploy your code along side one of the Failure Flag sidecars
3. Run an Experiment with the console, API, or command line

## Instrumenting Your Code

You can get started by adding github.com/gremlin/failure-flags-go to your package dependencies:

```sh
go get github.com/gremlin/failure-flags-go
```

Then instrument the part of your application where you want to inject faults. 

```go
import (
    failureflags "github.com/gremlin/failure-flags-go"
)

...

// invoke any behaviors associated with active experiments from Gremlin
failureflags.Invoke(FailureFlag{
    Name: `flagname`, // the name of your failure flag
    Labels: nil,      // additional metadata experiments can use for targeting
} 

...
```

The best spots to add a failure flag are just before or just after a call to one of your network dependencies like a database or other network service. Or you can instrument your request handler and affect the way your application responses to its callers. Here's a simple Lambda example:

```go main.go
package main

import (
	"fmt"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"

    gremlin "github.com/gremlin/failure-flags-go"
)

func handler(request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	start := time.Now()

	gremlin.Invoke(gremlin.FailureFlag{
	    Name: `http-ingress`,
	    Labels: map[string]string{
			`method`: request.HTTPMethod,
			`path`: request.Path,
		}})

	return events.APIGatewayProxyResponse{
		Body:       fmt.Sprintf(`{"processingTime": %v, "timestamp": "%v"}`, time.Since(start), start),
		StatusCode: 200,
	}, nil
}

func main() {
	lambda.Start(handler)
}
```

You can always bring your own behaviors and effects by providing a behavior function. Here's another Lambda example that writes the experiment data to the console instead of changing the application behavior:

```go main.go
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"

    gremlin "github.com/gremlin/failure-flags-go"
)

func handler(request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	start := time.Now()

	gremlin.Invoke(gremlin.FailureFlag{
	    Name: `http-ingress`,
	    Labels: map[string]string{
			`method`: request.HTTPMethod,
			`path`: request.Path,
		},
		// the following line provides an implementation of the failureflags.Behavior type
		Behavior: func(ff FailureFlag, exps []Experiment) (impacted bool, err error) {
			// write the experiments to standard out
			fmt.Fprintf(os.Stdout, `processing experiments: %v`, exps)
			// continue processing using the default behavior chain
			return failureFlags.DelayedPanicOrError(ff, exps)
		}})

	return events.APIGatewayProxyResponse{
		Body:       fmt.Sprintf(`{"processingTime": %v, "timestamp": "%v"}`, time.Since(start), start),
		StatusCode: 200,
	}, nil
}

func main() {
	lambda.Start(handler)
}
```

### Doing Something Different

Sometimes you need even more manual control. For example, in the event of an experiment you might not want to make some API call or need to rollback some transaction. In most cases the Exception effect can help, but the `Invoke` function also returns a boolean to indicate if there was an experiment. You can use that to create branches in your code like you would for any feature flag.

```js
...
if active, impacted, err := FailureFlag{Name: `branching-flag`}.Invoke(); active && impacted {
  // if there is a running experiment then do this
} else {
  // if there is no experiment, or it had no impact then do this
}
...
```

### Pulling the Experiment and Branching Manually

If you want to work with lower-level Experiment data you can use `FetchExperiment(FailureFlag)` directly.

## Targeting with Selectors

Experiments match specific invocations of a Failure Flag based on its name, and the labels you provide. Experiments define Selectors that the Failure Flags engine uses to determine if an invocation matches. Selectors are simple key to list of values maps. The basic matching logic is every key in a selector must be present in the Failure Flag labels, and at least one of the values in the list for a selector key must match the value in the label.

## Effects and Examples

Once you've instrumented your code and deployed your application with the sidecar you're ready to run an Experiment. None of the work you've done so far describes the Effect during an experiment. You've only marked the spots in code where you want the opportunity to experiment. Gremlin Failure Flags Experiments take an Effect parameter. The Effect parameter is a simple JSON map. That map is provided to the Failure Flags SDK if the application is targeted by a running Experiment. The Failure Flags SDK will process the map according to the default behavior chain or the behaviors you've provided. Today the default chain provides both latency and error Effects.

### Introduce Flat Latency

This Effect will introduce a constant 2000 millisecond delay.

```json
{ "latency": 2000 }
```

### Introduce Minimum Latency with Some Maximum Jitter

This Effect will introduce between 2000 and 2200 milliseconds of latency where there is a pseudo-random uniform probability of any delay between 2000 and 2200.

```json
{
  "latency": {
    "ms": 2000,
    "jitter": 200
  }
}
```

### Throw an Error

This Effect will cause Failure Flags to throw an Error with the provided message. This is useful if your application uses Errors with well-known messages.

```json
{ "exception": "this is a custom message" }
```

### Combining the Two for a "Delayed Exception"

Many common failure modes eventually result in an exception being thrown, but there will be some delay before that happens. Examples include network connection failures, or degradation, or other timeout-based issues.

This Effect Statement will cause a Failure Flag to pause for a full 2 seconds before throwing an exception/error a message, "Custom TCP Timeout Simulation"

```json
{
  "latency": 2000,
  "exception": "Custom TCP Timeout Simulation"
}
```

<!--
### Advanced: Changing Application Data

Suppose you want to be able to experiment with mangled data. This might include responses from your application's dependencies. You can do that with a little extra work. You need to provide prototype data in your call to `ifExperimentActive`.

```js
...
let myData = {name: 'HTTPResponse'}; // this is just example data, it could be anything

myData = await failureflags.ifExperimentActive({
  name: 'flagname',       // the name of your failure flag
  labels: {},             // additional attibutes about this invocation
  dataPrototype: myData); // "myData" is some variable like a request or response
                          // You could also pass in an object literal.
...
```

If the `dataPrototype` property is set then you can use the `data` property in the Effect statement:

```json
{
  "data": {
    "statusCode": 404,
    "statusMessage": "Not Found"
  }
}
```

Any properties in the `data` object in this map will be copied into a new object created from the prototype you provided. In this example, if the experiment is not running then `myData` will be returned unaltered, and if it is running it would have been altered to the following: 

```json
{
  "name": "HTTPResponse",
  "statusCode": 404,
  "statusMessage": "Not Found"
}
```

This is exciting because it means that you do not need to mock out whole responses before you know what experiments you'll need to run. 
-->

### Advanced: Providing Metadata to Custom Effects

The default effect chain included with the Failure Flags SDK is aware of well-known effect properties including, "latency" and "exception." The user can extend or replace that functionality and use the same properties, or provide their own. For example, suppose a user wants to use a "random jitter" effect that the Standard Chain does not provide. Suppose they wanted to inject a random amount of jitter up to some maximum. They could implement that small extension and make up their own Effect property called, "my-jitter" that specifies that maximum. The resulting Effect Statement would look like:

```json
{ "my-jitter": 500 }
```

They might also combine this with parts of the default chain:

```json
{
  "latency": 1000,
  "my-jitter": 500
}
```
