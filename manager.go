package interceptors

import (
	"context"
	"fmt"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type Metrics interface {
	IncHits(status int, method, path string)
	ObserveResponseTime(status int, method, path string, observeTime float64)
	IncRestPanicsTotal()
	IncGrpcPanicsTotal()
}

// InterceptorManager
type InterceptorManager struct {
	logger *logrus.Logger
	metr   Metrics
}

// InterceptorManager constructor
func NewInterceptorManager(logger *logrus.Logger, metr Metrics) *InterceptorManager {
	return &InterceptorManager{logger: logger, metr: metr}
}

// Logger Interceptor
func (im *InterceptorManager) Logger(ctx context.Context, req interface{},
	info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
	start := time.Now()
	md, _ := metadata.FromIncomingContext(ctx)
	resp, err = handler(ctx, req)

	deadline, ok := ctx.Deadline()
	formattedDeadline := ""
	if ok {
		formattedDeadline = deadline.Format(time.RFC3339)
	}
	im.logger.WithFields(logrus.Fields{
		"grpc.method":           info.FullMethod,
		"grpc.start_time":       start.Format(time.RFC3339),
		"grpc.request.deadline": formattedDeadline,
		"grpc.request.metadata": md,
		"grpc.time_ms":          time.Since(start).Milliseconds(),
		"grpc.code":             status.Code(err),
	}).Info("finished grpc unary call")

	return
}

func (im *InterceptorManager) StreamLogger(srv any, ss grpc.ServerStream,
	info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	start := time.Now()
	md, _ := metadata.FromIncomingContext(ss.Context())
	err := handler(srv, ss)
	deadline, ok := ss.Context().Deadline()
	formattedDeadline := ""
	if ok {
		formattedDeadline = deadline.Format(time.RFC3339)
	}
	im.logger.WithFields(logrus.Fields{
		"grpc.method":                          info.FullMethod,
		"grpc.request.stream.is_client_stream": info.IsClientStream,
		"grpc.request.stream.is_server_stream": info.IsServerStream,
		"grpc.start_time":                      start.Format(time.RFC3339),
		"grpc.request.deadline":                formattedDeadline,
		"grpc.request.metadata":                md,
		"grpc.time_ms":                         time.Since(start).Milliseconds(),
		"grpc.code":                            status.Code(err),
	}).Info("finished grpc streaming call")

	return err
}

func (im *InterceptorManager) Metrics(ctx context.Context, req interface{},
	info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	start := time.Now()
	resp, err := handler(ctx, req)
	status := ConvertGrpcCodeIntoHTTP(status.Code(err))
	im.metr.ObserveResponseTime(status, info.FullMethod, info.FullMethod, time.Since(start).Seconds())
	im.metr.IncHits(status, info.FullMethod, info.FullMethod)

	return resp, err
}

func (im *InterceptorManager) StreamMetrics(srv any, ss grpc.ServerStream,
	info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	start := time.Now()
	err := handler(srv, ss)
	status := ConvertGrpcCodeIntoHTTP(status.Code(err))

	im.metr.ObserveResponseTime(status, info.FullMethod, info.FullMethod, time.Since(start).Seconds())
	im.metr.IncHits(status, info.FullMethod, info.FullMethod)
	return err
}

func (im *InterceptorManager) RestLogger(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := httpsnoop.CaptureMetrics(handler, w, r)
		start := time.Now()
		deadline, ok := r.Context().Deadline()
		formattedDeadline := ""
		if ok {
			formattedDeadline = deadline.Format(time.RFC3339)
		}
		im.logger.WithFields(logrus.Fields{
			"rest.method":           r.Method,
			"rest.path":             r.URL.Path,
			"rest.start_time":       start.Format(time.RFC3339),
			"rest.request.deadline": formattedDeadline,
			"rest.time_ms":          time.Since(start).Milliseconds(),
			"rest.code":             m.Code,
		}).Info("finished rest call")
	})
}

func (im *InterceptorManager) RestMetrics(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := httpsnoop.CaptureMetrics(handler, w, r)
		status := m.Code
		im.metr.ObserveResponseTime(status, r.Method, r.URL.Path, m.Duration.Seconds())
		im.metr.IncHits(status, r.Method, r.URL.Path)
	})
}

func InjectTrace(span opentracing.Span, req **http.Request) error {
	carrier := opentracing.HTTPHeadersCarrier((*req).Header)
	return opentracing.GlobalTracer().Inject(span.Context(), opentracing.HTTPHeaders, carrier)
}

func ExtractTrace(req *http.Request) (opentracing.SpanContext, error) {
	carrier := opentracing.HTTPHeadersCarrier(req.Header)
	return opentracing.GlobalTracer().Extract(opentracing.HTTPHeaders, carrier)
}

var grpcGatewayTag = opentracing.Tag{Key: string(ext.Component), Value: "grpc-gateway"}

func (im *InterceptorManager) RestTracer(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		parentSpanContext, err := opentracing.GlobalTracer().Extract(
			opentracing.HTTPHeaders,
			opentracing.HTTPHeadersCarrier(r.Header))
		method := r.Method + " " + r.URL.Path

		if err == nil || err == opentracing.ErrSpanContextNotFound {
			serverSpan := opentracing.GlobalTracer().StartSpan(
				method,
				ext.RPCServerOption(parentSpanContext),
				grpcGatewayTag,
			)
			r = r.WithContext(opentracing.ContextWithSpan(r.Context(), serverSpan))
			defer serverSpan.Finish()
		}
		handler.ServeHTTP(w, r)
	})
}

func (im *InterceptorManager) RestPanicRecover(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			p := recover()
			if p != nil {
				http.Error(w, fmt.Sprintf("%s %v", http.StatusText(http.StatusInternalServerError), p),
					http.StatusInternalServerError)

				im.logger.WithFields(logrus.Fields{
					"panic": p,
					"stack": debug.Stack(),
				}).Error("rest panic recovered")
				im.metr.IncRestPanicsTotal()
				return
			}
		}()
		handler.ServeHTTP(w, r)
	})
}

func (im *InterceptorManager) GrpcUnaryServerPanicRecover(ctx context.Context, req any,
	info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {

	defer func() {
		p := recover()
		if p != nil {
			im.logger.WithFields(logrus.Fields{
				"panic": p,
				"stack": debug.Stack(),
			}).Error("grpc unary panic caught")

			im.metr.IncGrpcPanicsTotal()
			err = status.Error(codes.Internal, fmt.Sprint(p))
		}
	}()

	return handler(ctx, req)
}

func (im *InterceptorManager) GrpcStreamServerPanicRecover(srv any,
	stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
	defer func() {
		p := recover()
		if p != nil {
			im.logger.WithFields(logrus.Fields{
				"panic": p,
				"stack": debug.Stack(),
			}).Error("grpc stream panic caught")

			im.metr.IncGrpcPanicsTotal()
			err = status.Error(codes.Internal, fmt.Sprint(p))
		}
	}()

	return handler(srv, stream)

}

// Map GRPC errors codes to http status
func ConvertGrpcCodeIntoHTTP(code codes.Code) int {
	switch code {
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.AlreadyExists:
		return http.StatusBadRequest
	case codes.NotFound:
		return http.StatusNotFound
	case codes.Internal:
		return http.StatusInternalServerError
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.Canceled:
		return http.StatusRequestTimeout
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.OK:
		return http.StatusOK
	}
	return http.StatusInternalServerError
}
