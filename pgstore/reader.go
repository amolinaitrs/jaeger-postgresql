package pgstore

import (
	"context"
	"time"

	"github.com/go-pg/pg/v9"

	hclog "github.com/hashicorp/go-hclog"

	"github.com/jaegertracing/jaeger/model"
	"github.com/jaegertracing/jaeger/storage/spanstore"
)

var _ spanstore.Reader = (*Reader)(nil)

// Reader can query for and load traces from PostgreSQL v2.x.
type Reader struct {
	db *pg.DB

	logger hclog.Logger
}

// NewReader returns a new SpanReader for PostgreSQL v2.x.
func NewReader(db *pg.DB, logger hclog.Logger) *Reader {
	return &Reader{
		db:     db,
		logger: logger,
	}
}

// GetServices returns all services traced by Jaeger
func (r *Reader) GetServices(ctx context.Context) ([]string, error) {

	var services []Service
	err := r.db.Model(&services).Order("service_name ASC").Select()
	ret := make([]string, 0, len(services))

	for _, service := range services {
		if len(service.ServiceName) > 0 {
			ret = append(ret, service.ServiceName)
		}
	}

	return ret, err
}

// GetOperations returns all operations for a specific service traced by Jaeger
func (r *Reader) GetOperations(ctx context.Context, param spanstore.OperationQueryParameters) ([]spanstore.Operation, error) {

	var operations []Operation
	err := r.db.Model(&operations).Order("operation_name ASC").Select()
	ret := make([]spanstore.Operation, 0, len(operations))
	for _, operation := range operations {
		if len(operation.OperationName) > 0 {
			ret = append(ret, spanstore.Operation{Name: operation.OperationName})
		}
	}

	return ret, err
}

// GetTrace takes a traceID and returns a Trace associated with that traceID
func (r *Reader) GetTrace(ctx context.Context, traceID model.TraceID) (*model.Trace, error) {

	builder := &whereBuilder{where: "", params: make([]interface{}, 0)}

	if traceID.Low > 0 {
		builder.andWhere(traceID.Low, "trace_id_low = ?")
	}
	if traceID.High > 0 {
		builder.andWhere(traceID.High, "trace_id_high = ?")
	}

	var spans []Span
	query := r.db.Model(&spans).Where(builder.where, builder.params...).Relation("Operation").Relation("Service").Relation("SpanRefs") //.Limit(1)
	err := query.Select()
	ret := make([]*model.Span, 0, len(spans))
	ret2 := make([]model.Trace_ProcessMapping, 0, len(spans))
	for _, span := range spans {
		ret = append(ret, toModelSpan(span))
		ret2 = append(ret2, model.Trace_ProcessMapping{
			ProcessID: span.ProcessID,
			Process: model.Process{
				ServiceName: span.Service.ServiceName,
				Tags:        mapToModelKV(span.ProcessTags),
			},
		})
	}

	trace := &model.Trace{Spans: ret, ProcessMap: ret2}

	return trace, err
}

func buildTraceWhere(query *spanstore.TraceQueryParameters) *whereBuilder {
	builder := &whereBuilder{where: "", params: make([]interface{}, 0)}

	if len(query.ServiceName) > 0 {
		builder.andWhere(query.ServiceName, "service.service_name = ?")
	}
	if len(query.OperationName) > 0 {
		builder.andWhere(query.OperationName, "operation.operation_name = ?")
	}
	if query.StartTimeMin.After(time.Time{}) {
		builder.andWhere(query.StartTimeMin, "start_time >= ?")
	}
	if query.StartTimeMax.After(time.Time{}) {
		//TODO builder.andWhere(query.StartTimeMax, "start_time < ?")
	}
	if query.DurationMin > 0*time.Second {
		builder.andWhere(query.DurationMin, "duration < ?")
	}
	if query.DurationMax > 0*time.Second {
		builder.andWhere(query.DurationMax, "duration > ?")
	}

	//TODO Tags map[]string

	return builder
}

// FindTraces retrieve traces that match the traceQuery
func (r *Reader) FindTraces(ctx context.Context, query *spanstore.TraceQueryParameters) ([]*model.Trace, error) {

	traceIDs, err := r.FindTraceIDs(ctx, query)
	ret := make([]*model.Trace, 0, len(traceIDs))
	if err != nil {
		return ret, err
	}

	grouping := make(map[model.TraceID]*model.Trace)
	for _, traceID := range traceIDs {
		var spans []Span
		err = r.db.Model(&spans).Where("trace_id_low = ? and trace_id_high = ?", traceID.Low, traceID.High).
			Relation("Operation").Relation("Service").Relation("SpanRefs").
			Order("start_time ASC").Select()
		if err != nil {
			return ret, err
		}
		for _, span := range spans {
			modelSpan := toModelSpan(span)
			trace, found := grouping[modelSpan.TraceID]
			if !found {
				trace = &model.Trace{
					Spans:      make([]*model.Span, 0, len(spans)),
					ProcessMap: make([]model.Trace_ProcessMapping, 0, len(spans)),
				}
				grouping[modelSpan.TraceID] = trace
			}
			trace.Spans = append(trace.Spans, modelSpan)
			procMap := model.Trace_ProcessMapping{
				ProcessID: span.ProcessID,
				Process: model.Process{
					ServiceName: span.Service.ServiceName,
					Tags:        mapToModelKV(span.ProcessTags),
				},
			}
			trace.ProcessMap = append(trace.ProcessMap, procMap)
		}
	}

	for _, trace := range grouping {
		ret = append(ret, trace)
	}

	return ret, err
}

// FindTraceIDs retrieve traceIDs that match the traceQuery
func (r *Reader) FindTraceIDs(ctx context.Context, query *spanstore.TraceQueryParameters) (ret []model.TraceID, err error) {

	builder := buildTraceWhere(query)

	limit := query.NumTraces
	if limit <= 0 {
		limit = 10
	}

	err = r.db.Model((*Span)(nil)).
		Join("JOIN operations AS operation ON operation.id = span.operation_id").
		Join("JOIN services AS service ON service.id = span.service_id").
		ColumnExpr("distinct trace_id_low as Low, trace_id_high as High").
		Where(builder.where, builder.params...).Limit(100 * limit).Select(&ret)

	return ret, err
}

// GetDependencies returns all inter-service dependencies
func (r *Reader) GetDependencies(endTs time.Time, lookback time.Duration) (ret []model.DependencyLink, err error) {

	err = r.db.Model((*SpanRef)(nil)).
		ColumnExpr("source_spans.service_id AS parent").
		ColumnExpr("source_service.service_name AS parent_name").
		ColumnExpr("child_spans.service_id AS child").
		ColumnExpr("child_service.service_name AS child_name").
		ColumnExpr("count(*) AS call_count").
		Join("JOIN spans AS source_spans ON source_spans.id = span_ref.id").
		Join("JOIN services AS source_service ON source_service.id = source_spans.service_id").
		Join("JOIN spans AS child_spans ON child_spans.id = span_ref.child_span_id").
		Join("JOIN services AS child_service ON child_service.id = source_spans.service_id").
		Group("source_spans.service_id").
		Group("source_service.service_name").
		Group("child_spans.service_id").
		Group("child_service.service_name").
		Select(&ret)

	return ret, err
}
