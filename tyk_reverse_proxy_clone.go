// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// HTTP reverse proxy handler

package main

import (
	"bytes"
	"github.com/Sirupsen/logrus"
	"github.com/gorilla/context"
	"github.com/gorilla/websocket"
	"github.com/pmylund/go-cache"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ServiceCache *cache.Cache

func GetURLFromService(spec *APISpec) (interface{}, error) {
	sd := ServiceDiscovery{}
	sd.New(&spec.Proxy.ServiceDiscovery)
	data, err := sd.GetTarget(spec.Proxy.ServiceDiscovery.QueryEndpoint)

	return data, err
}

func EnsureTransport(host string) string {
	if strings.HasPrefix(host, "https://") {
		return host
	}

	if strings.HasPrefix(host, "http://") {
		return host
	}

	// no prototcol, assum ehttp
	host = "http://" + host
	return host
}

func GetNextTarget(targetData interface{}, spec *APISpec, tryCount int) string {
	if spec.Proxy.EnableLoadBalancing {
		log.Debug("[PROXY] [LOAD BALANCING] Load balancer enabled, getting upstream target")
		// Use a list
		spec.RoundRobin.SetMax(targetData)
		td := *targetData.(*[]string)

		pos := spec.RoundRobin.GetPos()
		if pos > (len(td) - 1) {
			// problem
			spec.RoundRobin.SetMax(td)
			pos = 0
		}

		thisHost := EnsureTransport(td[pos])

		// Check hosts against uptime tests
		if spec.Proxy.CheckHostAgainstUptimeTests {
			if !GlobalHostChecker.IsHostDown(thisHost) {
				// Don't overdo it
				if tryCount < len(td) {
					// Host is down, skip
					return GetNextTarget(targetData, spec, tryCount+1)
				}
				log.Error("[PROXY] [LOAD BALANCING] All hosts seem to be down, all uptime tests are failing!")
			}
		}

		return thisHost
	}
	// Use standard target - might still be service data
	log.Debug("TARGET DATA:", targetData)
	return EnsureTransport(targetData.(string))
}

// TykNewSingleHostReverseProxy returns a new ReverseProxy that rewrites
// URLs to the scheme, host, and base path provided in target. If the
// target's path is "/base" and the incoming request was for "/dir",
// the target request will be for /base/dir. This version modifies the
// stdlib version by also setting the host to the target, this allows
// us to work with heroku and other such providers
func TykNewSingleHostReverseProxy(target *url.URL, spec *APISpec) *ReverseProxy {
	// initalise round robin
	spec.RoundRobin = &RoundRobin{}
	spec.RoundRobin.SetMax(&[]string{})

	if spec.Proxy.ServiceDiscovery.UseDiscoveryService {
		log.Debug("[PROXY] Service discovery enabled")
		if ServiceCache == nil {
			log.Debug("[PROXY] Service cache initialising")
			expiry := 120
			if config.ServiceDiscovery.DefaultCacheTimeout > 0 {
				expiry = config.ServiceDiscovery.DefaultCacheTimeout
			}
			ServiceCache = cache.New(time.Duration(expiry)*time.Second, 15*time.Second)
		}
	}

	targetQuery := target.RawQuery
	director := func(req *http.Request) {
		var targetSet bool
		if spec.Proxy.ServiceDiscovery.UseDiscoveryService {
			tempTargetURL, tErr := GetURLFromService(spec)
			if tErr != nil {
				log.Error("[PROXY] [SERVICE DISCOVERY] Failed target lookup: ", tErr)
			} else {
				// No error, replace the target
				if spec.Proxy.EnableLoadBalancing {
					var targetPtr *[]string = tempTargetURL.(*[]string)
					remote, err := url.Parse(GetNextTarget(targetPtr, spec, 0))
					if err != nil {
						log.Error("[PROXY] [SERVICE DISCOVERY] Couldn't parse target URL:", err)
					} else {
						// Only replace target if everything is OK
						target = remote
						targetQuery = target.RawQuery
					}
				} else {
					var targetPtr string = tempTargetURL.(string)
					remote, err := url.Parse(GetNextTarget(targetPtr, spec, 0))
					if err != nil {
						log.Error("[PROXY] [SERVICE DISCOVERY] Couldn't parse target URL:", err)
					} else {
						// Only replace target if everything is OK
						target = remote
						targetQuery = target.RawQuery
					}
				}
			}
			// We've overriden remote now, don;t need to do it again
			targetSet = true
		}

		if !targetSet {
			// no override, better check if LB is enabled
			if spec.Proxy.EnableLoadBalancing {
				// it is, lets get that target data
				lbRemote, lbErr := url.Parse(GetNextTarget(&spec.Proxy.TargetList, spec, 0))
				if lbErr != nil {
					log.Error("[PROXY] [LOAD BALANCING] Couldn't parse target URL:", lbErr)
				} else {
					// Only replace target if everything is OK
					target = lbRemote
					targetQuery = target.RawQuery
				}
			}
		}

		// No override, and no load balancing? Use the existing target
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = singleJoiningSlash(target.Path, req.URL.Path)
		if !spec.Proxy.PreserveHostHeader {
			req.Host = target.Host
		}
		if targetQuery == "" || req.URL.RawQuery == "" {
			req.URL.RawQuery = targetQuery + req.URL.RawQuery
		} else {
			req.URL.RawQuery = targetQuery + "&" + req.URL.RawQuery
		}
	}

	return &ReverseProxy{Director: director, TykAPISpec: spec, FlushInterval: time.Duration(config.HttpServerOptions.FlushInterval) * time.Millisecond}
}

// onExitFlushLoop is a callback set by tests to detect the state of the
// flushLoop() goroutine.
var onExitFlushLoop func()

// ReverseProxy is an HTTP Handler that takes an incoming request and
// sends it to another server, proxying the response back to the
// client.
type ReverseProxy struct {
	// Director must be a function which modifies
	// the request into a new request to be sent
	// using Transport. Its response is then copied
	// back to the original client unmodified.
	Director func(*http.Request)

	// The transport used to perform proxy requests.
	// If nil, http.DefaultTransport is used.
	Transport http.RoundTripper

	// FlushInterval specifies the flush interval
	// to flush to the client while copying the
	// response body.
	// If zero, no periodic flushing is done.
	FlushInterval time.Duration

	TykAPISpec      *APISpec
	ErrorHandler    ErrorHandler
	ResponseHandler ResponseChain
}

var TykDefaultTransport http.RoundTripper = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	Dial: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).Dial,
	TLSHandshakeTimeout: 10 * time.Second,
}

func GetTransport(timeOut int) http.RoundTripper {
	if timeOut > 0 {
		log.Debug("Setting timeout for outbound request to: ", timeOut)
		var ModifiedTransport http.RoundTripper = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			Dial: (&net.Dialer{
				Timeout:   time.Duration(timeOut) * time.Second,
				KeepAlive: 30 * time.Second,
			}).Dial,
			ResponseHeaderTimeout: time.Duration(timeOut) * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
		}

		return ModifiedTransport

	}
	return TykDefaultTransport
}

func cleanSlashes(a string) string {
	endSlash := strings.HasSuffix(a, "//")
	startSlash := strings.HasPrefix(a, "//")

	if startSlash {
		a = "/" + strings.TrimPrefix(a, "//")
	}

	if endSlash {
		a = strings.TrimSuffix(a, "//") + "/"
	}

	return a
}

func singleJoiningSlash(a, b string) string {
	a = cleanSlashes(a)
	b = cleanSlashes(b)

	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")

	switch {
	case aslash && bslash:
		log.Debug(a + b)
		return a + b[1:]
	case !aslash && !bslash:
		if len(b) > 0 {
			log.Debug(a + b)
			return a + "/" + b
		} else {
			log.Debug(a + b)
			return a
		}

	}
	log.Debug(a + b)
	return a + b
}

// NewSingleHostReverseProxy returns a new ReverseProxy that rewrites
// URLs to the scheme, host, and base path provided in target. If the
// target's path is "/base" and the incoming request was for "/dir",
// the target request will be for /base/dir.
func NewSingleHostReverseProxy(target *url.URL) *ReverseProxy {
	targetQuery := target.RawQuery
	director := func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = singleJoiningSlash(target.Path, req.URL.Path)
		if targetQuery == "" || req.URL.RawQuery == "" {
			req.URL.RawQuery = targetQuery + req.URL.RawQuery
		} else {
			req.URL.RawQuery = targetQuery + "&" + req.URL.RawQuery
		}
	}
	return &ReverseProxy{Director: director, FlushInterval: 1000 * time.Millisecond}
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// Hop-by-hop headers. These are removed when sent to the backend.
// http://www.w3.org/Protocols/rfc2616/rfc2616-sec13.html
var hopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te", // canonicalized version of "TE"
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
}

func (p *ReverseProxy) ReturnRequestServeHttp(rw http.ResponseWriter, req *http.Request) *http.Request {
	outreq := new(http.Request)

	p.ServeHTTP(rw, req)

	return outreq
}

func (p *ReverseProxy) New(c interface{}, spec *APISpec) (TykResponseHandler, error) {
	p.ErrorHandler = ErrorHandler{TykMiddleware: &TykMiddleware{spec, p}}
	return nil, nil
}

func (p *ReverseProxy) ServeHTTP(rw http.ResponseWriter, req *http.Request) *http.Response {
	return p.WrappedServeHTTP(rw, req, RecordDetail(req))
	// return nil
}

func (p *ReverseProxy) ServeHTTPForCache(rw http.ResponseWriter, req *http.Request) *http.Response {
	return p.WrappedServeHTTP(rw, req, true)
}

func (p *ReverseProxy) CheckHardTimeoutEnforced(spec *APISpec, req *http.Request) (bool, int) {
	var stat RequestStatus
	var meta interface{}
	var found bool

	_, versionPaths, _, _ := spec.GetVersionData(req)
	found, meta = spec.CheckSpecMatchesStatus(req.URL.Path, req.Method, versionPaths, HardTimeout)
	if found {
		stat = StatusHardTimeout
	}

	if stat == StatusHardTimeout {
		thisMeta := meta.(*int)
		log.Debug("HARD TIMEOUT ENFORCED: ", *thisMeta)
		return true, *thisMeta
	}

	return false, 0
}

func (p *ReverseProxy) CheckCircuitBreakerEnforced(spec *APISpec, req *http.Request) (bool, *ExtendedCircuitBreakerMeta) {
	var stat RequestStatus
	var meta interface{}
	var found bool

	_, versionPaths, _, _ := spec.GetVersionData(req)
	found, meta = spec.CheckSpecMatchesStatus(req.URL.Path, req.Method, versionPaths, CircuitBreaker)
	if found {
		stat = StatusCircuitBreaker
	}

	if stat == StatusCircuitBreaker {
		thisMeta := meta.(*ExtendedCircuitBreakerMeta)
		log.Debug("CB Enforced for path: ", *thisMeta)
		return true, thisMeta
	}

	return false, nil
}

func (p *ReverseProxy) isWebSocketRequest(req *http.Request) bool {
	connection := strings.ToLower(strings.TrimSpace(req.Header.Get("Connection")))
	if connection != "upgrade" {
		return false
	}

	upgrade := strings.ToLower(strings.TrimSpace(req.Header.Get("Upgrade")))
	if upgrade != "websocket" {
		return false
	}
	return true
}


func canonicalAddr(url *url.URL) string {
	addr := url.Host
	// If the addr has a port number attached
	if !(strings.LastIndex(addr, ":") > strings.LastIndex(addr, "]")) {
		return addr + ":80"
	}
	return addr
}

func (p *ReverseProxy) Dial(tripper http.RoundTripper, rw http.ResponseWriter, req *http.Request, isWebSocket bool) (*http.Response, error) {
	if !isWebSocket {
		return tripper.RoundTrip(req)
	}

	transport := tripper.(*http.Transport)
	url := req.URL

	// Apply proxy settings if exists
	if transport.Proxy != nil {
		var err error
		url, err = transport.Proxy(req)
		if err != nil {
			return nil, err
		}
	}

	// TODO: Support wss:// currently websocket are proxed as ws://
	target := canonicalAddr(url)
	dialConn, err := transport.Dial("tcp", target)
	if err != nil {
		return nil, err
	}

	err = req.Write(dialConn)
	if err != nil {
		return nil, err
	}

	jack, ok := rw.(http.Hijacker)
	if !ok {
		return nil, err
	}

	rwConn, _, err := jack.Hijack()
	if err != nil {
		return nil, err
	}

	done := make(chan error, 2)
	copy := func(dst io.Writer, src io.Reader) {
		_, err := io.Copy(dst, src)
		done <- err
	}
	go copy(dialConn, rwConn)
	go copy(rwConn, dialConn)
	// Wait until someone hangs up which results in an error from io.Copy()
	<-done

	return nil, nil
}


func (p *ReverseProxy) WrappedServeHTTP(rw http.ResponseWriter, req *http.Request, withCache bool) *http.Response {
	transport := p.Transport
	if transport == nil {
		// 1. Check if timeouts are set for this endpoint
		_, timeout := p.CheckHardTimeoutEnforced(p.TykAPISpec, req)
		transport = GetTransport(timeout)
	}

	// Do this before we make a shallow copy
	sessVal := context.Get(req, SessionData)

	outreq := new(http.Request)
	logreq := new(http.Request)
	log.Debug("UPSTREAM REQUEST URL: ", req.URL)
	*outreq = *req // includes shallow copies of maps, but okay
	*logreq = *req

	log.Debug("Outbound Request: ", outreq.URL.String())
	p.Director(outreq)
	outreq.Proto = "HTTP/1.1"
	outreq.ProtoMajor = 1
	outreq.ProtoMinor = 1
	outreq.Close = false

	//isWebSocket := p.isWebSocketRequest(req)

	// Remove hop-by-hop headers to the backend.  Especially
	// important is "Connection" because we want a persistent
	// connection, regardless of what the client sent to us.  This
	// is modifying the same underlying map from req (shallow
	// copied above) so we only copy it if necessary.
	copiedHeaders := false
	for _, h := range hopHeaders {
		if outreq.Header.Get(h) != "" {
			if !copiedHeaders {
				outreq.Header = make(http.Header)
				logreq.Header = make(http.Header)
				copyHeader(outreq.Header, req.Header)
				copyHeader(logreq.Header, req.Header)
				copiedHeaders = true
			}
			outreq.Header.Del(h)
			logreq.Header.Del(h)
		}
	}

	var thisIP string
	if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		// If we aren't the first proxy retain prior
		// X-Forwarded-For information as a comma+space
		// separated list and fold multiple headers into one.
		if prior, ok := outreq.Header["X-Forwarded-For"]; ok {
			clientIP = strings.Join(prior, ", ") + ", " + clientIP
		}
		outreq.Header.Set("X-Forwarded-For", clientIP)
		thisIP = clientIP
	}

	isWebSocket := websocket.IsWebSocketUpgrade(req)

	// Circuit breaker
	breakerEnforced, breakerConf := p.CheckCircuitBreakerEnforced(p.TykAPISpec, req)
	// TODO:
	// 1. If the circuit breaker is active - wrap the RoundTrip call with a breaker function
	// 2. when we init the APISpec, we need to create CBs for each monitored endpoint, this means extending the APISpec so we can store pointers
	// 3. Set up monitoring functions and hook them up to the event handler

	var res *http.Response
	var err error
	if breakerEnforced {
		log.Debug("ON REQUEST: Breaker status: ", breakerConf.CB.Ready())
		if breakerConf.CB.Ready() {
			res, err = p.Dial(transport, rw, outreq, isWebSocket)
			if err != nil {
				breakerConf.CB.Fail()
			} else if res.StatusCode == 500 {
				breakerConf.CB.Fail()
			} else {
				breakerConf.CB.Success()
			}
		} else {
			p.ErrorHandler.HandleError(rw, logreq, "Service temporarily unnavailable.", 503)
			return nil
		}
	} else {
		res, err = p.Dial(transport, rw, outreq, isWebSocket)
	}

	if err != nil {

		authHeaderValue := context.Get(req, AuthHeaderValue).(string)
		var obfuscated string
		if len(authHeaderValue) > 4 {
			obfuscated = "****" + authHeaderValue[len(authHeaderValue)-4:]
		}

		var alias string
		if sessVal != nil {
			alias = sessVal.(SessionState).Alias
		}

		log.WithFields(logrus.Fields{
			"prefix":      "proxy",
			"user_ip":     thisIP,
			"server_name": outreq.Host,
			"user_id":     obfuscated,
			"user_name":   alias,
			"org_id":      p.TykAPISpec.APIDefinition.OrgID,
			"api_id":      p.TykAPISpec.APIDefinition.APIID,
		}).Error("http: proxy error: ", err)

		if strings.Contains(err.Error(), "timeout awaiting response headers") {
			p.ErrorHandler.HandleError(rw, logreq, "Upstream service reached hard timeout.", 408)

			if p.TykAPISpec.Proxy.ServiceDiscovery.UseDiscoveryService {
				if ServiceCache != nil {
					log.Debug("[PROXY] [SERVICE DISCOVERY] Upstream host failed, refreshing host list")
					ServiceCache.Delete(p.TykAPISpec.APIID)
				}
			}
			return nil
		}
		if strings.Contains(err.Error(), "no such host") {
			p.ErrorHandler.HandleError(rw, logreq, "Upstream host lookup failed", 500)
			return nil
		}

		p.ErrorHandler.HandleError(rw, logreq, "There was a problem proxying the request", 500)
		return nil

	}

	if isWebSocket {
		return nil
	}

	inres := new(http.Response)
	if withCache {
		*inres = *res // includes shallow copies of maps, but okay

		defer res.Body.Close()

		// Buffer body data
		var bodyBuffer bytes.Buffer
		bodyBuffer2 := new(bytes.Buffer)

		p.CopyResponse(&bodyBuffer, res.Body)
		*bodyBuffer2 = bodyBuffer

		// Create new ReadClosers so we can split output
		res.Body = ioutil.NopCloser(&bodyBuffer)
		inres.Body = ioutil.NopCloser(bodyBuffer2)
	}

	ses := SessionState{}
	if sessVal != nil {
		ses = sessVal.(SessionState)
	}

	// Middleware chain handling here - very simple, but should do the trick
	chainErr := p.ResponseHandler.Go(p.TykAPISpec.ResponseChain, rw, res, req, &ses)
	if chainErr != nil {
		log.Error("Response chain failed! ", chainErr)
	}

	// We should at least copy the status code in
	inres.StatusCode = res.StatusCode
	inres.ContentLength = res.ContentLength
	p.HandleResponse(rw, res, req, &ses)
	return inres
}

func (p *ReverseProxy) HandleResponse(rw http.ResponseWriter, res *http.Response, req *http.Request, ses *SessionState) error {

	for _, h := range hopHeaders {
		res.Header.Del(h)
	}
	defer res.Body.Close()

	// Close connections
	if config.CloseConnections {
		res.Header.Set("Connection", "close")
	}

	// Add resource headers
	if ses != nil {
		// We have found a session, lets report back
		res.Header.Add("X-RateLimit-Limit", strconv.Itoa(int(ses.QuotaMax)))
		res.Header.Add("X-RateLimit-Remaining", strconv.Itoa(int(ses.QuotaRemaining)))
		res.Header.Add("X-RateLimit-Reset", strconv.Itoa(int(ses.QuotaRenews)))
	}

	copyHeader(rw.Header(), res.Header)

	rw.WriteHeader(res.StatusCode)
	p.CopyResponse(rw, res.Body)
	return nil
}

func (p *ReverseProxy) CopyResponse(dst io.Writer, src io.Reader) {
	if p.FlushInterval != 0 {
		if wf, ok := dst.(writeFlusher); ok {
			mlw := &maxLatencyWriter{
				dst:     wf,
				latency: p.FlushInterval,
				done:    make(chan bool),
			}
			go mlw.flushLoop()
			defer mlw.stop()
			dst = mlw
		}
	}

	io.Copy(dst, src)
}

type writeFlusher interface {
	io.Writer
	http.Flusher
}

type maxLatencyWriter struct {
	dst     writeFlusher
	latency time.Duration

	lk   sync.Mutex // protects Write + Flush
	done chan bool
}

func (m *maxLatencyWriter) Write(p []byte) (int, error) {
	m.lk.Lock()
	defer m.lk.Unlock()
	return m.dst.Write(p)
}

func (m *maxLatencyWriter) flushLoop() {
	t := time.NewTicker(m.latency)
	defer t.Stop()
	for {
		select {
		case <-m.done:
			if onExitFlushLoop != nil {
				onExitFlushLoop()
			}
			return
		case <-t.C:
			m.lk.Lock()
			m.dst.Flush()
			m.lk.Unlock()
		}
	}
}

func (m *maxLatencyWriter) stop() { m.done <- true }
