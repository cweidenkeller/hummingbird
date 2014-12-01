package hummingbird

import (
	"fmt"
	"log/syslog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"sync"
	"syscall"
	"time"
)

var responseTemplate = "<html><h1>%s</h1><p>%s</p></html>"

var responseBodies = map[int]string{
	100: "",
	200: "",
	201: "",
	202: fmt.Sprintf(responseTemplate, "Accepted", "The request is accepted for processing."),
	204: "",
	206: "",
	301: fmt.Sprintf(responseTemplate, "Moved Permanently", "The resource has moved permanently."),
	302: fmt.Sprintf(responseTemplate, "Found", "The resource has moved temporarily."),
	303: fmt.Sprintf(responseTemplate, "See Other", "The response to the request can be found under a different URI."),
	304: fmt.Sprintf(responseTemplate, "Not Modified", ""),
	307: fmt.Sprintf(responseTemplate, "Temporary Redirect", "The resource has moved temporarily."),
	400: fmt.Sprintf(responseTemplate, "Bad Request", "The server could not comply with the request since it is either malformed or otherwise incorrect."),
	401: fmt.Sprintf(responseTemplate, "Unauthorized", "This server could not verify that you are authorized to access the document you requested."),
	402: fmt.Sprintf(responseTemplate, "Payment Required", "Access was denied for financial reasons."),
	403: fmt.Sprintf(responseTemplate, "Forbidden", "Access was denied to this resource."),
	404: fmt.Sprintf(responseTemplate, "Not Found", "The resource could not be found."),
	405: fmt.Sprintf(responseTemplate, "Method Not Allowed", "The method is not allowed for this resource."),
	406: fmt.Sprintf(responseTemplate, "Not Acceptable", "The resource is not available in a format acceptable to your browser."),
	408: fmt.Sprintf(responseTemplate, "Request Timeout", "The server has waited too long for the request to be sent by the client."),
	409: fmt.Sprintf(responseTemplate, "Conflict", "There was a conflict when trying to complete your request."),
	410: fmt.Sprintf(responseTemplate, "Gone", "This resource is no longer available."),
	411: fmt.Sprintf(responseTemplate, "Length Required", "Content-Length header required."),
	412: fmt.Sprintf(responseTemplate, "Precondition Failed", "A precondition for this request was not met."),
	413: fmt.Sprintf(responseTemplate, "Request Entity Too Large", "The body of your request was too large for this server."),
	414: fmt.Sprintf(responseTemplate, "Request URI Too Long", "The request URI was too long for this server."),
	415: fmt.Sprintf(responseTemplate, "Unsupported Media Type", "The request media type is not supported by this server."),
	416: fmt.Sprintf(responseTemplate, "Requested Range Not Satisfiable", "The Range requested is not available."),
	417: fmt.Sprintf(responseTemplate, "Expectation Failed", "Expectation failed."),
	422: fmt.Sprintf(responseTemplate, "Unprocessable Entity", "Unable to process the contained instructions"),
	499: fmt.Sprintf(responseTemplate, "Client Disconnect", "The client was disconnected during request."),
	500: fmt.Sprintf(responseTemplate, "Internal Error", "The server has either erred or is incapable of performing the requested operation."),
	501: fmt.Sprintf(responseTemplate, "Not Implemented", "The requested method is not implemented by this server."),
	502: fmt.Sprintf(responseTemplate, "Bad Gateway", "Bad gateway."),
	503: fmt.Sprintf(responseTemplate, "Service Unavailable", "The server is currently unavailable. Please try again at a later time."),
	504: fmt.Sprintf(responseTemplate, "Gateway Timeout", "A timeout has occurred speaking to a backend server."),
	507: fmt.Sprintf(responseTemplate, "Insufficient Storage", "There was not enough space to save the resource."),
}

// ResponseWriter that saves its status - used for logging.

type WebWriter struct {
	http.ResponseWriter
	Status int
}

func (w *WebWriter) WriteHeader(status int) {
	w.ResponseWriter.WriteHeader(status)
	w.Status = status
}

func (w *WebWriter) CopyResponseHeaders(src *http.Response) {
	for key := range src.Header {
		w.Header().Set(key, src.Header.Get(key))
	}
}

func (w *WebWriter) StandardResponse(statusCode int) {
	w.WriteHeader(statusCode)
	body := responseBodies[statusCode]
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Content-Length", strconv.FormatInt(int64(len(body)), 10))
	w.Write([]byte(body))
}

// http.Request that also contains swift-specific info about the request

type WebRequest struct {
	*http.Request
	TransactionId string
	XTimestamp    string
	Start         time.Time
	Logger        *syslog.Writer
}

func (r *WebRequest) CopyRequestHeaders(dst *http.Request) {
	for key := range r.Header {
		dst.Header.Set(key, r.Header.Get(key))
	}
	dst.Header.Set("X-Timestamp", r.XTimestamp)
	dst.Header.Set("X-Trans-Id", r.TransactionId)
}

func (r *WebRequest) NillableFormValue(key string) *string {
	if r.Form == nil {
		r.ParseForm()
	}
	if vs, ok := r.Form[key]; !ok {
		return nil
	} else {
		return &vs[0]
	}
}

func (r WebRequest) LogError(format string, args ...interface{}) {
	r.Logger.Err(fmt.Sprintf(format, args...) + " (txn:" + r.TransactionId + ")")
}

func (r WebRequest) LogInfo(format string, args ...interface{}) {
	r.Logger.Info(fmt.Sprintf(format, args...) + " (txn:" + r.TransactionId + ")")
}

func (r WebRequest) LogDebug(format string, args ...interface{}) {
	r.Logger.Debug(fmt.Sprintf(format, args...) + " (txn:" + r.TransactionId + ")")
}

func (r WebRequest) LogPanics() {
	if e := recover(); e != nil {
		r.Logger.Err(fmt.Sprintf("PANIC: %s: %s", e, debug.Stack()) + " (txn:" + r.TransactionId + ")")
	}
}

type LoggingContext interface {
	LogError(format string, args ...interface{})
	LogInfo(format string, args ...interface{})
	LogDebug(format string, args ...interface{})
}

/* http.Server that knows how to shut down gracefully */

type HummingbirdServer struct {
	http.Server
	Listener net.Listener
	wg       sync.WaitGroup
}

func (srv *HummingbirdServer) ConnStateChange(conn net.Conn, state http.ConnState) {
	if state == http.StateNew {
		srv.wg.Add(1)
	} else if state == http.StateClosed {
		srv.wg.Done()
	}
}

func (srv *HummingbirdServer) BeginShutdown() {
	srv.SetKeepAlivesEnabled(false)
	srv.Listener.Close()
}

func (srv *HummingbirdServer) Wait() {
	srv.wg.Wait()
}

/*
	SIGHUP - graceful restart
	SIGINT - graceful shutdown
	SIGTERM, SIGQUIT - immediate shutdown

	Graceful shutdown/restart gives any open connections 5 minutes to complete, then exits.
*/
func RunServers(configFile string, GetServer func(string) (string, int, http.Handler)) {
	var servers []*HummingbirdServer
	configFiles, err := filepath.Glob(fmt.Sprintf("%s/*.conf", configFile))
	if err != nil || len(configFiles) <= 0 {
		configFiles = []string{configFile}
	}
	for _, configFile := range configFiles {
		ip, port, handler := GetServer(configFile)
		sock, err := net.Listen("tcp", fmt.Sprintf("%s:%d", ip, port))
		if err != nil {
			panic("Error listening on socket!")
		}
		srv := HummingbirdServer{}
		srv.Handler = handler
		srv.ConnState = srv.ConnStateChange
		srv.Listener = sock
		go srv.Serve(sock)
		servers = append(servers, &srv)
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)
	s := <-c
	if s == syscall.SIGINT {
		for _, srv := range servers {
			srv.BeginShutdown()
		}
		go func() {
			time.Sleep(time.Minute * 5)
			os.Exit(0)
		}()
		for _, srv := range servers {
			srv.Wait()
			time.Sleep(time.Second * 5)
		}
	}
	os.Exit(0)
}
