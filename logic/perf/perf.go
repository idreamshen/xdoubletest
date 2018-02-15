package perf

import (
	"bytes"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptrace"
	"xframe/log"
	"xframe/utils"
)

type Perf interface {
	//start performance task
	Start() (interface{}, error)
	//stop performance task
	Stop() error
}

type DefaultPerf struct {
	Id      string
	Number  uint32
	Cc      int
	Qps     int
	Method  string
	Url     string
	Body    []byte
	stopChs []chan struct{}
	results []chan Result
}

func initDefaultPerf(number uint32, cc int, qps int, method string, url string, body []byte) *DefaultPerf {
	this := new(DefaultPerf)
	this.Id = utils.NewUUIDV4().String()
	this.Number = number
	this.Cc = cc
	this.Qps = qps
	this.Method = method
	this.Url = url
	this.Body = body
	this.stopCh = make([]chan struct{}, this.Cc)
	this.results = make([]chan Result, this.Number)
}

func (this *DefaultPerf) makeRequest() (http.Request, error) {
	if this.Method == "GET" {
		return http.NewRequest(this.Method, this.Url, nil)
	} else if this.Method == "POST" {
		buf := bytes.NewBuffer(this.Body)
		return http.NewRequest(this.Method, this.Url, buf)
	}
}

func (this *DefaultPerf) DoRequest(client http.Client) {
	s := time.Now()
	var size int64
	var code int
	var dnsStart, connStart, resStart, reqStart, delayStart time.Time
	var dnsDuration, connDuration, resDuration, reqDuration, delayDuration time.Duration
	req := this.makeRequest()
	trace := &httptrace.ClientTrace{
		DNSStart: func(info httptrace.DNSStartInfo) {
			dnsStart = time.Now()
		},
		DNSDone: func(dnsInfo httptrace.DNSDoneInfo) {
			dnsDuration = time.Now().Sub(dnsStart)
		},
		GetConn: func(h string) {
			connStart = time.Now()
		},
		GotConn: func(connInfo httptrace.GotConnInfo) {
			connDuration = time.Now().Sub(connStart)
			reqStart = time.Now()
		},
		WroteRequest: func(w httptrace.WroteRequestInfo) {
			reqDuration = time.Now().Sub(reqStart)
			delayStart = time.Now()
		},
		GotFirstResponseByte: func() {
			delayDuration = time.Now().Sub(delayStart)
			resStart = time.Now()
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := client.Do(req)
	if err == nil {
		size = resp.ContentLength
		code = resp.StatusCode
		io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
	}
	t := time.Now()
	resDuration = t.Sub(resStart)
	finish := t.Sub(s)
	this.results <- Result{
		statusCode:    code,
		duration:      finish,
		err:           err,
		contentLength: size,
		connDuration:  connDuration,
		dnsDuration:   dnsDuration,
		reqDuration:   reqDuration,
		resDuration:   resDuration,
		delayDuration: delayDuration,
	}
}

func (this *DefaultPerf) runWorker(n uint32, stopCh chan struct{}) {
	var counter uint32
	tick := time.Tick(time.Duration(1000/this.Qps) * time.MilliSecond)
	cli := http.Client{}
	for {
		select {
		case <-tick:
			couter++
			if counter == n {
				return
			}
			go this.DoRequest(cli)
		case <-stopCh:
			log.DEBUG("receive stop signal")
			return
		}
	}
}

func (this *DefaultPerf) Report() interface{} {
	var r Report
	for res := range this.results {
		if res.err != nil {
			r.errorDist[res.err.Error()]++
		} else {
			r.lats = append(r.lats, res.duration.Seconds())
			r.avgTotal += res.duration.Seconds()
			r.avgConn += res.connDuration.Seconds()
			r.avgDelay += res.delayDuration.Seconds()
			r.avgDns += res.dnsDuration.Seconds()
			r.avgReq += res.reqDuration.Seconds()
			r.avgRes += res.resDuration.Seconds()
			r.connLats = append(r.connLats, res.connDuration.Seconds())
			r.dnsLats = append(r.dnsLats, res.dnsDuration.Seconds())
			r.reqLats = append(r.reqLats, res.reqDuration.Seconds())
			r.delayLats = append(r.delayLats, res.delayDuration.Seconds())
			r.resLats = append(r.resLats, res.resDuration.Seconds())
			r.statusCodeDist[res.statusCode]++
			if res.contentLength > 0 {
				r.sizeTotal += res.contentLength
			}
		}
	}
	r.rps = float64(len(r.lats)) / r.total.Seconds()
	r.average = r.avgTotal / float64(len(r.lats))
	r.avgConn = r.avgConn / float64(len(r.lats))
	r.avgDelay = r.avgDelay / float64(len(r.lats))
	r.avgDns = r.avgDns / float64(len(r.lats))
	r.avgReq = r.avgReq / float64(len(r.lats))
	r.avgRes = r.avgRes / float64(len(r.lats))
	r.fastest = r.lats[0]
	r.slowest = r.lats[len(r.lats)-1]
	return r
}

func (this *DefaultPerf) Start() (interface{}, error) {
	//split into cc worker with number / cc request
	sync.Add(this.Cc)
	for i := 0; i < this.Cc; i++ {
		go func() {
			this.runWorker(this.Number/uint32(this.Cc), this.stopChs[i])
			sync.Done()
		}()
	}
	sync.Wait()
	close(this.results)
	return this.Report(), nil
}

func (this *DefaultPerf) Stop() error {
	for ch := range this.stopChs {
		go close(ch)
	}
}
