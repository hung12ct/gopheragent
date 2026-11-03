package auditlog

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strings"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	grpcgzip "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type grpcExporter struct {
	conn   *grpc.ClientConn
	client collogspb.LogsServiceClient
	md     metadata.MD
	opts   []grpc.CallOption
}

func newGRPCExporter(_ context.Context, cfg Config, tc *tls.Config) (*grpcExporter, error) {
	creds := insecure.NewCredentials()
	if tc != nil {
		creds = credentials.NewTLS(tc)
	}
	conn, err := grpc.NewClient(cfg.Endpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("auditlog: grpc dial %s: %w", cfg.Endpoint, err)
	}
	e := &grpcExporter{conn: conn, client: collogspb.NewLogsServiceClient(conn), md: metadata.New(cfg.Headers)}
	if cfg.Gzip {
		e.opts = append(e.opts, grpc.UseCompressor(grpcgzip.Name))
	}
	return e, nil
}

func (e *grpcExporter) export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, bool, error) {
	if len(e.md) > 0 {
		ctx = metadata.NewOutgoingContext(ctx, e.md)
	}
	resp, err := e.client.Export(ctx, req, e.opts...)
	if err != nil {
		switch status.Code(err) {
		case codes.Unavailable, codes.ResourceExhausted, codes.Aborted, codes.DeadlineExceeded:
			return nil, true, err
		}
		return nil, false, err
	}
	return resp, false, nil
}

func (e *grpcExporter) close() error { return e.conn.Close() }

type httpExporter struct {
	url     string
	client  *http.Client
	headers map[string]string
	gzip    bool
}

func newHTTPExporter(cfg Config, tc *tls.Config) *httpExporter {
	scheme := "https"
	if tc == nil {
		scheme = "http"
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tc
	return &httpExporter{
		url:     scheme + "://" + cfg.Endpoint + "/v1/logs",
		client:  &http.Client{Transport: tr},
		headers: cfg.Headers,
		gzip:    cfg.Gzip,
	}
}

func (e *httpExporter) export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, bool, error) {
	body, err := proto.Marshal(req)
	if err != nil {
		return nil, false, fmt.Errorf("auditlog: marshal request: %w", err)
	}
	if e.gzip {
		if body, err = gzipBytes(body); err != nil {
			return nil, false, err
		}
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("auditlog: build request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/x-protobuf")
	if e.gzip {
		hreq.Header.Set("Content-Encoding", "gzip")
	}
	for k, v := range e.headers {
		hreq.Header.Set(k, v)
	}
	hresp, err := e.client.Do(hreq)
	if err != nil {
		return nil, true, err // network error: transient
	}
	defer hresp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(hresp.Body, 1<<20))
	if err != nil {
		return nil, true, fmt.Errorf("auditlog: read response: %w", err)
	}
	if hresp.StatusCode/100 != 2 {
		retryable := hresp.StatusCode == http.StatusTooManyRequests || hresp.StatusCode == http.StatusBadGateway ||
			hresp.StatusCode == http.StatusServiceUnavailable || hresp.StatusCode == http.StatusGatewayTimeout
		return nil, retryable, fmt.Errorf("auditlog: collector returned %s: %s", hresp.Status, strings.TrimSpace(string(raw[:min(len(raw), 256)])))
	}
	var resp collogspb.ExportLogsServiceResponse
	if err := proto.Unmarshal(raw, &resp); err != nil {
		return nil, false, fmt.Errorf("auditlog: decode response: %w", err)
	}
	return &resp, false, nil
}

func (e *httpExporter) close() error {
	e.client.CloseIdleConnections()
	return nil
}

func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		return nil, fmt.Errorf("auditlog: gzip: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("auditlog: gzip: %w", err)
	}
	return buf.Bytes(), nil
}
