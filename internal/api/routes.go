package api

import (
	"net/http"
	"slices"
	"strings"
)

// handlerFunc is an http.HandlerFunc that reports failure by returning an
// error, which Server.h turns into the JSON error response.
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// h adapts a handlerFunc to an http.HandlerFunc.
func (s *Server) h(fn handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			s.fail(w, r, err)
		}
	}
}

// routes registers every route of docs/API.md, plus /metrics and the UI.
func (s *Server) routes() {
	m := s.mux
	const v1 = "/api/v1"

	// System.
	m.HandleFunc("GET "+v1+"/health", s.h(s.health))
	m.HandleFunc("GET "+v1+"/info", s.h(s.info))
	m.HandleFunc("GET /metrics", s.h(s.prometheus))

	// Services.
	m.HandleFunc("GET "+v1+"/services", s.h(s.listServices))
	m.HandleFunc("POST "+v1+"/services", s.h(s.createService))
	m.HandleFunc("GET "+v1+"/services/{id}", s.h(s.getService))
	m.HandleFunc("PUT "+v1+"/services/{id}", s.h(s.updateService))
	m.HandleFunc("DELETE "+v1+"/services/{id}", s.h(s.deleteService))
	m.HandleFunc("POST "+v1+"/services/{id}/mode", s.h(s.setServiceMode))
	m.HandleFunc("POST "+v1+"/services/{id}/enable", s.h(s.enableService))
	m.HandleFunc("POST "+v1+"/services/{id}/disable", s.h(s.disableService))
	m.HandleFunc("POST "+v1+"/services/{id}/reload", s.h(s.reloadService))
	m.HandleFunc("GET "+v1+"/services/{id}/stats", s.h(s.serviceStats))
	m.HandleFunc("GET "+v1+"/services/{id}/series", s.h(s.serviceSeries))

	// Recordings and exchanges. "import" is a literal segment: the mux
	// prefers it over the {id} pattern for POST /recordings/import.
	m.HandleFunc("GET "+v1+"/recordings", s.h(s.listRecordings))
	m.HandleFunc("POST "+v1+"/recordings", s.h(s.createRecording))
	m.HandleFunc("POST "+v1+"/recordings/import", s.h(s.importRecording))
	m.HandleFunc("GET "+v1+"/recordings/{id}", s.h(s.getRecording))
	m.HandleFunc("PATCH "+v1+"/recordings/{id}", s.h(s.patchRecording))
	m.HandleFunc("DELETE "+v1+"/recordings/{id}", s.h(s.deleteRecording))
	m.HandleFunc("GET "+v1+"/recordings/{id}/endpoints", s.h(s.recordingEndpoints))
	m.HandleFunc("GET "+v1+"/recordings/{id}/exchanges", s.h(s.listExchanges))
	m.HandleFunc("GET "+v1+"/exchanges/{id}", s.h(s.getExchange))
	m.HandleFunc("POST "+v1+"/recordings/{id}/anonymize", s.h(s.anonymizeRecording))
	m.HandleFunc("GET "+v1+"/recordings/{id}/export.har", s.h(s.exportRecording))

	// Anonymization.
	m.HandleFunc("GET "+v1+"/profiles", s.h(s.listProfiles))
	m.HandleFunc("POST "+v1+"/profiles", s.h(s.createProfile))
	m.HandleFunc("GET "+v1+"/profiles/{id}", s.h(s.getProfile))
	m.HandleFunc("PUT "+v1+"/profiles/{id}", s.h(s.updateProfile))
	m.HandleFunc("DELETE "+v1+"/profiles/{id}", s.h(s.deleteProfile))
	m.HandleFunc("GET "+v1+"/anonymize/detectors", s.h(s.listDetectors))
	m.HandleFunc("POST "+v1+"/anonymize/preview", s.h(s.previewAnonymization))

	// Fault injection.
	m.HandleFunc("GET "+v1+"/faults", s.h(s.listFaults))
	m.HandleFunc("POST "+v1+"/faults", s.h(s.createFault))
	m.HandleFunc("GET "+v1+"/faults/{id}", s.h(s.getFault))
	m.HandleFunc("PUT "+v1+"/faults/{id}", s.h(s.updateFault))
	m.HandleFunc("DELETE "+v1+"/faults/{id}", s.h(s.deleteFault))

	// Replay.
	m.HandleFunc("GET "+v1+"/replays", s.h(s.listReplays))
	m.HandleFunc("POST "+v1+"/replays", s.h(s.createReplay))
	m.HandleFunc("GET "+v1+"/replays/{id}", s.h(s.getReplay))
	m.HandleFunc("POST "+v1+"/replays/{id}/start", s.h(s.startReplay))
	m.HandleFunc("POST "+v1+"/replays/{id}/stop", s.h(s.stopReplay))
	m.HandleFunc("POST "+v1+"/replays/{id}/clone", s.h(s.cloneReplay))
	m.HandleFunc("DELETE "+v1+"/replays/{id}", s.h(s.deleteReplay))
	m.HandleFunc("GET "+v1+"/replays/{id}/series", s.h(s.replaySeries))

	// Live metrics.
	m.HandleFunc("GET "+v1+"/stats", s.h(s.allStats))
	m.HandleFunc("GET "+v1+"/stream", s.h(s.stream))

	// Anything else under /api/ is a JSON 404 (or 405); everything else is
	// the web UI.
	m.HandleFunc("/api/", s.apiNotFound)
	m.HandleFunc("/api", s.apiNotFound)
	if s.ui != nil {
		m.Handle("/", s.ui)
	} else {
		m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusNotFound, errorBody{Error: "the web UI is not enabled on this server"})
		})
	}
}

// apiMethods are probed to tell "wrong method" from "no such route".
var apiMethods = []string{
	http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
}

// apiNotFound answers requests that matched no API route: 405 with an Allow
// header when the path exists under other methods, 404 otherwise.
func (s *Server) apiNotFound(w http.ResponseWriter, r *http.Request) {
	var allow []string
	for _, method := range apiMethods {
		probe := r.Clone(r.Context())
		probe.Method = method
		if _, pattern := s.mux.Handler(probe); pattern != "" && pattern != "/api/" && pattern != "/api" {
			allow = append(allow, method)
			if method == http.MethodGet {
				allow = append(allow, http.MethodHead)
			}
		}
	}
	if len(allow) > 0 {
		slices.Sort(allow)
		w.Header().Set("Allow", strings.Join(allow, ", "))
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method " + r.Method + " is not allowed for " + r.URL.Path})
		return
	}
	writeJSON(w, http.StatusNotFound, errorBody{Error: "no such endpoint: " + r.Method + " " + r.URL.Path})
}
