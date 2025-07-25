package engine

import (
	"context"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/stretchr/testify/require"

	"github.com/grafana/loki/v3/pkg/engine/executor"
	"github.com/grafana/loki/v3/pkg/engine/internal/datatype"
	"github.com/grafana/loki/v3/pkg/engine/internal/types"
	"github.com/grafana/loki/v3/pkg/logproto"
	"github.com/grafana/loki/v3/pkg/logqlmodel"
	"github.com/grafana/loki/v3/pkg/logqlmodel/metadata"
	"github.com/grafana/loki/v3/pkg/logqlmodel/stats"

	"github.com/prometheus/prometheus/promql"

	"github.com/grafana/loki/pkg/push"
)

func createRecord(t *testing.T, schema *arrow.Schema, data [][]interface{}) arrow.Record {
	mem := memory.NewGoAllocator()
	builder := array.NewRecordBuilder(mem, schema)
	defer builder.Release()

	for _, row := range data {
		for j, val := range row {
			if val == nil {
				builder.Field(j).AppendNull()
				continue
			}

			switch builder.Field(j).(type) {
			case *array.BooleanBuilder:
				builder.Field(j).(*array.StringBuilder).Append(val.(string))
			case *array.StringBuilder:
				builder.Field(j).(*array.StringBuilder).Append(val.(string))
			case *array.Uint64Builder:
				builder.Field(j).(*array.Uint64Builder).Append(val.(uint64))
			case *array.Int64Builder:
				builder.Field(j).(*array.Int64Builder).Append(val.(int64))
			case *array.Float64Builder:
				builder.Field(j).(*array.Float64Builder).Append(val.(float64))
			case *array.TimestampBuilder:
				builder.Field(j).(*array.TimestampBuilder).Append(val.(arrow.Timestamp))
			default:
				t.Fatal("invalid field type")
			}
		}
	}

	return builder.NewRecord()
}

func TestStreamsResultBuilder(t *testing.T) {
	mdTypeLabel := datatype.ColumnMetadata(types.ColumnTypeLabel, datatype.Loki.String)
	mdTypeMetadata := datatype.ColumnMetadata(types.ColumnTypeMetadata, datatype.Loki.String)

	t.Run("rows without log line, timestamp, or labels are ignored", func(t *testing.T) {
		schema := arrow.NewSchema(
			[]arrow.Field{
				{Name: types.ColumnNameBuiltinTimestamp, Type: arrow.FixedWidthTypes.Timestamp_ns, Metadata: datatype.ColumnMetadataBuiltinTimestamp},
				{Name: types.ColumnNameBuiltinMessage, Type: arrow.BinaryTypes.String, Metadata: datatype.ColumnMetadataBuiltinMessage},
				{Name: "env", Type: arrow.BinaryTypes.String, Metadata: mdTypeLabel},
			},
			nil,
		)

		data := [][]interface{}{
			{arrow.Timestamp(1620000000000000001), nil, "prod"},
			{nil, "log line", "prod"},
			{arrow.Timestamp(1620000000000000003), "log line", nil},
		}

		record := createRecord(t, schema, data)
		defer record.Release()

		pipeline := executor.NewBufferedPipeline(record)
		defer pipeline.Close()

		builder := newStreamsResultBuilder()
		err := collectResult(context.Background(), pipeline, builder)

		require.NoError(t, err)
		require.Equal(t, 0, builder.Len(), "expected no entries to be collected")
	})

	t.Run("fields without metadata are ignored", func(t *testing.T) {
		schema := arrow.NewSchema(
			[]arrow.Field{
				{Name: types.ColumnNameBuiltinTimestamp, Type: arrow.FixedWidthTypes.Timestamp_ns},
				{Name: types.ColumnNameBuiltinMessage, Type: arrow.BinaryTypes.String},
				{Name: "env", Type: arrow.BinaryTypes.String},
			},
			nil,
		)

		data := [][]interface{}{
			{arrow.Timestamp(1620000000000000001), "log line 1", "prod"},
			{arrow.Timestamp(1620000000000000002), "log line 2", "prod"},
			{arrow.Timestamp(1620000000000000003), "log line 3", "prod"},
		}

		record := createRecord(t, schema, data)
		defer record.Release()

		pipeline := executor.NewBufferedPipeline(record)
		defer pipeline.Close()

		builder := newStreamsResultBuilder()
		err := collectResult(context.Background(), pipeline, builder)

		require.NoError(t, err)
		require.Equal(t, 0, builder.Len(), "expected no entries to be collected")
	})

	t.Run("successful conversion of labels, log line, timestamp, and structured metadata ", func(t *testing.T) {
		schema := arrow.NewSchema(
			[]arrow.Field{
				{Name: types.ColumnNameBuiltinTimestamp, Type: arrow.FixedWidthTypes.Timestamp_ns, Metadata: datatype.ColumnMetadataBuiltinTimestamp},
				{Name: types.ColumnNameBuiltinMessage, Type: arrow.BinaryTypes.String, Metadata: datatype.ColumnMetadataBuiltinMessage},
				{Name: "env", Type: arrow.BinaryTypes.String, Metadata: mdTypeLabel},
				{Name: "namespace", Type: arrow.BinaryTypes.String, Metadata: mdTypeLabel},
				{Name: "traceID", Type: arrow.BinaryTypes.String, Metadata: mdTypeMetadata},
			},
			nil,
		)

		data := [][]interface{}{
			{arrow.Timestamp(1620000000000000001), "log line 1", "dev", "loki-dev-001", "860e403fcf754312"},
			{arrow.Timestamp(1620000000000000002), "log line 2", "prod", "loki-prod-001", "46ce02549441e41c"},
			{arrow.Timestamp(1620000000000000003), "log line 3", "dev", "loki-dev-002", "61330481e1e59b18"},
			{arrow.Timestamp(1620000000000000004), "log line 4", "prod", "loki-prod-001", "40e50221e284b9d2"},
			{arrow.Timestamp(1620000000000000005), "log line 5", "dev", "loki-dev-002", "0cf883f112ad239b"},
		}

		record := createRecord(t, schema, data)
		defer record.Release()

		pipeline := executor.NewBufferedPipeline(record)
		defer pipeline.Close()

		builder := newStreamsResultBuilder()
		err := collectResult(context.Background(), pipeline, builder)

		require.NoError(t, err)
		require.Equal(t, 5, builder.Len())

		md, _ := metadata.NewContext(t.Context())
		result := builder.Build(stats.Result{}, md)
		require.Equal(t, 5, result.Data.(logqlmodel.Streams).Len())

		expected := logqlmodel.Streams{
			push.Stream{
				Labels: labels.FromStrings("env", "dev", "namespace", "loki-dev-001", "traceID", "860e403fcf754312").String(),
				Entries: []logproto.Entry{
					{Line: "log line 1", Timestamp: time.Unix(0, 1620000000000000001), StructuredMetadata: logproto.FromLabelsToLabelAdapters(labels.FromStrings("traceID", "860e403fcf754312")), Parsed: logproto.FromLabelsToLabelAdapters(labels.Labels{})},
				},
			},
			push.Stream{
				Labels: labels.FromStrings("env", "dev", "namespace", "loki-dev-002", "traceID", "0cf883f112ad239b").String(),
				Entries: []logproto.Entry{
					{Line: "log line 5", Timestamp: time.Unix(0, 1620000000000000005), StructuredMetadata: logproto.FromLabelsToLabelAdapters(labels.FromStrings("traceID", "0cf883f112ad239b")), Parsed: logproto.FromLabelsToLabelAdapters(labels.Labels{})},
				},
			},
			push.Stream{
				Labels: labels.FromStrings("env", "dev", "namespace", "loki-dev-002", "traceID", "61330481e1e59b18").String(),
				Entries: []logproto.Entry{
					{Line: "log line 3", Timestamp: time.Unix(0, 1620000000000000003), StructuredMetadata: logproto.FromLabelsToLabelAdapters(labels.FromStrings("traceID", "61330481e1e59b18")), Parsed: logproto.FromLabelsToLabelAdapters(labels.Labels{})},
				},
			},
			push.Stream{
				Labels: labels.FromStrings("env", "prod", "namespace", "loki-prod-001", "traceID", "40e50221e284b9d2").String(),
				Entries: []logproto.Entry{
					{Line: "log line 4", Timestamp: time.Unix(0, 1620000000000000004), StructuredMetadata: logproto.FromLabelsToLabelAdapters(labels.FromStrings("traceID", "40e50221e284b9d2")), Parsed: logproto.FromLabelsToLabelAdapters(labels.Labels{})},
				},
			},
			push.Stream{
				Labels: labels.FromStrings("env", "prod", "namespace", "loki-prod-001", "traceID", "46ce02549441e41c").String(),
				Entries: []logproto.Entry{
					{Line: "log line 2", Timestamp: time.Unix(0, 1620000000000000002), StructuredMetadata: logproto.FromLabelsToLabelAdapters(labels.FromStrings("traceID", "46ce02549441e41c")), Parsed: logproto.FromLabelsToLabelAdapters(labels.Labels{})},
				},
			},
		}
		require.Equal(t, expected, result.Data.(logqlmodel.Streams))
	})
}

func TestVectorResultBuilder(t *testing.T) {
	mdTypeString := datatype.ColumnMetadata(types.ColumnTypeAmbiguous, datatype.Loki.String)

	t.Run("successful conversion of vector data", func(t *testing.T) {
		schema := arrow.NewSchema(
			[]arrow.Field{
				{Name: types.ColumnNameBuiltinTimestamp, Type: arrow.FixedWidthTypes.Timestamp_ns, Metadata: datatype.ColumnMetadataBuiltinTimestamp},
				{Name: types.ColumnNameGeneratedValue, Type: arrow.PrimitiveTypes.Int64, Metadata: datatype.ColumnMetadata(types.ColumnTypeGenerated, datatype.Loki.Integer)},
				{Name: "instance", Type: arrow.BinaryTypes.String, Metadata: mdTypeString},
				{Name: "job", Type: arrow.BinaryTypes.String, Metadata: mdTypeString},
			},
			nil,
		)

		data := [][]any{
			{arrow.Timestamp(1620000000000000000), int64(42), "localhost:9090", "prometheus"},
			{arrow.Timestamp(1620000000000000000), int64(23), "localhost:9100", "node-exporter"},
			{arrow.Timestamp(1620000000000000000), int64(15), "localhost:9100", "prometheus"},
		}

		record := createRecord(t, schema, data)
		defer record.Release()

		pipeline := executor.NewBufferedPipeline(record)
		defer pipeline.Close()

		builder := newVectorResultBuilder()
		err := collectResult(context.Background(), pipeline, builder)

		require.NoError(t, err)
		require.Equal(t, 3, builder.Len())

		md, _ := metadata.NewContext(t.Context())
		result := builder.Build(stats.Result{}, md)
		vector := result.Data.(promql.Vector)
		require.Equal(t, 3, len(vector))

		// Check first sample
		require.Equal(t, int64(1620000000000), vector[0].T)
		require.Equal(t, 42.0, vector[0].F)
		require.Equal(t, labels.FromStrings("instance", "localhost:9090", "job", "prometheus"), vector[0].Metric)

		// Check second sample
		require.Equal(t, int64(1620000000000), vector[1].T)
		require.Equal(t, 23.0, vector[1].F)
		require.Equal(t, labels.FromStrings("instance", "localhost:9100", "job", "node-exporter"), vector[1].Metric)

		// Check third sample
		require.Equal(t, int64(1620000000000), vector[2].T)
		require.Equal(t, 15.0, vector[2].F)
		require.Equal(t, labels.FromStrings("instance", "localhost:9100", "job", "prometheus"), vector[2].Metric)
	})

	// TODO:(ashwanth) also enforce grouping labels are all present?
	t.Run("rows without timestamp or value are ignored", func(t *testing.T) {
		schema := arrow.NewSchema(
			[]arrow.Field{
				{Name: types.ColumnNameBuiltinTimestamp, Type: arrow.FixedWidthTypes.Timestamp_ns, Metadata: datatype.ColumnMetadataBuiltinTimestamp},
				{Name: types.ColumnNameGeneratedValue, Type: arrow.PrimitiveTypes.Int64, Metadata: datatype.ColumnMetadata(types.ColumnTypeGenerated, datatype.Loki.Integer)},
				{Name: "instance", Type: arrow.BinaryTypes.String, Metadata: mdTypeString},
			},
			nil,
		)

		data := [][]interface{}{
			{nil, int64(42), "localhost:9090"},
			{arrow.Timestamp(1620000000000000000), nil, "localhost:9100"},
		}

		record := createRecord(t, schema, data)
		defer record.Release()

		pipeline := executor.NewBufferedPipeline(record)
		defer pipeline.Close()

		builder := newVectorResultBuilder()
		err := collectResult(context.Background(), pipeline, builder)

		require.NoError(t, err)
		require.Equal(t, 0, builder.Len(), "expected no samples to be collected")
	})
}
