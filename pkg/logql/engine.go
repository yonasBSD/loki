package logql

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/grafana/loki/v3/pkg/logqlmodel/metadata"
	"github.com/grafana/loki/v3/pkg/tracing"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/flagext"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	promql_parser "github.com/prometheus/prometheus/promql/parser"

	"github.com/grafana/dskit/tenant"

	"github.com/grafana/loki/v3/pkg/iter"
	"github.com/grafana/loki/v3/pkg/logproto"
	"github.com/grafana/loki/v3/pkg/logql/syntax"
	"github.com/grafana/loki/v3/pkg/logqlmodel"
	"github.com/grafana/loki/v3/pkg/logqlmodel/stats"
	"github.com/grafana/loki/v3/pkg/util"
	"github.com/grafana/loki/v3/pkg/util/constants"
	"github.com/grafana/loki/v3/pkg/util/httpreq"
	logutil "github.com/grafana/loki/v3/pkg/util/log"
	"github.com/grafana/loki/v3/pkg/util/server"
	"github.com/grafana/loki/v3/pkg/util/validation"
)

var tracer = otel.Tracer("pkg/logql")

const (
	DefaultBlockedQueryMessage = "blocked by policy"
)

var (
	QueryTime = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "logql",
		Name:      "query_duration_seconds",
		Help:      "LogQL query timings",
		Buckets:   prometheus.DefBuckets,
	}, []string{"query_type"})

	QueriesBlocked = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: constants.Loki,
		Name:      "blocked_queries",
		Help:      "Count of queries blocked by per-tenant policy",
	}, []string{"user"})

	lastEntryMinTime = time.Unix(-100, 0)
)

type QueryParams interface {
	LogSelector() (syntax.LogSelectorExpr, error)
	GetStart() time.Time
	GetEnd() time.Time
	GetShards() []string
	GetDeletes() []*logproto.Delete
}

// SelectParams specifies parameters passed to data selections.
type SelectLogParams struct {
	*logproto.QueryRequest
}

func (s SelectLogParams) WithStoreChunks(chunkRefGroup *logproto.ChunkRefGroup) SelectLogParams {
	cpy := *s.QueryRequest
	cpy.StoreChunks = chunkRefGroup
	return SelectLogParams{&cpy}
}

func (s SelectLogParams) String() string {
	if s.QueryRequest != nil {
		return fmt.Sprintf("selector=%s, direction=%s, start=%s, end=%s, limit=%d, shards=%s",
			s.Selector, logproto.Direction_name[int32(s.Direction)], s.Start, s.End, s.Limit, strings.Join(s.Shards, ","))
	}
	return ""
}

// LogSelector returns the LogSelectorExpr from the SelectParams.
// The `LogSelectorExpr` can then returns all matchers and filters to use for that request.
func (s SelectLogParams) LogSelector() (syntax.LogSelectorExpr, error) {
	if s.QueryRequest.Plan == nil {
		return nil, errors.New("query plan is empty")
	}
	expr, ok := s.QueryRequest.Plan.AST.(syntax.LogSelectorExpr)
	if !ok {
		return nil, errors.New("only log selector is supported")
	}
	return expr, nil
}

type SelectSampleParams struct {
	*logproto.SampleQueryRequest
}

func (s SelectSampleParams) WithStoreChunks(chunkRefGroup *logproto.ChunkRefGroup) SelectSampleParams {
	cpy := *s.SampleQueryRequest
	cpy.StoreChunks = chunkRefGroup
	return SelectSampleParams{&cpy}
}

// Expr returns the SampleExpr from the SelectSampleParams.
// The `LogSelectorExpr` can then returns all matchers and filters to use for that request.
func (s SelectSampleParams) Expr() (syntax.SampleExpr, error) {
	if s.SampleQueryRequest.Plan == nil {
		return nil, errors.New("query plan is empty")
	}
	expr, ok := s.SampleQueryRequest.Plan.AST.(syntax.SampleExpr)
	if !ok {
		return nil, errors.New("only sample expression supported")
	}
	return expr, nil
}

// LogSelector returns the LogSelectorExpr from the SelectParams.
// The `LogSelectorExpr` can then returns all matchers and filters to use for that request.
func (s SelectSampleParams) LogSelector() (syntax.LogSelectorExpr, error) {
	expr, err := s.Expr()
	if err != nil {
		return nil, err
	}
	return expr.Selector()
}

// Querier allows a LogQL expression to fetch an EntryIterator for a
// set of matchers and filters
type Querier interface {
	SelectLogs(context.Context, SelectLogParams) (iter.EntryIterator, error)
	SelectSamples(context.Context, SelectSampleParams) (iter.SampleIterator, error)
}

type Engine interface {
	Query(Params) Query
}

// EngineOpts is the list of options to use with the LogQL query engine.
type EngineOpts struct {
	// MaxLookBackPeriod is the maximum amount of time to look back for log lines.
	// only used for instant log queries.
	MaxLookBackPeriod time.Duration `yaml:"max_look_back_period"`

	// LogExecutingQuery will control if we log the query when Exec is called.
	LogExecutingQuery bool `yaml:"-"`

	// MaxCountMinSketchHeapSize is the maximum number of labels the heap for a topk query using a count min sketch
	// can track. This impacts the memory usage and accuracy of a sharded probabilistic topk query.
	MaxCountMinSketchHeapSize int `yaml:"max_count_min_sketch_heap_size"`

	// Enable the next generation Loki Query Engine for supported queries.
	EnableV2Engine bool `yaml:"enable_v2_engine" category:"experimental"`

	// Batch size of the v2 execution engine.
	BatchSize int `yaml:"batch_size" category:"experimental"`

	// CataloguePath is the path to the catalogue in the object store.
	CataloguePath string `yaml:"-" doc:"hidden" category:"experimental"`

	// DataobjScanPageCacheSize determines how many bytes of future page data
	// should be downloaded before it's immediately needed. Used to reduce the
	// number of roundtrips to object storage. Setting to zero disables
	// downloading pages that are not immediately needed.
	//
	// This setting is only used when the v2 engine is being used.
	DataobjScanPageCacheSize flagext.Bytes `yaml:"dataobjscan_page_cache_size" category:"experimental"`
}

func (opts *EngineOpts) RegisterFlagsWithPrefix(prefix string, f *flag.FlagSet) {
	_ = opts.DataobjScanPageCacheSize.Set("0B")

	f.DurationVar(&opts.MaxLookBackPeriod, prefix+"max-lookback-period", 30*time.Second, "The maximum amount of time to look back for log lines. Used only for instant log queries.")
	f.IntVar(&opts.MaxCountMinSketchHeapSize, prefix+"max-count-min-sketch-heap-size", 10_000, "The maximum number of labels the heap of a topk query using a count min sketch can track.")
	f.BoolVar(&opts.EnableV2Engine, prefix+"enable-v2-engine", false, "Experimental: Enable next generation query engine for supported queries.")
	f.IntVar(&opts.BatchSize, prefix+"batch-size", 100, "Experimental: Batch size of the next generation query engine.")
	f.StringVar(&opts.CataloguePath, prefix+"catalogue-path", "", "The path to the catalogue in the object store.")
	f.Var(&opts.DataobjScanPageCacheSize, prefix+"dataobjscan-page-cache-size", "Experimental: Maximum total size of future pages for DataObjScan to download before they are needed, for roundtrip reduction to object storage. Setting to zero disables downloading future pages. Only used in the next generation query engine.")

	// Log executing query by default
	opts.LogExecutingQuery = true
}

func (opts *EngineOpts) applyDefault() {
	if opts.MaxLookBackPeriod == 0 {
		opts.MaxLookBackPeriod = 30 * time.Second
	}
}

// QueryEngine is the LogQL engine.
type QueryEngine struct {
	logger           log.Logger
	evaluatorFactory EvaluatorFactory
	limits           Limits
	opts             EngineOpts
}

// NewEngine creates a new LogQL [QueryEngine].
func NewEngine(opts EngineOpts, q Querier, l Limits, logger log.Logger) *QueryEngine {
	opts.applyDefault()
	if logger == nil {
		logger = log.NewNopLogger()
	}
	return &QueryEngine{
		logger:           logger,
		evaluatorFactory: NewDefaultEvaluator(q, opts.MaxLookBackPeriod, opts.MaxCountMinSketchHeapSize),
		limits:           l,
		opts:             opts,
	}
}

// Query creates a new LogQL query. Instant/Range type is derived from the parameters.
func (qe *QueryEngine) Query(params Params) Query {
	return &query{
		logger:       qe.logger,
		params:       params,
		evaluator:    qe.evaluatorFactory,
		record:       true,
		logExecQuery: qe.opts.LogExecutingQuery,
		limits:       qe.limits,
	}
}

// Query is a LogQL query to be executed.
type Query interface {
	// Exec processes the query.
	Exec(ctx context.Context) (logqlmodel.Result, error)
}

type query struct {
	logger       log.Logger
	params       Params
	limits       Limits
	evaluator    EvaluatorFactory
	record       bool
	logExecQuery bool
}

func (q *query) resultLength(res promql_parser.Value) int {
	switch r := res.(type) {
	case promql.Vector:
		return len(r)
	case promql.Matrix:
		return r.TotalSamples()
	case logqlmodel.Streams:
		return int(r.Lines())
	case ProbabilisticQuantileMatrix:
		return len(r)
	default:
		// for `scalar` or `string` or any other return type, we just return `0` as result length.
		return 0
	}
}

// Exec Implements `Query`. It handles instrumentation & defers to Eval.
func (q *query) Exec(ctx context.Context) (logqlmodel.Result, error) {
	ctx, sp := tracer.Start(ctx, "query.Exec")
	defer sp.End()

	sp.SetAttributes(
		attribute.String("type", string(GetRangeType(q.params))),
		attribute.String("query", q.params.QueryString()),
		attribute.String("start", q.params.Start().String()),
		attribute.String("end", q.params.End().String()),
		attribute.String("step", q.params.Step().String()),
		attribute.String("length", q.params.End().Sub(q.params.Start()).String()),
	)

	if q.logExecQuery {
		queryHash := util.HashedQuery(q.params.QueryString())

		logValues := []interface{}{
			"msg", "executing query",
			"query", q.params.QueryString(),
			"query_hash", queryHash,
		}
		tags := httpreq.ExtractQueryTagsFromContext(ctx)
		tagValues := httpreq.TagsToKeyValues(tags)
		if GetRangeType(q.params) == InstantType {
			logValues = append(logValues, "type", "instant")
		} else {
			logValues = append(logValues, "type", "range", "length", q.params.End().Sub(q.params.Start()), "step", q.params.Step())
		}
		logValues = append(logValues, tagValues...)
		level.Info(logutil.WithContext(ctx, q.logger)).Log(logValues...)
	}

	rangeType := GetRangeType(q.params)
	timer := prometheus.NewTimer(QueryTime.WithLabelValues(string(rangeType)))
	defer timer.ObserveDuration()

	// records query statistics
	start := time.Now()
	statsCtx, ctx := stats.NewContext(ctx)
	metadataCtx, ctx := metadata.NewContext(ctx)

	data, err := q.Eval(ctx)

	queueTime, _ := ctx.Value(httpreq.QueryQueueTimeHTTPHeader).(time.Duration)

	statResult := statsCtx.Result(time.Since(start), queueTime, q.resultLength(data))
	sp.SetAttributes(tracing.KeyValuesToOTelAttributes(statResult.KVList())...)

	status, _ := server.ClientHTTPStatusAndError(err)

	if q.record {
		RecordRangeAndInstantQueryMetrics(ctx, q.logger, q.params, strconv.Itoa(status), statResult, data)
	}

	return logqlmodel.Result{
		Data:       data,
		Statistics: statResult,
		Headers:    metadataCtx.Headers(),
		Warnings:   metadataCtx.Warnings(),
	}, err
}

func (q *query) Eval(ctx context.Context) (promql_parser.Value, error) {
	tenants, _ := tenant.TenantIDs(ctx)
	timeoutCapture := func(id string) time.Duration { return q.limits.QueryTimeout(ctx, id) }
	queryTimeout := validation.SmallestPositiveNonZeroDurationPerTenant(tenants, timeoutCapture)
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	if q.checkBlocked(ctx, tenants) {
		return nil, logqlmodel.ErrBlocked
	}

	switch e := q.params.GetExpression().(type) {
	// A VariantsExpr is a specific type of SampleExpr, so make sure this case is evaulated first
	case syntax.VariantsExpr:
		tenants, _ := tenant.TenantIDs(ctx)
		multiVariantEnabled := false
		for _, t := range tenants {
			if q.limits.EnableMultiVariantQueries(t) {
				multiVariantEnabled = true
				break
			}
		}

		if !multiVariantEnabled {
			return nil, logqlmodel.ErrVariantsDisabled
		}

		value, err := q.evalVariants(ctx, e)
		return value, err
	case syntax.SampleExpr:
		value, err := q.evalSample(ctx, e)
		return value, err

	case syntax.LogSelectorExpr:
		itr, err := q.evaluator.NewIterator(ctx, e, q.params)
		if err != nil {
			return nil, err
		}

		encodingFlags := httpreq.ExtractEncodingFlagsFromCtx(ctx)
		if encodingFlags.Has(httpreq.FlagCategorizeLabels) {
			itr = iter.NewCategorizeLabelsIterator(itr)
		}

		defer util.LogErrorWithContext(ctx, "closing iterator", itr.Close)
		streams, err := readStreams(itr, q.params.Limit(), q.params.Direction(), q.params.Interval())
		return streams, err
	default:
		return nil, fmt.Errorf("unexpected type (%T): cannot evaluate", e)
	}
}

func (q *query) checkBlocked(ctx context.Context, tenants []string) bool {
	blocker := newQueryBlocker(ctx, q)

	for _, tenant := range tenants {
		if blocker.isBlocked(ctx, tenant) {
			QueriesBlocked.WithLabelValues(tenant).Inc()
			return true
		}
	}

	return false
}

// evalSample evaluate a sampleExpr
func (q *query) evalSample(ctx context.Context, expr syntax.SampleExpr) (promql_parser.Value, error) {
	if lit, ok := expr.(*syntax.LiteralExpr); ok {
		return q.evalLiteral(ctx, lit)
	}
	if vec, ok := expr.(*syntax.VectorExpr); ok {
		return q.evalVector(ctx, vec)
	}

	tenantIDs, err := tenant.TenantIDs(ctx)
	if err != nil {
		return nil, err
	}

	maxIntervalCapture := func(id string) time.Duration { return q.limits.MaxQueryRange(ctx, id) }
	maxQueryInterval := validation.SmallestPositiveNonZeroDurationPerTenant(tenantIDs, maxIntervalCapture)
	if maxQueryInterval != 0 {
		err = q.checkIntervalLimit(expr, maxQueryInterval)
		if err != nil {
			return nil, err
		}
	}

	expr, err = optimizeSampleExpr(expr)
	if err != nil {
		return nil, err
	}

	stepEvaluator, err := q.evaluator.NewStepEvaluator(ctx, q.evaluator, expr, q.params)
	if err != nil {
		return nil, err
	}
	defer util.LogErrorWithContext(ctx, "closing SampleExpr", stepEvaluator.Close)

	next, _, r := stepEvaluator.Next()
	if stepEvaluator.Error() != nil {
		return nil, stepEvaluator.Error()
	}

	if next && r != nil {
		switch vec := r.(type) {
		case SampleVector:
			maxSeriesCapture := func(id string) int { return q.limits.MaxQuerySeries(ctx, id) }
			maxSeries := validation.SmallestPositiveIntPerTenant(tenantIDs, maxSeriesCapture)
			mfl := false
			if rae, ok := expr.(*syntax.RangeAggregationExpr); ok && (rae.Operation == syntax.OpRangeTypeFirstWithTimestamp || rae.Operation == syntax.OpRangeTypeLastWithTimestamp) {
				mfl = true
			}
			return q.JoinSampleVector(ctx, next, vec, stepEvaluator, maxSeries, mfl)
		case ProbabilisticQuantileVector:
			return JoinQuantileSketchVector(next, vec, stepEvaluator, q.params)
		case CountMinSketchVector:
			return JoinCountMinSketchVector(next, vec, stepEvaluator, q.params)
		case HeapCountMinSketchVector:
			return JoinCountMinSketchVector(next, vec.CountMinSketchVector, stepEvaluator, q.params)
		default:
			return nil, fmt.Errorf("unsupported result type: %T", r)
		}
	}
	return nil, errors.New("unexpected empty result")
}

func vectorsToSeries(vec promql.Vector, sm map[uint64]promql.Series) {
	vectorsToSeriesWithLimit(vec, sm, 0) // 0 means no limit
}

func vectorsToSeriesWithLimit(vec promql.Vector, sm map[uint64]promql.Series, maxSeries int) bool {
	limitExceeded := false
	for _, p := range vec {
		var (
			series promql.Series
			hash   = labels.StableHash(p.Metric)
			ok     bool
		)

		series, ok = sm[hash]

		// create a new series if under the limit
		if !ok && !limitExceeded {
			// Check if adding a new series would exceed the limit
			if maxSeries > 0 && len(sm) >= maxSeries {
				// We've reached the series limit, skip adding new series
				limitExceeded = true
				continue
			}
			series = promql.Series{
				Metric: p.Metric,
				Floats: make([]promql.FPoint, 0, 1),
			}
			sm[hash] = series
		}
		series.Floats = append(series.Floats, promql.FPoint{
			T: p.T,
			F: p.F,
		})
		sm[hash] = series
	}
	return limitExceeded
}

func multiVariantVectorsToSeries(ctx context.Context, maxSeries int, vec promql.Vector, sm map[string]map[uint64]promql.Series, skippedVariants map[string]struct{}) int {
	count := 0
	metadataCtx := metadata.FromContext(ctx)

	for _, p := range vec {
		var (
			series promql.Series
			hash   = labels.StableHash(p.Metric)
			ok     bool
		)

		if !p.Metric.Has(constants.VariantLabel) {
			continue
		}

		variantLabel := p.Metric.Get(constants.VariantLabel)

		if _, ok = skippedVariants[variantLabel]; ok {
			continue
		}

		if _, ok = sm[variantLabel]; !ok {
			variant := make(map[uint64]promql.Series)
			sm[variantLabel] = variant
		}

		if len(sm[variantLabel]) >= maxSeries {
			skippedVariants[variantLabel] = struct{}{}
			// This can cause count to be negative, as we may be removing series added in a previous iteration
			// However, since we sum this value across all iterations, a negative will make sure the total series count is correct
			count = count - len(sm[variantLabel])
			delete(sm, variantLabel)
			metadataCtx.AddWarning(fmt.Sprintf("maximum of series (%d) reached for variant (%s)", maxSeries, variantLabel))
			continue
		}

		series, ok = sm[variantLabel][hash]
		if !ok {
			series = promql.Series{
				Metric: p.Metric,
				Floats: make([]promql.FPoint, 0, 1),
			}
			sm[variantLabel][hash] = series
			count++
		}

		series.Floats = append(series.Floats, promql.FPoint{
			T: p.T,
			F: p.F,
		})
		sm[variantLabel][hash] = series
	}

	return count
}

func (q *query) JoinSampleVector(ctx context.Context, next bool, r StepResult, stepEvaluator StepEvaluator, maxSeries int, mergeFirstLast bool) (promql_parser.Value, error) {
	vec := promql.Vector{}
	if next {
		vec = r.SampleVector()
	}
	seriesIndex := map[uint64]promql.Series{}

	// fail fast for the first step or instant query
	if len(vec) > maxSeries {
		if httpreq.IsLogsDrilldownRequest(ctx) {
			// For Logs Drilldown requests, return partial results with warning
			vec = vec[:maxSeries]
			metadata.FromContext(ctx).AddWarning(fmt.Sprintf("maximum number of series (%d) reached for a single query; returning partial results", maxSeries))
			// Since we've already reached the series limit, skip processing additional steps and add the initial vector to seriesIndex
			next = false
			vectorsToSeries(vec, seriesIndex)
		} else {
			return nil, logqlmodel.NewSeriesLimitError(maxSeries)
		}
	}

	if GetRangeType(q.params) == InstantType {
		// an instant query sharded first/last_over_time can return a single vector
		if mergeFirstLast {
			vectorsToSeries(vec, seriesIndex)
			series := make([]promql.Series, 0, len(seriesIndex))
			for _, s := range seriesIndex {
				series = append(series, s)
			}
			result := promql.Matrix(series)
			sort.Sort(result)
			return result, stepEvaluator.Error()
		}

		sortByValue, err := Sortable(q.params)
		if err != nil {
			return nil, fmt.Errorf("fail to check Sortable, logql: %s ,err: %s", q.params.QueryString(), err)
		}
		if !sortByValue {
			sort.Slice(vec, func(i, j int) bool { return labels.Compare(vec[i].Metric, vec[j].Metric) < 0 })
		}
		return vec, nil
	}

	for next {
		vec = r.SampleVector()

		if httpreq.IsLogsDrilldownRequest(ctx) {
			// For Logs Drilldown requests, use limited vectorsToSeries to prevent exceeding maxSeries
			limitExceeded := vectorsToSeriesWithLimit(vec, seriesIndex, maxSeries)
			// If the limit was exceeded (series were skipped), add warning and break
			if limitExceeded {
				metadata.FromContext(ctx).AddWarning(fmt.Sprintf("maximum number of series (%d) reached for a single query; returning partial results", maxSeries))
				break // Break out of the loop to return partial results
			}
		} else {
			// For non-drilldown requests, use unlimited vectorsToSeries and check for hard limit
			vectorsToSeries(vec, seriesIndex)
			if len(seriesIndex) > maxSeries {
				return nil, logqlmodel.NewSeriesLimitError(maxSeries)
			}
		}

		next, _, r = stepEvaluator.Next()
		if stepEvaluator.Error() != nil {
			return nil, stepEvaluator.Error()
		}
	}

	series := make([]promql.Series, 0, len(seriesIndex))
	for _, s := range seriesIndex {
		series = append(series, s)
	}
	result := promql.Matrix(series)
	sort.Sort(result)

	return result, stepEvaluator.Error()
}

func (q *query) JoinMultiVariantSampleVector(ctx context.Context, next bool, r StepResult, stepEvaluator StepEvaluator, maxSeries int) (promql_parser.Value, error) {
	vec := promql.Vector{}
	if next {
		vec = r.SampleVector()
	}

	seriesIndex := map[string]map[uint64]promql.Series{}
	// Track variants that exceed the limit across all steps
	// use a map for faster lookup
	skippedVariants := map[string]struct{}{}

	if GetRangeType(q.params) == InstantType {
		multiVariantVectorsToSeries(ctx, maxSeries, vec, seriesIndex, skippedVariants)

		// Filter the vector to remove skipped variants
		filterVariantVector(&vec, skippedVariants)

		// an instant query sharded first/last_over_time can return a single vector
		sortByValue, err := Sortable(q.params)
		if err != nil {
			return nil, fmt.Errorf("fail to check Sortable, logql: %s ,err: %s", q.params.QueryString(), err)
		}
		if !sortByValue {
			sort.Slice(vec, func(i, j int) bool { return labels.Compare(vec[i].Metric, vec[j].Metric) < 0 })
		}
		return vec, nil
	}

	seriesCount := 0
	for next {
		vec = r.SampleVector()
		// Filter out any samples from variants we've already skipped
		filterVariantVector(&vec, skippedVariants)
		seriesCount += multiVariantVectorsToSeries(ctx, maxSeries, vec, seriesIndex, skippedVariants)

		next, _, r = stepEvaluator.Next()
		if stepEvaluator.Error() != nil {
			return nil, stepEvaluator.Error()
		}
	}

	series := make([]promql.Series, 0, seriesCount)
	for _, ss := range seriesIndex {
		for _, s := range ss {
			series = append(series, s)
		}
	}
	result := promql.Matrix(series)
	sort.Sort(result)

	return result, stepEvaluator.Error()
}

// filterVariantVector removes samples from the vector that belong to skipped variants
func filterVariantVector(vec *promql.Vector, skipped map[string]struct{}) {
	if len(skipped) == 0 {
		return
	}

	// Filter the vector
	for i := 0; i < len(*vec); i++ {
		sample := (*vec)[i]
		if sample.Metric.Has(constants.VariantLabel) {
			variant := sample.Metric.Get(constants.VariantLabel)
			if _, shouldSkip := skipped[variant]; shouldSkip {
				*vec = slices.Delete(*vec, i, i+1)
				i-- // Adjust the index since we removed an item
			}
		}
	}
}

func (q *query) checkIntervalLimit(expr syntax.SampleExpr, limit time.Duration) error {
	var err error
	expr.Walk(func(e syntax.Expr) bool {
		switch e := e.(type) {
		case *syntax.LogRangeExpr:
			if e.Interval > limit {
				err = fmt.Errorf("%w: [%s] > [%s]", logqlmodel.ErrIntervalLimit, model.Duration(e.Interval), model.Duration(limit))
			}
		}
		return true
	})
	return err
}

func (q *query) evalLiteral(_ context.Context, expr *syntax.LiteralExpr) (promql_parser.Value, error) {
	value, err := expr.Value()
	if err != nil {
		return nil, err
	}
	s := promql.Scalar{
		T: q.params.Start().UnixNano() / int64(time.Millisecond),
		V: value,
	}

	if GetRangeType(q.params) == InstantType {
		return s, nil
	}

	return PopulateMatrixFromScalar(s, q.params), nil
}

func (q *query) evalVector(_ context.Context, expr *syntax.VectorExpr) (promql_parser.Value, error) {
	value, err := expr.Value()
	if err != nil {
		return nil, err
	}
	s := promql.Scalar{
		T: q.params.Start().UnixNano() / int64(time.Millisecond),
		V: value,
	}

	if GetRangeType(q.params) == InstantType {
		return promql.Vector{promql.Sample{
			T:      q.params.Start().UnixMilli(),
			F:      value,
			Metric: labels.Labels{},
		}}, nil
	}

	return PopulateMatrixFromScalar(s, q.params), nil
}

func PopulateMatrixFromScalar(data promql.Scalar, params Params) promql.Matrix {
	var (
		start  = params.Start()
		end    = params.End()
		step   = params.Step()
		series = promql.Series{
			Floats: make(
				[]promql.FPoint,
				0,
				// allocate enough space for all needed entries
				int(end.Sub(start)/step)+1,
			),
		}
	)

	for ts := start; !ts.After(end); ts = ts.Add(step) {
		series.Floats = append(series.Floats, promql.FPoint{
			T: ts.UnixNano() / int64(time.Millisecond),
			F: data.V,
		})
	}
	return promql.Matrix{series}
}

// readStreams reads the streams from the iterator and returns them sorted.
// If categorizeLabels is true, the stream labels contains just the stream labels and entries inside each stream have their
// structuredMetadata and parsed fields populated with structured metadata labels plus the parsed labels respectively.
// Otherwise, the stream labels are the whole series labels including the stream labels, structured metadata labels and parsed labels.
func readStreams(i iter.EntryIterator, size uint32, dir logproto.Direction, interval time.Duration) (logqlmodel.Streams, error) {
	streams := map[string]*logproto.Stream{}
	respSize := uint32(0)
	// lastEntry should be a really old time so that the first comparison is always true, we use a negative
	// value here because many unit tests start at time.Unix(0,0)
	lastEntry := lastEntryMinTime
	for respSize < size && i.Next() {
		streamLabels, entry := i.Labels(), i.At()

		forwardShouldOutput := dir == logproto.FORWARD &&
			(entry.Timestamp.Equal(lastEntry.Add(interval)) || entry.Timestamp.After(lastEntry.Add(interval)))
		backwardShouldOutput := dir == logproto.BACKWARD &&
			(entry.Timestamp.Equal(lastEntry.Add(-interval)) || entry.Timestamp.Before(lastEntry.Add(-interval)))

		// If step == 0 output every line.
		// If lastEntry.Unix < 0 this is the first pass through the loop and we should output the line.
		// Then check to see if the entry is equal to, or past a forward or reverse step
		if interval == 0 || lastEntry.Unix() < 0 || forwardShouldOutput || backwardShouldOutput {
			stream, ok := streams[streamLabels]
			if !ok {
				stream = &logproto.Stream{
					Labels: streamLabels,
				}
				streams[streamLabels] = stream
			}
			stream.Entries = append(stream.Entries, entry)
			lastEntry = i.At().Timestamp
			respSize++
		}
	}

	result := make(logqlmodel.Streams, 0, len(streams))
	for _, stream := range streams {
		result = append(result, *stream)
	}
	sort.Sort(result)
	return result, i.Err()
}

type groupedAggregation struct {
	labels      labels.Labels
	value       float64
	mean        float64
	groupCount  int
	heap        vectorByValueHeap
	reverseHeap vectorByReverseValueHeap
}

func (q *query) evalVariants(
	ctx context.Context,
	expr syntax.VariantsExpr,
) (promql_parser.Value, error) {
	tenantIDs, err := tenant.TenantIDs(ctx)
	if err != nil {
		return nil, err
	}

	maxIntervalCapture := func(id string) time.Duration { return q.limits.MaxQueryRange(ctx, id) }
	maxQueryInterval := validation.SmallestPositiveNonZeroDurationPerTenant(
		tenantIDs,
		maxIntervalCapture,
	)
	if maxQueryInterval != 0 {
		for i, v := range expr.Variants() {
			err = q.checkIntervalLimit(v, maxQueryInterval)
			if err != nil {
				return nil, err
			}

			vExpr, err := optimizeSampleExpr(v)
			if err != nil {
				return nil, err
			}

			if err = expr.SetVariant(i, vExpr); err != nil {
				return nil, err
			}
		}
	}

	stepEvaluator, err := q.evaluator.NewVariantsStepEvaluator(ctx, expr, q.params)
	if err != nil {
		return nil, err
	}
	defer util.LogErrorWithContext(ctx, "closing VariantsExpr", stepEvaluator.Close)

	next, _, r := stepEvaluator.Next()
	if stepEvaluator.Error() != nil {
		return nil, stepEvaluator.Error()
	}

	if next && r != nil {
		switch vec := r.(type) {
		case SampleVector:
			maxSeriesCapture := func(id string) int { return q.limits.MaxQuerySeries(ctx, id) }
			maxSeries := validation.SmallestPositiveIntPerTenant(tenantIDs, maxSeriesCapture)
			return q.JoinMultiVariantSampleVector(ctx, next, vec, stepEvaluator, maxSeries)
		default:
			return nil, fmt.Errorf("unsupported result type: %T", r)
		}
	}
	return nil, errors.New("unexpected empty result")
}
