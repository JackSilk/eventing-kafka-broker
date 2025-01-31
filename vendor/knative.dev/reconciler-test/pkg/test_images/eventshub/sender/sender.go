/*
Copyright 2020 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sender

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	nethttp "net/http"
	"strconv"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/cloudevents/sdk-go/v2/binding"
	cehttp "github.com/cloudevents/sdk-go/v2/protocol/http"
	"github.com/kelseyhightower/envconfig"
	"go.opencensus.io/plugin/ochttp"
	"k8s.io/apimachinery/pkg/util/wait"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/tracing/propagation/tracecontextb3"

	"knative.dev/reconciler-test/pkg/test_images/eventshub"
)

type generator struct {
	SenderName string `envconfig:"POD_NAME" default:"sender-default" required:"true"`

	// Sink url for the message destination
	Sink string `envconfig:"SINK" required:"true"`

	// The number of seconds to wait before starting sending the first message
	Delay int `envconfig:"DELAY" default:"5" required:"false"`

	// ProbeSink will probe the sink until it responds.
	ProbeSink bool `envconfig:"PROBE_SINK" default:"true"`

	// ProbeSinkTimeout defines the maximum amount of time in seconds to wait for the probe sink to succeed.
	ProbeSinkTimeout int `envconfig:"PROBE_SINK_TIMEOUT" required:"false" default:"60"`

	// InputEvent json encoded
	InputEvent string `envconfig:"INPUT_EVENT" required:"false"`

	// The encoding of the cloud event: [binary, structured].
	EventEncoding string `envconfig:"EVENT_ENCODING" default:"binary" required:"false"`

	// InputHeaders to send (this overrides any event provided input)
	InputHeaders map[string]string `envconfig:"INPUT_HEADERS" required:"false"`

	// InputBody to send (this overrides any event provided input)
	InputBody string `envconfig:"INPUT_BODY" required:"false"`

	// InputMethod to use when sending the http request
	InputMethod string `envconfig:"INPUT_METHOD" default:"POST" required:"false"`

	// Should tracing be added to events sent.
	AddTracing bool `envconfig:"ADD_TRACING" default:"false" required:"false"`

	// Should add extension 'sequence' identifying the sequence number.
	AddSequence bool `envconfig:"ADD_SEQUENCE" default:"false" required:"false"`

	// Override the event id with an incremental id.
	IncrementalId bool `envconfig:"INCREMENTAL_ID" default:"false" required:"false"`

	// Override the event time with the time when sending the event.
	OverrideTime bool `envconfig:"OVERRIDE_TIME" default:"false" required:"false"`

	// The number of seconds between messages.
	Period int `envconfig:"PERIOD" default:"5" required:"false"`

	// The number of messages to attempt to send. 0 for unlimited.
	MaxMessages int `envconfig:"MAX_MESSAGES" default:"1" required:"false"`

	// --- Processed State ---

	// baseEvent is parsed from InputEvent.
	baseEvent *cloudevents.Event

	// sequence is state counter for outbound events.
	sequence int
}

func Start(ctx context.Context, logs *eventshub.EventLogs) error {
	var env generator
	if err := envconfig.Process("", &env); err != nil {
		return fmt.Errorf("failed to process env var. %w", err)
	}
	if err := env.init(); err != nil {
		return err
	}

	logging.FromContext(ctx).Infof("Sender environment configuration: %+v", env)

	period := time.Duration(env.Period) * time.Second
	delay := time.Duration(env.Delay) * time.Second

	if delay > 0 {
		logging.FromContext(ctx).Info("will sleep for ", delay)
		time.Sleep(delay)
		logging.FromContext(ctx).Info("awake, continuing")
	}

	if env.ProbeSink {
		probingTimeout := time.Duration(env.ProbeSinkTimeout) * time.Second
		// Probe the sink for up to a minute.
		if err := wait.PollImmediate(100*time.Millisecond, probingTimeout, func() (bool, error) {
			req, err := nethttp.NewRequest(nethttp.MethodHead, env.Sink, nil)
			if err != nil {
				return false, err
			}

			if _, err := nethttp.DefaultClient.Do(req); err != nil {
				return false, nil
			}
			return true, nil
		}); err != nil {
			return fmt.Errorf("probing the sink '%s' using timeout %s failed: %w", env.Sink, probingTimeout, err)
		}
	}

	httpClient := &nethttp.Client{}
	if env.AddTracing {
		httpClient.Transport = &ochttp.Transport{
			Base:        nethttp.DefaultTransport,
			Propagation: tracecontextb3.TraceContextEgress,
		}
	}

	switch env.EventEncoding {
	case "binary":
		ctx = cloudevents.WithEncodingBinary(ctx)
	case "structured":
		ctx = cloudevents.WithEncodingStructured(ctx)
	default:
		return fmt.Errorf("unsupported encoding option: %q", env.EventEncoding)
	}

	ticker := time.NewTicker(period)
	for {

		req, event, err := env.next(ctx)
		if err != nil {
			return err
		}

		res, err := httpClient.Do(req)
		// Publish sent event info
		if err := logs.Vent(env.sentInfo(event, req, err)); err != nil {
			return fmt.Errorf("cannot forward event info: %w", err)
		}

		if err == nil {
			// Vent the response info
			if err := logs.Vent(env.responseInfo(res, event)); err != nil {
				return fmt.Errorf("cannot forward event info: %w", err)
			}
		}

		if !env.hasNext() {
			return nil
		}

		// Check if ctx is done before the next loop
		select {
		// Wait for next tick
		case <-ticker.C:
			// Keep looping.
		case <-ctx.Done():
			logging.FromContext(ctx).Infof("Canceled sending messages because context was closed")
			return nil
		}
	}
}

func (g *generator) sentInfo(event *cloudevents.Event, req *nethttp.Request, err error) eventshub.EventInfo {
	var eventId string
	if event != nil {
		eventId = event.ID()
	}

	if err != nil {
		return eventshub.EventInfo{
			Kind:     eventshub.EventSent,
			Error:    err.Error(),
			Origin:   g.SenderName,
			Observer: g.SenderName,
			Time:     time.Now(),
			Sequence: uint64(g.sequence),
			SentId:   eventId,
		}
	}

	sentEventInfo := eventshub.EventInfo{
		Kind:     eventshub.EventSent,
		Event:    event,
		Origin:   g.SenderName,
		Observer: g.SenderName,
		Time:     time.Now(),
		Sequence: uint64(g.sequence),
		SentId:   eventId,
	}

	sentHeaders := make(nethttp.Header)
	for k, v := range req.Header {
		sentHeaders[k] = v
	}
	sentEventInfo.HTTPHeaders = sentHeaders

	if g.InputBody != "" {
		sentEventInfo.Body = []byte(g.InputBody)
	}
	return sentEventInfo
}

func (g *generator) responseInfo(res *nethttp.Response, event *cloudevents.Event) eventshub.EventInfo {
	var eventId string
	if event != nil {
		eventId = event.ID()
	}

	responseInfo := eventshub.EventInfo{
		Kind:        eventshub.EventResponse,
		HTTPHeaders: res.Header,
		Origin:      g.Sink,
		Observer:    g.SenderName,
		Time:        time.Now(),
		Sequence:    uint64(g.sequence),
		StatusCode:  res.StatusCode,
		SentId:      eventId,
	}

	responseMessage := cehttp.NewMessageFromHttpResponse(res)

	if responseMessage.ReadEncoding() == binding.EncodingUnknown {
		body, err := ioutil.ReadAll(res.Body)

		if err != nil {
			responseInfo.Error = err.Error()
		} else {
			responseInfo.Body = body
		}
	} else {
		responseEvent, err := binding.ToEvent(context.Background(), responseMessage)
		if err != nil {
			responseInfo.Error = err.Error()
		} else {
			responseInfo.Event = responseEvent
		}
	}
	return responseInfo
}

func (g *generator) init() error {
	if g.InputEvent != "" {
		if err := json.Unmarshal([]byte(g.InputEvent), &g.baseEvent); err != nil {
			return fmt.Errorf("unable to unmarshal the event from json: %w", err)
		}
	}

	if g.InputEvent == "" && g.InputBody == "" && len(g.InputHeaders) == 0 {
		return fmt.Errorf("input values not provided")
	}

	return nil
}

func (g *generator) hasNext() bool {
	if g.MaxMessages == 0 {
		return true
	}
	return g.sequence < g.MaxMessages
}

func (g *generator) next(ctx context.Context) (*nethttp.Request, *cloudevents.Event, error) {
	req, err := nethttp.NewRequest(g.InputMethod, g.Sink, nil)
	if err != nil {
		logging.FromContext(ctx).Error("Cannot create the request: ", err)
		return nil, nil, err
	}

	var event *cloudevents.Event
	if g.baseEvent != nil {
		e := g.baseEvent.Clone()
		event = &e

		g.sequence++
		if g.AddSequence {
			event.SetExtension("sequence", g.sequence)
		}
		if g.IncrementalId {
			event.SetID(strconv.Itoa(g.sequence))
		}
		if g.OverrideTime {
			event.SetTime(time.Now())
		}

		logging.FromContext(ctx).Info("I'm going to send\n", event)

		err := cehttp.WriteRequest(ctx, binding.ToMessage(event), req)
		if err != nil {
			logging.FromContext(ctx).Error("Cannot write the event: ", err)
			return nil, nil, err
		}
	}

	if len(g.InputHeaders) != 0 {
		for k, v := range g.InputHeaders {
			req.Header.Add(k, v)
		}
	}

	if g.InputBody != "" {
		req.Body = ioutil.NopCloser(bytes.NewReader([]byte(g.InputBody)))
	}

	return req, event, nil
}
