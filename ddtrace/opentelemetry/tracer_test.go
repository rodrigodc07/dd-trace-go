// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

package opentelemetry

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/internal"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

func TestGetTracer(t *testing.T) {
	assert := assert.New(t)
	tp := NewTracerProvider()
	tr := tp.Tracer("ot")
	dd, ok := internal.GetGlobalTracer().(ddtrace.Tracer)
	assert.True(ok)
	ott, ok := tr.(*oteltracer)
	assert.True(ok)
	assert.Equal(ott.Tracer, dd)
}

func TestSpanWithContext(t *testing.T) {
	assert := assert.New(t)
	tp := NewTracerProvider()
	otel.SetTracerProvider(tp)
	tr := otel.Tracer("ot", oteltrace.WithInstrumentationVersion("0.1"))
	ctx, sp := tr.Start(context.Background(), "otel.test")
	got, ok := tracer.SpanFromContext(ctx)

	assert.True(ok)
	assert.Equal(got, sp.(*span).Span)
	assert.Equal(fmt.Sprintf("%016x", got.Context().SpanID()), sp.SpanContext().SpanID().String())
}

func TestSpanWithNewRoot(t *testing.T) {
	assert := assert.New(t)
	otel.SetTracerProvider(NewTracerProvider())
	tr := otel.Tracer("")

	noopParent, ddCtx := tracer.StartSpanFromContext(context.Background(), "otel.child")

	otelCtx, child := tr.Start(ddCtx, "otel.child", oteltrace.WithNewRoot())
	got, ok := tracer.SpanFromContext(otelCtx)
	assert.True(ok)
	assert.Equal(got, child.(*span).Span)

	var parentBytes oteltrace.TraceID
	uint64ToByte(noopParent.Context().TraceID(), parentBytes[:])
	assert.NotEqual(parentBytes, child.SpanContext().TraceID())
}

func TestSpanWithoutNewRoot(t *testing.T) {
	assert := assert.New(t)
	otel.SetTracerProvider(NewTracerProvider())
	tr := otel.Tracer("")

	parent, ddCtx := tracer.StartSpanFromContext(context.Background(), "otel.child")
	_, child := tr.Start(ddCtx, "otel.child")
	var parentBytes oteltrace.TraceID
	// TraceID is big-endian so the LOW order bits are at the END of parentBytes
	uint64ToByte(parent.Context().TraceID(), parentBytes[8:])
	assert.Equal(parentBytes, child.SpanContext().TraceID())
}

func TestTracerOptions(t *testing.T) {
	assert := assert.New(t)
	otel.SetTracerProvider(NewTracerProvider(tracer.WithEnv("wrapper_env")))
	tr := otel.Tracer("ot")
	ctx, sp := tr.Start(context.Background(), "otel.test")
	got, ok := tracer.SpanFromContext(ctx)
	assert.True(ok)
	assert.Equal(got, sp.(*span).Span)
	assert.Contains(fmt.Sprint(sp), "dd.env=wrapper_env")
}

func TestSpanContext(t *testing.T) {
	assert := assert.New(t)
	tp := NewTracerProvider()
	defer tp.Shutdown()
	otel.SetTracerProvider(tp)
	tr := otel.Tracer("")

	ctx, err := tracer.Extract(tracer.TextMapCarrier{
		"traceparent": "00-000000000000000000000000075bcd15-1234567890123456-01",
		"tracestate":  "dd=s:2;o:rum;t.usr.id:baz64~~",
	})
	if err != nil {
		t.Fatalf("couldn't propagate headers")
	}
	_, s := tr.Start(ContextWithStartOptions(context.Background(), tracer.ChildOf(ctx), tracer.WithSpanID(16)), "parent")
	sctx := s.SpanContext()

	assert.Equal(oteltrace.FlagsSampled, sctx.TraceFlags())
	assert.Equal("000000000000000000000000075bcd15", sctx.TraceID().String())
	assert.Equal("0000000000000010", sctx.SpanID().String())
	assert.Equal("dd=s:2;o:rum;t.usr.id:baz64~~", sctx.TraceState().String())
	assert.Equal(true, sctx.IsRemote())
}

func TestForceFlush(t *testing.T) {
	assert := assert.New(t)
	const (
		UNSET = iota
		ERROR
		OK
	)
	testData := []struct {
		timeOut   time.Duration
		flushed   bool
		flushFunc func()
	}{
		{timeOut: 30 * time.Second, flushed: true, flushFunc: tracer.Flush},
		{timeOut: 0 * time.Second, flushed: false, flushFunc: func() {
			time.Sleep(300 * time.Second)
		}},
	}
	for _, tc := range testData {
		t.Run(fmt.Sprintf("Flush success: %t", tc.flushed), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			tp, payloads, cleanup := mockTracerProvider(t)
			defer cleanup()

			flushStatus := UNSET
			setFlushStatus := func(ok bool) {
				if ok {
					flushStatus = OK
				} else {
					flushStatus = ERROR
				}
			}
			tr := otel.Tracer("")
			_, sp := tr.Start(context.Background(), "test_span")
			sp.End()
			tp.forceFlush(tc.timeOut, setFlushStatus, tc.flushFunc)
			payload, err := waitForPayload(ctx, payloads)
			if tc.flushed {
				assert.NoError(err)
				assert.Contains(payload, "test_span")
				assert.Equal(OK, flushStatus)
			} else {
				assert.Equal(ERROR, flushStatus)
			}
		})
	}

	t.Run("Flush after shutdown", func(t *testing.T) {
		tp := NewTracerProvider()
		otel.SetTracerProvider(tp)
		testLog := new(log.RecordLogger)
		defer log.UseLogger(testLog)()

		tp.stopped = 1
		tp.ForceFlush(time.Second, func(ok bool) {})

		logs := testLog.Logs()
		assert.Contains(logs[len(logs)-1], "Cannot perform (*TracerProvider).Flush since the tracer is already stopped")
	})
}

func TestShutdownOnce(t *testing.T) {
	assert := assert.New(t)
	tp := NewTracerProvider()
	otel.SetTracerProvider(tp)
	tp.Shutdown()
	// attempt to get the Tracer after shutdown and
	// start a span. The context and span returned
	// from calling Start should be nil.
	tr := otel.Tracer("")
	ctx, sp := tr.Start(context.Background(), "after_shutdown")
	assert.Equal(uint32(1), tp.stopped)
	assert.Equal(sp, nil)
	assert.Equal(ctx, nil)
}

func BenchmarkApiWithNoTags(b *testing.B) {
	testData := struct {
		env, srv, op string
	}{"test_env", "test_srv", "op_name"}

	tp := NewTracerProvider(tracer.WithEnv(testData.env), tracer.WithService(testData.srv))
	defer tp.Shutdown()
	otel.SetTracerProvider(tp)
	tr := otel.Tracer("")

	b.ResetTimer()
	b.Run("otel_api", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, sp := tr.Start(context.Background(), testData.op)
			sp.End()
		}
	})

	tracer.Start(tracer.WithEnv(testData.env), tracer.WithService(testData.srv))
	defer tracer.Stop()
	b.ResetTimer()
	b.Run("datadog_otel_api", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			sp, _ := tracer.StartSpanFromContext(context.Background(), testData.op)
			sp.Finish()
		}
	})
}
func BenchmarkApiWithCustomTags(b *testing.B) {
	testData := struct {
		env, srv, oldOp, newOp, tagKey, tagValue string
	}{"test_env", "test_srv", "old_op", "new_op", "tag_1", "tag_1_val"}

	tp := NewTracerProvider(tracer.WithEnv(testData.env), tracer.WithService(testData.srv))
	defer tp.Shutdown()
	otel.SetTracerProvider(tp)
	tr := otel.Tracer("")

	b.ResetTimer()
	b.Run("otel_api", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, sp := tr.Start(context.Background(), testData.oldOp)
			sp.SetAttributes(attribute.String(testData.tagKey, testData.tagValue))
			sp.SetName(testData.newOp)
			sp.End()
		}
	})

	tracer.Start(tracer.WithEnv(testData.env), tracer.WithService(testData.srv))
	defer tracer.Stop()
	b.ResetTimer()
	b.Run("datadog_otel_api", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			sp, _ := tracer.StartSpanFromContext(context.Background(), testData.oldOp)
			sp.SetTag(testData.tagKey, testData.tagValue)
			sp.SetOperationName(testData.newOp)
			sp.Finish()
		}
	})
}

func BenchmarkOtelConcurrentTracing(b *testing.B) {
	tp := NewTracerProvider()
	defer tp.Shutdown()
	otel.SetTracerProvider(NewTracerProvider())
	tr := otel.Tracer("")

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		wg := sync.WaitGroup{}
		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx := context.Background()
				newCtx, parent := tr.Start(ctx, "parent")
				parent.SetAttributes(attribute.String("ServiceName", "pylons"),
					attribute.String("ResourceName", "/"))
				defer parent.End()

				for i := 0; i < 10; i++ {
					_, child := tr.Start(newCtx, "child")
					child.End()
				}
			}()
		}
	}
}
