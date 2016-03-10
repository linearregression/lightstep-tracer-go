package lightstep

import (
	"flag"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/lightstep/lightstep-tracer-go/lightstep_thrift"
	"github.com/lightstep/lightstep-tracer-go/thrift_0_9_2/lib/go/thrift"
	"github.com/lightstep/lightstep-tracer-go/truncator"
	"github.com/opentracing/basictracer-go"
	ot "github.com/opentracing/opentracing-go"
)

const (
	collectorPath = "/_rpc/v1/reports/binary"

	defaultCollectorPlainPort  = 80
	defaultCollectorSecurePort = 443

	defaultCollectorHost = "collector.lightstep.com"

	// See the comment for shouldFlush() for more about these tuning
	// parameters.
	defaultMaxReportingPeriod = 2500 * time.Millisecond
	minReportingPeriod        = 500 * time.Millisecond

	// TraceGUIDKey is the tag key used to define traces which are
	// joined based on a GUID.
	TraceGUIDKey = "join:trace_guid"
	// ParentSpanGUIDKey is the tag key used to record the relationship
	// between child and parent spans.
	ParentSpanGUIDKey = "parent_span_guid"

	ComponentNameKey = "component_name"
	ComponentGUIDKey = "component_guid"
	HostnameKey      = "hostname"
	CmdlineKey       = "cmdline"

	ellipsis = "…"
)

// TODO move these to Options
var (
	flagMaxLogMessageLen     = flag.Int("lightstep_max_log_message_len_bytes", 1024, "the maximum number of bytes used by a single log message")
	flagMaxPayloadFieldBytes = flag.Int("lightstep_max_log_payload_field_bytes", 1024, "the maximum number of bytes exported in a single payload field")
	flagMaxPayloadTotalBytes = flag.Int("lightstep_max_log_payload_max_total_bytes", 4096, "the maximum number of bytes exported in an entire payload")
)

var sharedTrunactor *truncator.Truncator

func init() {
	sharedTrunactor = truncator.NewTruncator(*flagMaxPayloadFieldBytes, *flagMaxPayloadTotalBytes)
}

// Options control how the LightStep Tracer behaves.
type Options struct {
	// AccessToken is the unique API key for your LightStep project.  It is
	// available on your account page at https://app.lightstep.com/account
	AccessToken string

	// CollectorHost is the host to which spans sent.  If empty, the default
	// will be used.
	CollectorHost string
	// CollectorPort is the  describes the service to which span and log data will be
	// sent.  If zero, the default will be used.
	CollectorPort int
	// CollectorPlaintext indicates whether to use HTTP (true) or TLS/HTTPS (false, the
	// default) when sending spans to a collector.
	CollectorPlaintext bool

	// ComponentAttributes are
	ComponentAttributes map[string]interface{}

	// MaxBufferedSpans is the maximum number of spans that will be buffered
	// before sending them to a collector.
	MaxBufferedSpans int

	// ReportingPeriod is the maximum duration of time between sending spans
	// to a collector.  If zero, the default will be used.
	ReportingPeriod time.Duration

	// Set Verbose to true to enable more logging.
	Verbose bool
}

// NewTracer returns a new Tracer that reports spans to a LightStep
// collector.
func NewTracer(opts Options) ot.Tracer {
	options := basictracer.DefaultOptions()
	options.ShouldSample = func(_ int64) bool { return true }
	options.Recorder = NewRecorder(opts)
	return basictracer.NewWithOptions(options)
}

// Recorder buffers spans and forwards them to a LightStep collector.
type Recorder struct {
	lock sync.Mutex

	// auth and runtime information
	auth       *lightstep_thrift.Auth
	attributes map[string]string
	startTime  time.Time

	// Time window of the data to be included in the next report.
	reportOldest   time.Time
	reportYoungest time.Time

	// buffered data
	buffer   spansBuffer
	counters counterSet

	lastReportAttempt  time.Time
	maxReportingPeriod time.Duration
	reportInFlight     bool
	// Remote service that will receive reports
	backend lightstep_thrift.ReportingService

	verbose bool

	// We allow our remote peer to disable this instrumentation at any
	// time, turning all potentially costly runtime operations into
	// no-ops.
	disabled bool
}

func NewRecorder(opts Options) basictracer.SpanRecorder {
	if len(opts.AccessToken) == 0 {
		// TODO maybe return a no-op recorder instead?
		panic("LightStep Recorder options.AccessToken must not be empty")
	}
	if opts.ComponentAttributes == nil {
		opts.ComponentAttributes = make(map[string]interface{})
	}
	// Set some default attributes if not found in options
	if _, found := opts.ComponentAttributes[ComponentNameKey]; !found {
		opts.ComponentAttributes[ComponentNameKey] = path.Base(os.Args[0])
	}
	if _, found := opts.ComponentAttributes[ComponentGUIDKey]; !found {
		opts.ComponentAttributes[ComponentGUIDKey] = genSeededGUID()
	}
	if _, found := opts.ComponentAttributes[HostnameKey]; !found {
		hostname, _ := os.Hostname()
		opts.ComponentAttributes[HostnameKey] = hostname
	}
	if _, found := opts.ComponentAttributes[CmdlineKey]; !found {
		opts.ComponentAttributes[CmdlineKey] = strings.Join(os.Args, " ")
	}

	attributes := make(map[string]string)
	for k, v := range opts.ComponentAttributes {
		attributes[k] = fmt.Sprint(v)
	}
	attributes["lightstep_tracer_platform"] = "go"
	attributes["lightstep_tracer_version"] = "0.9.0"

	collectorHost := defaultCollectorHost
	if len(opts.CollectorHost) > 0 {
		collectorHost = opts.CollectorHost
	}
	httpProtocol := "https"
	collectorPort := defaultCollectorSecurePort
	if opts.CollectorPlaintext {
		httpProtocol = "http"
		collectorPort = defaultCollectorPlainPort
	}
	if opts.CollectorPort > 0 {
		collectorPort = opts.CollectorPort
	}

	now := time.Now()
	rec := &Recorder{
		auth: &lightstep_thrift.Auth{
			AccessToken: thrift.StringPtr(opts.AccessToken),
		},
		attributes:         attributes,
		startTime:          now,
		reportOldest:       now,
		reportYoungest:     now,
		maxReportingPeriod: defaultMaxReportingPeriod,
		verbose:            opts.Verbose,
	}
	rec.buffer.setDefaults()

	if opts.MaxBufferedSpans > 0 {
		rec.buffer.setMaxBufferSize(opts.MaxBufferedSpans)
	}

	transport, err := thrift.NewTHttpPostClient(
		fmt.Sprintf("%s://%s:%d%s", httpProtocol, collectorHost, collectorPort, collectorPath))
	if err != nil {
		rec.maybeLogError(err)
		return nil
	}
	rec.backend = lightstep_thrift.NewReportingServiceClientFactory(
		transport, thrift.NewTBinaryProtocolFactoryDefault())

	go rec.reportLoop()

	return rec
}

func (r *Recorder) RecordSpan(raw basictracer.RawSpan) {
	r.lock.Lock()
	defer r.lock.Unlock()

	// Early-out for disabled runtimes.
	if r.disabled {
		return
	}

	r.counters.droppedSpans += r.buffer.addSpans([]basictracer.RawSpan{raw})
}

func (r *Recorder) Flush() {
	r.lock.Lock()

	if r.disabled {
		r.lock.Unlock()
		return
	}

	if r.reportInFlight == true {
		r.maybeLogError(fmt.Errorf("A previous Report is still in flight; aborting Flush()."))
		r.lock.Unlock()
		return
	}

	now := time.Now()
	r.lastReportAttempt = now
	r.reportYoungest = now

	rawSpans := r.buffer.current()
	// Convert them to thrift.
	recs := make([]*lightstep_thrift.SpanRecord, len(rawSpans))
	// TODO: could pool lightstep_thrift.SpanRecords
	for i, raw := range rawSpans {
		var joinIds []*lightstep_thrift.TraceJoinId
		var attributes []*lightstep_thrift.KeyValue
		for key, value := range raw.Tags {
			if strings.HasPrefix("join:", key) {
				joinIds = append(joinIds, &lightstep_thrift.TraceJoinId{key, fmt.Sprint(value)})
			} else {
				attributes = append(attributes, &lightstep_thrift.KeyValue{key, fmt.Sprint(value)})
			}
		}
		logs := make([]*lightstep_thrift.LogRecord, len(raw.Logs))
		for j, log := range raw.Logs {
			event := ""
			if len(log.Event) > 0 {
				// Don't allow for arbitrarily long log messages.
				if len(log.Event) > *flagMaxLogMessageLen {
					event = log.Event[:(*flagMaxLogMessageLen-1)] + ellipsis
				} else {
					event = log.Event
				}
			}

			var thriftPayload *string
			if log.Payload != nil {
				// This converts values to strings to avoid lossy encoding, i.e.
				// not the same as a call to json.Marshal().  TruncateToJSON() is
				// thread-safe.
				jsonString, err := sharedTrunactor.TruncateToJSON(log.Payload)
				if err != nil {
					thriftPayload = thrift.StringPtr(fmt.Sprintf("Error encoding payload object: %v", err))
				} else {
					thriftPayload = &jsonString
				}
			}
			logs[j] = &lightstep_thrift.LogRecord{
				TimestampMicros: thrift.Int64Ptr(log.Timestamp.UnixNano() / 1000),
				StableName:      thrift.StringPtr(event),
				PayloadJson:     thriftPayload,
			}
		}

		// TODO implement baggage

		joinIds = append(joinIds, &lightstep_thrift.TraceJoinId{TraceGUIDKey,
			fmt.Sprint(raw.TraceID)})
		if raw.ParentSpanID != 0 {
			attributes = append(attributes, &lightstep_thrift.KeyValue{ParentSpanGUIDKey,
				fmt.Sprint(raw.ParentSpanID)})
		}

		recs[i] = &lightstep_thrift.SpanRecord{
			SpanGuid:       thrift.StringPtr(fmt.Sprint(raw.SpanID)),
			SpanName:       thrift.StringPtr(raw.Operation),
			JoinIds:        joinIds,
			OldestMicros:   thrift.Int64Ptr(raw.Start.UnixNano() / 1000),
			YoungestMicros: thrift.Int64Ptr(raw.Start.Add(raw.Duration).UnixNano() / 1000),
			Attributes:     attributes,
			LogRecords:     logs,
		}
	}
	req := &lightstep_thrift.ReportRequest{
		OldestMicros:   thrift.Int64Ptr(r.reportOldest.UnixNano() / 1000),
		YoungestMicros: thrift.Int64Ptr(r.reportYoungest.UnixNano() / 1000),
		Runtime:        r.thriftRuntime(),
		SpanRecords:    recs,
		Counters:       r.counters.toThrift(),
	}

	// Do *not* wait until the report RPC finishes to clear the buffer.
	// Consider the case of a new span coming in during the RPC: it'll be
	// discarded along with the data that was just sent if the buffers are
	// cleared later.
	r.buffer.reset()

	r.reportInFlight = true
	r.lock.Unlock() // unlock before making the RPC itself

	resp, err := r.backend.Report(r.auth, req)
	if err != nil {
		r.maybeLogError(err)
	} else if len(resp.Errors) > 0 {
		// These should never occur, since this library should understand what
		// makes for valid logs and spans, but just in case, log it anyway.
		for _, err := range resp.Errors {
			r.maybeLogError(fmt.Errorf("Remote report returned error: %s", err))
		}
	} else {
		r.maybeLogInfof("Report: resp=%v, err=%v", resp, err)
	}

	r.lock.Lock()
	r.reportInFlight = false
	if err != nil {
		// Restore the records that did not get sent correctly
		r.counters.droppedSpans += r.buffer.addSpans(rawSpans)

		r.lock.Unlock()
		return
	}

	// Reset the buffers
	r.reportOldest = now
	r.reportYoungest = now
	// TODO: this ends up discarding counts coming in during the RPC
	r.counters = counterSet{}

	// TODO something about timing
	r.lock.Unlock()

	for _, c := range resp.Commands {
		if c.Disable != nil && *c.Disable {
			r.Disable()
		}
	}
}

// caller must hold r.lock
func (r *Recorder) thriftRuntime() *lightstep_thrift.Runtime {
	runtimeAttrs := []*lightstep_thrift.KeyValue{}
	for k, v := range r.attributes {
		runtimeAttrs = append(runtimeAttrs, &lightstep_thrift.KeyValue{k, v})
	}
	return &lightstep_thrift.Runtime{
		StartMicros: thrift.Int64Ptr(r.startTime.UnixNano() / 1000),
		Attrs:       runtimeAttrs,
	}
}

func (r *Recorder) Disable() {
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.disabled {
		return
	}

	fmt.Printf("Disabling Runtime instance: %p", r)

	r.buffer.reset()
	r.disabled = true
}

// Every minReportingPeriod the reporting loop wakes up and checks to see if
// either (a) the Runtime's max reporting period is about to expire (see
// maxReportingPeriod()), (b) the number of buffered log records is
// approaching kMaxBufferedLogs, or if (c) the number of buffered span records
// is approaching kMaxBufferedSpans. If any of those conditions are true,
// pending data is flushed to the remote peer. If not, the reporting loop waits
// until the next cycle. See Runtime.maybeFlush() for details.
//
// This could alternatively be implemented using flush channels and so forth,
// but that would introduce opportunities for client code to block on the
// runtime library, and we want to avoid that at all costs (even dropping data,
// which can certainly happen with high data rates and/or unresponsive remote
// peers).
func (r *Recorder) shouldFlush() bool {
	r.lock.Lock()
	defer r.lock.Unlock()

	if time.Now().Add(minReportingPeriod).Sub(r.lastReportAttempt) > r.maxReportingPeriod {
		// Flush timeout.
		r.maybeLogInfof("--> timeout")
		return true
	} else if r.buffer.len() > r.buffer.cap()/2 {
		// Too many queued span records.
		r.maybeLogInfof("--> span queue")
		return true
	}
	return false
}

func (r *Recorder) reportLoop() {
	// (Thrift really should do this internally, but we saw some too-many-fd's
	// errors and thrift is the most likely culprit.)
	switch b := r.backend.(type) {
	case *lightstep_thrift.ReportingServiceClient:
		// TODO This is a bit racy with other calls to Flush, but we're
		// currently assuming that no one calls Flush after Disable.
		defer b.Transport.Close()
	}

	tickerChan := time.Tick(minReportingPeriod)
	for range tickerChan {
		r.maybeLogInfof("reporting alarm fired")

		// Kill the reportLoop() if we've been disabled.
		r.lock.Lock()
		if r.disabled {
			r.lock.Unlock()
			break
		}
		r.lock.Unlock()

		if r.shouldFlush() {
			r.Flush()
		}
	}
}
