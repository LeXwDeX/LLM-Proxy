package main

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/LeXwDeX/LLM-Proxy/internal/admin"
	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	appmiddleware "github.com/LeXwDeX/LLM-Proxy/internal/middleware"
	"github.com/go-chi/chi/v5"
)

// routeDependencies contains fully initialized services. The composition root
// owns their lifetimes; this file only defines HTTP entry points and guards.
type routeDependencies struct {
	Sessions     *admin.SessionManager
	Clients      *auth.Store
	Portal       *admin.Portal
	Admin        http.Handler
	Proxy        http.Handler
	AppLogger    *slog.Logger
	AccessLogger *slog.Logger
}

// queryMethod is the RFC 10008 QUERY method. chi 5.4.0 added it to its method
// union, so every Mount() now matches it; chi 5.3.0 rejected it with 405 before
// routing. Keep the external behavior unchanged: no authentication, session
// middleware, or proxy forwarding for QUERY.
const queryMethod = "QUERY"

// rejectQueryMethod must run on the root mux, whose middleware executes before
// route matching, so the request never reaches the mounted admin or protected
// routers. It is registered after the logging middleware to preserve the
// pre-upgrade access-log record for rejected requests.
func rejectQueryMethod(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == queryMethod {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func newRouter(deps routeDependencies) chi.Router {
	router := chi.NewRouter()
	router.Use(appmiddleware.RequestID())
	router.Use(appmiddleware.Recoverer(deps.AppLogger))
	router.Use(appmiddleware.AccessLogger(deps.AccessLogger))
	router.Use(rejectQueryMethod)

	router.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	router.Get("/login", deps.Portal.HandleLoginPage)
	router.Post("/login", deps.Portal.HandleLogin)
	router.Post("/logout", deps.Portal.HandleLogout)

	// Every /admin route passes the session middleware.
	adminRouter := chi.NewRouter()
	adminRouter.Use(admin.AdminSecurityHeaders)
	adminRouter.Use(deps.Sessions.Middleware)
	adminRouter.Mount("/", deps.Admin)
	router.Mount("/admin", adminRouter)

	protected := chi.NewRouter()
	protected.Use(auth.Middleware(deps.Clients, deps.AppLogger))
	protected.Get("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		principal, ok := auth.PrincipalFromContext(r.Context())
		clientName := ""
		if ok && principal != nil {
			clientName = principal.Name
		}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]string{"message": "pong", "client": clientName}
		_ = json.NewEncoder(w).Encode(resp)
	})

	protected.NotFound(deps.Proxy.ServeHTTP)

	router.Mount("/", protected)
	return router
}
