package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"reflect"
	"sort"
	"time"

	"github.com/kardianos/service"
	dto "github.com/prometheus/client_model/go"

	"github.com/Control-D-Inc/ctrld"
	"github.com/Control-D-Inc/ctrld/internal/controld"
)

const (
	contentTypeJson  = "application/json"
	listClientsPath  = "/clients"
	startedPath      = "/started"
	reloadPath       = "/reload"
	deactivationPath = "/deactivation"
	cdPath           = "/cd"
	ifacePath        = "/iface"
	viewLogsPath     = "/logs/view"
	sendLogsPath     = "/logs/send"
)

type controlServer struct {
	server *http.Server
	mux    *http.ServeMux
	addr   string
}

func newControlServer(addr string) (*controlServer, error) {
	mux := http.NewServeMux()
	s := &controlServer{
		server: &http.Server{Handler: mux},
		mux:    mux,
	}
	s.addr = addr
	return s, nil
}

func (s *controlServer) start() error {
	_ = os.Remove(s.addr)
	unixListener, err := net.Listen("unix", s.addr)
	if l, ok := unixListener.(*net.UnixListener); ok {
		l.SetUnlinkOnClose(true)
	}
	if err != nil {
		return err
	}
	go s.server.Serve(unixListener)
	return nil
}

func (s *controlServer) stop() error {
	_ = os.Remove(s.addr)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*2)
	defer cancel()
	return s.server.Shutdown(ctx)
}

func (s *controlServer) register(pattern string, handler http.Handler) {
	s.mux.Handle(pattern, jsonResponse(handler))
}

func (p *prog) registerControlServerHandler() {
	p.cs.register(listClientsPath, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		clients := p.ciTable.ListClients()
		sort.Slice(clients, func(i, j int) bool {
			return clients[i].IP.Less(clients[j].IP)
		})
		if p.metricsQueryStats.Load() {
			for _, client := range clients {
				client.IncludeQueryCount = true
				dm := &dto.Metric{}
				m, err := statsClientQueriesCount.MetricVec.GetMetricWithLabelValues(
					client.IP.String(),
					client.Mac,
					client.Hostname,
				)
				if err != nil {
					mainLog.Load().Debug().Err(err).Msgf("could not get metrics for client: %v", client)
					continue
				}
				if err := m.Write(dm); err == nil {
					client.QueryCount = int64(dm.Counter.GetValue())
				}
			}
		}

		if err := json.NewEncoder(w).Encode(&clients); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}))
	p.cs.register(startedPath, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		select {
		case <-p.onStartedDone:
			w.WriteHeader(http.StatusOK)
		case <-time.After(10 * time.Second):
			w.WriteHeader(http.StatusRequestTimeout)
		}
	}))
	p.cs.register(reloadPath, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		listeners := make(map[string]*ctrld.ListenerConfig)
		p.mu.Lock()
		for k, v := range p.cfg.Listener {
			listeners[k] = &ctrld.ListenerConfig{
				IP:   v.IP,
				Port: v.Port,
			}
		}
		oldSvc := p.cfg.Service
		p.mu.Unlock()
		if err := p.sendReloadSignal(); err != nil {
			mainLog.Load().Err(err).Msg("could not send reload signal")
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		select {
		case <-p.reloadDoneCh:
		case <-time.After(5 * time.Second):
			http.Error(w, "timeout waiting for ctrld reload", http.StatusInternalServerError)
			return
		}

		p.mu.Lock()
		defer p.mu.Unlock()

		// Checking for cases that we could not do a reload.

		// 1. Listener config ip or port changes.
		for k, v := range p.cfg.Listener {
			l := listeners[k]
			if l == nil || l.IP != v.IP || l.Port != v.Port {
				w.WriteHeader(http.StatusCreated)
				return
			}
		}

		// 2. Service config changes.
		if !reflect.DeepEqual(oldSvc, p.cfg.Service) {
			w.WriteHeader(http.StatusCreated)
			return
		}

		// Otherwise, reload is done.
		w.WriteHeader(http.StatusOK)
	}))
	p.cs.register(deactivationPath, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		// Non-cd mode always allowing deactivation.
		if cdUID == "" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Re-fetch pin code from API.
		if rc, err := controld.FetchResolverConfig(cdUID, rootCmd.Version, cdDev); rc != nil {
			if rc.DeactivationPin != nil {
				cdDeactivationPin.Store(*rc.DeactivationPin)
			} else {
				cdDeactivationPin.Store(defaultDeactivationPin)
			}
		} else {
			mainLog.Load().Warn().Err(err).Msg("could not re-fetch deactivation pin code")
		}

		// If pin code not set, allowing deactivation.
		if deactivationPinNotSet() {
			w.WriteHeader(http.StatusOK)
			return
		}

		var req deactivationRequest
		if err := json.NewDecoder(request.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusPreconditionFailed)
			mainLog.Load().Err(err).Msg("invalid deactivation request")
			return
		}

		code := http.StatusForbidden
		switch req.Pin {
		case cdDeactivationPin.Load():
			code = http.StatusOK
		case defaultDeactivationPin:
			// If the pin code was set, but users do not provide --pin, return proper code to client.
			code = http.StatusBadRequest
		}
		w.WriteHeader(code)
	}))
	p.cs.register(cdPath, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if cdUID != "" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(cdUID))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	p.cs.register(ifacePath, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		// p.setDNS is only called when running as a service
		if !service.Interactive() {
			<-p.csSetDnsDone
			if p.csSetDnsOk {
				w.Write([]byte(iface))
				return
			}
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	p.cs.register(viewLogsPath, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		data, err := p.logContent()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(data) == 0 {
			w.WriteHeader(http.StatusMovedPermanently)
			return
		}
		if err := json.NewEncoder(w).Encode(&logViewResponse{Data: string(data)}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}))
	p.cs.register(sendLogsPath, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if time.Since(p.internalLogSent) < logSentInterval {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		data, err := p.logContent()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(data) == 0 {
			w.WriteHeader(http.StatusMovedPermanently)
			return
		}
		logFile := base64.StdEncoding.EncodeToString(data)
		req := &controld.LogsRequest{
			UID:     cdUID,
			LogFile: logFile,
		}
		mainLog.Load().Debug().Msg("sending log file to ControlD server")
		resp := logSentResponse{Size: len(data)}
		if err := controld.SendLogs(req, cdDev); err != nil {
			mainLog.Load().Error().Msgf("could not send log file to ControlD server: %v", err)
			resp.Error = err.Error()
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			mainLog.Load().Debug().Msg("sending log file successfully")
			w.WriteHeader(http.StatusOK)
		}
		if err := json.NewEncoder(w).Encode(&resp); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		p.internalLogSent = time.Now()
	}))
}

func jsonResponse(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next.ServeHTTP(w, r)
	})
}
