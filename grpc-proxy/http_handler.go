package grpc_proxy

import (
	"fmt"
	"github.com/bradleyjkemp/grpc-tools/internal/marker"
	"github.com/improbable-eng/grpc-web/go/grpcweb"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strings"
)

func newHttpServer(grpcHandler *grpcweb.WrappedGrpcServer, internalRedirect func(net.Conn, string)) *http.Server {
	s := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS != nil {
				// Knowing whether this came in over HTTP or HTTPS
				// is important for being able to replay the request.
				// This adds a Forwarded header with the protocol information.
				marker.AddHTTPSMarker(r.Header)
			}

			switch {
			case r.Method == http.MethodConnect:
				handleConnect(w, r, internalRedirect)
			case isGrpcRequest(grpcHandler, r):
				grpcHandler.ServeHTTP(w, r)
			default:
				// Many clients use a mix of gRPC and non-gRPC requests
				// so must try to be as transparent as possible for normal
				// HTTP requests by proxying the request to the original destination.
				httpReverseProxy.ServeHTTP(w, r)
			}
		}),
	}

	s.Handler = h2c.NewHandler(s.Handler, &http2.Server{}) // Adds support for unencrypted HTTP2

	return s
}

func handleConnect(w http.ResponseWriter, r *http.Request, internalRedirect func(net.Conn, string)) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	clientConn.Write([]byte("HTTP/1.0 200 OK\r\n\r\n"))
	var conn tlsMuxConn
	if conn, ok = clientConn.(tlsMuxConn); !ok {
		fmt.Fprintf(os.Stderr, "Err: unknown connection type: %v\n", clientConn)
		clientConn.Close()
		return
	}

	internalRedirect(conn, r.Host)
}

var httpReverseProxy = &httputil.ReverseProxy{
	Director: func(request *http.Request) {
		// Because of the TLSmux used to server HTTP and HTTPS on the same port
		// we have to rely on the Forwarded header (added by middleware) to
		// tell which protocol to use for proxying.
		// (we could always set HTTP but would mean relying on the upstream
		// properly redirecting HTTP->HTTPS)
		if marker.IsTLSRequest(request.Header) {
			request.URL.Scheme = "https"
		} else {
			request.URL.Scheme = "http"
		}
		request.URL.Host = request.Host
	},
}

func isGrpcRequest(server *grpcweb.WrappedGrpcServer, r *http.Request) bool {
	return server.IsAcceptableGrpcCorsRequest(r) || // CORS request from browser
		server.IsGrpcWebRequest(r) || // gRPC-Web request from browser
		r.ProtoMajor == 2 && strings.Contains(r.Header.Get("Content-Type"), "application/grpc") // Standard gRPC request
}