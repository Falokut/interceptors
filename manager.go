package interceptors

import (
	"context"
	"net/http"
	"time"

	"github.com/Falokut/grpc_errors"
	"github.com/felixge/httpsnoop"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type Metrics interface {
	IncHits(status int, method, path string)
	ObserveResponseTime(status int, method, path string, observeTime float64)
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
	reply, err := handler(ctx, req)
	im.logger.Infof("Method: %s, Time: %v, Metadata: %v, Err: %v", info.FullMethod, time.Since(start), md, err)

	return reply, err
}

func (im *InterceptorManager) StreamLogger(srv any, ss grpc.ServerStream,
	info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	start := time.Now()
	md, _ := metadata.FromIncomingContext(ss.Context())
	err := handler(srv, ss)
	im.logger.Infof("Streaming Method: %s, Time: %v, Metadata: %v, Err: %v", info.FullMethod, time.Since(start), md, err)
	return err
}

func (im *InterceptorManager) Metrics(ctx context.Context, req interface{},
	info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	start := time.Now()
	resp, err := handler(ctx, req)
	var status = http.StatusOK
	if err != nil {
		status = grpc_errors.ConvertGrpcCodeIntoHTTP(grpc_errors.GetGrpcCode(err))
	}
	im.metr.ObserveResponseTime(status, info.FullMethod, info.FullMethod, time.Since(start).Seconds())
	im.metr.IncHits(status, info.FullMethod, info.FullMethod)

	return resp, err
}

func (im *InterceptorManager) StreamMetrics(srv any, ss grpc.ServerStream,
	info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	start := time.Now()
	var status = http.StatusOK
	err := handler(srv, ss)
	if err != nil {
		status = grpc_errors.ConvertGrpcCodeIntoHTTP(grpc_errors.GetGrpcCode(err))
	}
	im.metr.ObserveResponseTime(status, info.FullMethod, info.FullMethod, time.Since(start).Seconds())
	im.metr.IncHits(status, info.FullMethod, info.FullMethod)
	return err
}

func (im *InterceptorManager) RestLogger(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := httpsnoop.CaptureMetrics(handler, w, r)
		im.logger.Infof("Method: %s, Path: %s, Time: %v, status code: %v", r.Method, r.URL.Path,
			m.Duration, m.Code)
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
