// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Tobias Schottdorf (tobias.schottdorf@gmail.com)

package tracing

import (
	"bytes"
	"encoding/gob"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/util/caller"
	"github.com/cockroachdb/cockroach/util/envutil"
	"github.com/lightstep/lightstep-tracer-go"
	basictracer "github.com/opentracing/basictracer-go"
	"github.com/opentracing/basictracer-go/events"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
)

// Snowball is set as Baggage on traces which are used for snowball tracing.
const Snowball = "sb"

// A CallbackRecorder immediately invokes itself on received trace spans.
type CallbackRecorder func(sp basictracer.RawSpan)

// RecordSpan implements basictracer.SpanRecorder.
func (cr CallbackRecorder) RecordSpan(sp basictracer.RawSpan) {
	cr(sp)
}

// JoinOrNew creates a new Span joined to the provided DelegatingCarrier or
// creates Span from the given tracer.
func JoinOrNew(tr opentracing.Tracer, carrier *Span, opName string) (opentracing.Span, error) {
	if carrier != nil {
		wireContext, err := tr.Extract(basictracer.Delegator, carrier)
		switch err {
		case nil:
			sp := tr.StartSpan(opName, opentracing.FollowsFrom(wireContext))

			// Copy baggage items to tags so they show up in the Lightstep UI.
			sp.Context().ForeachBaggageItem(func(k, v string) bool { sp.SetTag(k, v); return true })

			sp.LogEvent(opName)
			return sp, nil
		case opentracing.ErrSpanContextNotFound:
		default:
			return nil, err
		}
	}
	return tr.StartSpan(opName), nil
}

// JoinOrNewSnowball returns a Span which records directly via the specified
// callback. If the given DelegatingCarrier is nil, a new Span is created.
// otherwise, the created Span is a child.
// TODO(andrei): JoinOrNewSnowball creates a new tracer, which is not kosher.
// Also this can't use the lightstep tracer.
func JoinOrNewSnowball(opName string, carrier *Span, callback func(sp basictracer.RawSpan)) (opentracing.Span, error) {
	tr := basictracer.NewWithOptions(defaultOptions(callback))
	sp, err := JoinOrNew(tr, carrier, opName)
	if err == nil {
		// We definitely want to sample a Snowball trace.
		// This must be set *before* SetBaggageItem, as that will otherwise be ignored.
		ext.SamplingPriority.Set(sp, 1)
		sp.SetBaggageItem(Snowball, "1")
	}
	return sp, err
}

// NewTracerAndSpanFor7881 creates a new tracer and a root span. The tracer is
// to be used for tracking down #7881; it runs a callback for each finished span
// (and the callback used accumulates the spans in a SQL txn).
func NewTracerAndSpanFor7881(
	callback func(sp basictracer.RawSpan),
) (opentracing.Span, opentracing.Tracer, error) {
	opts := defaultOptions(callback)
	// Don't trim the logs in "unsampled" spans". Note that this tracer does not
	// use sampling; instead it uses an ad-hoc mechanism for marking spans of
	// interest.
	opts.TrimUnsampledSpans = false
	tr := basictracer.NewWithOptions(opts)
	sp, err := JoinOrNew(tr, nil, "sql txn")
	return sp, tr, err
}

// ForkCtxSpan checks if ctx has a Span open; if it does, it creates a new Span
// that follows from the original Span. This allows the resulting context to be
// used in an async task that might outlive the original operation.
//
// Returns the new context and a function that closes the span.
func ForkCtxSpan(ctx context.Context, opName string) (context.Context, func()) {
	if span := opentracing.SpanFromContext(ctx); span != nil {
		if span.BaggageItem(Snowball) == "1" {
			// If we are doing snowball tracing, the span might outlive the snowball
			// tracer (calling the record function when it is no longer legal to do
			// so). Return a context with no span in this case.
			return opentracing.ContextWithSpan(ctx, nil), func() {}
		}
		tr := span.Tracer()
		newSpan := tr.StartSpan(opName, opentracing.FollowsFrom(span.Context()))
		return opentracing.ContextWithSpan(ctx, newSpan), func() { newSpan.Finish() }
	}
	return ctx, func() {}
}

func defaultOptions(recorder func(basictracer.RawSpan)) basictracer.Options {
	opts := basictracer.DefaultOptions()
	opts.ShouldSample = func(traceID uint64) bool { return false }
	opts.TrimUnsampledSpans = true
	opts.Recorder = CallbackRecorder(recorder)
	opts.NewSpanEventListener = events.NetTraceIntegrator
	opts.DebugAssertUseAfterFinish = true // provoke crash on use-after-Finish
	return opts
}

var lightstepToken = envutil.EnvOrDefaultString("COCKROACH_LIGHTSTEP_TOKEN", "")

// By default, if a lightstep token is specified we trace to both Lightstep and
// net/trace. If this flag is enabled, we will only trace to Lightstep.
var lightstepOnly = envutil.EnvOrDefaultBool("COCKROACH_LIGHTSTEP_ONLY", false)

// newTracer implements NewTracer and allows that function to be mocked out via Disable().
var newTracer = func() opentracing.Tracer {
	if lightstepToken != "" {
		lsTr := lightstep.NewTracer(lightstep.Options{AccessToken: lightstepToken})
		if lightstepOnly {
			return lsTr
		}
		basicTr := basictracer.NewWithOptions(defaultOptions(func(_ basictracer.RawSpan) {}))
		// The TeeTracer uses the first tracer for serialization of span contexts;
		// lightspan needs to be first because it correlates spans between nodes.
		return NewTeeTracer(lsTr, basicTr)
	}
	return basictracer.NewWithOptions(defaultOptions(func(_ basictracer.RawSpan) {}))
}

// NewTracer creates a Tracer which records to the net/trace
// endpoint.
func NewTracer() opentracing.Tracer {
	return newTracer()
}

// EnsureContext checks whether the given context.Context contains a Span. If
// not, it creates one using the provided Tracer and wraps it in the returned
// Span. The returned closure must be called after the request has been fully
// processed.
func EnsureContext(ctx context.Context, tracer opentracing.Tracer) (context.Context, func()) {
	_, _, funcName := caller.Lookup(1)
	if opentracing.SpanFromContext(ctx) == nil {
		sp := tracer.StartSpan(funcName)
		return opentracing.ContextWithSpan(ctx, sp), sp.Finish
	}
	return ctx, func() {}
}

// Disable is for benchmarking use and causes all future tracers to deal in
// no-ops. Calling the returned closure undoes this effect. There is no
// synchronization, so no moving parts are allowed while Disable and the
// closure are called.
func Disable() func() {
	orig := newTracer
	newTracer = func() opentracing.Tracer { return opentracing.NoopTracer{} }
	return func() {
		newTracer = orig
	}
}

// EncodeRawSpan encodes a raw span into bytes, using the given dest slice
// as a buffer.
func EncodeRawSpan(rawSpan *basictracer.RawSpan, dest []byte) ([]byte, error) {
	// This is not a greatly efficient (but convenient) use of gob.
	buf := bytes.NewBuffer(dest[:0])
	err := gob.NewEncoder(buf).Encode(rawSpan)
	return buf.Bytes(), err
}

// DecodeRawSpan unmarshals into the given RawSpan.
func DecodeRawSpan(enc []byte, dest *basictracer.RawSpan) error {
	return gob.NewDecoder(bytes.NewBuffer(enc)).Decode(dest)
}

// contextTracerKeyType is an empty type for the handle associated with the
// tracer value (see context.Value).
type contextTracerKeyType struct{}

// WithTracer returns a context derived from the given context, for which
// TracerFromCtx returns the given tracer.
func WithTracer(ctx context.Context, tracer opentracing.Tracer) context.Context {
	return context.WithValue(ctx, contextTracerKeyType{}, tracer)
}

// TracerFromCtx returns the tracer set on the context (or a parent context) via
// WithTracer.
func TracerFromCtx(ctx context.Context) opentracing.Tracer {
	if tracerVal := ctx.Value(contextTracerKeyType{}); tracerVal != nil {
		return tracerVal.(opentracing.Tracer)
	}
	return nil
}
