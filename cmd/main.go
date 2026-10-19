package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"oumma/internal/ai"
	"oumma/internal/auth"
	"oumma/internal/documents"
	"oumma/internal/domain"
	"oumma/internal/engine"
	"oumma/internal/events"
	"oumma/internal/forms"
	"oumma/internal/handler"
	"oumma/internal/processconfig"
	"oumma/internal/repository"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(logger)

	// --- Repository ---
	var store repository.Repository
	var logStore repository.LLMLogStore
	var cpStore repository.CheckpointStore
	var commentStore repository.TaskCommentStore
	var attachmentStore repository.TaskAttachmentStore
	var trackStore repository.TrackStore
	var designStore repository.DesignStore
	var pgStore *repository.PostgresStore

	if dbURL := os.Getenv("DATABASE_URL"); dbURL != "" {
		pgs, err := repository.NewPostgresStore(dbURL)
		if err != nil {
			logger.Error("failed to connect to PostgreSQL", "error", err)
			os.Exit(1)
		}
		pgStore = pgs
		defer pgStore.Close()
		store = pgStore
		logStore = pgStore
		cpStore = pgStore
		commentStore = pgStore
		attachmentStore = pgStore
		trackStore = pgStore
		designStore = pgStore
		// Monthly usage rollup: archive the running counters into
		// usage_monthly (department/user/process cost) and reset them.
		pgStore.StartUsageRollover(context.Background())
		logger.Info("using PostgreSQL storage", "url", redactURL(dbURL))
	} else {
		memStore := repository.NewInMemoryStore()
		store = memStore
		logStore = memStore
		cpStore = memStore
		commentStore = memStore
		attachmentStore = memStore
		trackStore = memStore
		designStore = memStore
		logger.Info("using in-memory storage (set DATABASE_URL for PostgreSQL)")
	}

	// --- In-process data services (Go ports of the former Java sidecars) ---
	// The DMS document service (/dms-api) and the operational data index
	// (/oper-api) run inside this binary, sharing the same Postgres database.
	var docsSvc *documents.Service
	var formsSvc *forms.Service
	if pgStore != nil {
		db := pgStore.DB()
		if err := documents.Migrate(context.Background(), db); err != nil {
			logger.Error("failed to migrate dms schema", "error", err)
			os.Exit(1)
		}
		if err := forms.Migrate(context.Background(), db); err != nil {
			logger.Error("failed to migrate oper-data-index schema", "error", err)
			os.Exit(1)
		}
		authClaims := func(ctx context.Context) (string, []string, bool) {
			c, ok := handler.ClaimsFromContext(ctx)
			if !ok {
				return "", nil, false
			}
			return c.Username, c.Groups, true
		}
		docsSvc = documents.New(db, envOr("DMS_UPLOAD_DIR", "docs"), authClaims, logger)
		formsSvc = forms.New(db, authClaims, logger)
		logger.Info("in-process data services enabled", "dms_upload_dir", envOr("DMS_UPLOAD_DIR", "docs"))
	} else {
		logger.Warn("no DATABASE_URL — /dms-api and /oper-api endpoints are disabled")
	}

	// Load workflow definitions from YAML files.
	// Priority: CLI args > PROCESSES_DIR env > /processes volume > ./workflows/
	processDir := envOr("PROCESSES_DIR", "")
	if processDir == "" {
		// Check for Docker volume mount at /processes, fall back to local ./workflows/
		if info, err := os.Stat("/processes"); err == nil && info.IsDir() {
			processDir = "/processes"
		} else {
			processDir = "workflows"
		}
	}
	yamlPaths := os.Args[1:]
	if len(yamlPaths) == 0 {
		entries, err := os.ReadDir(processDir)
		if err != nil {
			logger.Error("failed to read processes directory", "dir", processDir, "error", err)
			os.Exit(1)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if (len(name) > 5 && name[len(name)-5:] == ".yaml") || (len(name) > 4 && name[len(name)-4:] == ".yml") {
				yamlPaths = append(yamlPaths, processDir+"/"+name)
			}
		}
		if len(yamlPaths) == 0 {
			logger.Error("no workflow YAML files found", "dir", processDir)
			os.Exit(1)
		}
	}
	for _, path := range yamlPaths {
		def, err := domain.ParseYAMLFile(path)
		if err != nil {
			// App-config-only YAML (an "app:" section but no engine tasks) is
			// not an engine definition — the processconfig store serves it.
			if processconfig.ParseAppFile(path) {
				logger.Warn("workflow YAML carries app config only — not registered as engine definition", "path", path)
				continue
			}
			logger.Error("failed to load workflow YAML", "path", path, "error", err)
			os.Exit(1)
		}
		// Designer-managed process (an "app:" section): the engine must not
		// serve it until the user CONFIRMS a deployment in the design
		// versions tab. A newly created project is therefore invisible to
		// the engine (and unlaunchable) while undeployed. Legacy file
		// processes without an app section keep the file-as-truth behaviour.
		if designStore != nil && processconfig.ParseAppFile(path) {
			if active, _, aerr := designStore.GetActiveDesign(context.Background(), def.ID); aerr != nil || active == nil {
				logger.Info("process not deployed yet — engine registration skipped until the user deploys a design version", "id", def.ID)
				continue
			}
		}
		if err := store.RegisterDefinition(context.Background(), def); err != nil {
			logger.Error("failed to register definition", "id", def.ID, "error", err)
			os.Exit(1)
		}
		logger.Info("loaded workflow definition",
			"id", def.ID,
			"tasks", len(def.Tasks),
			"edges", len(def.Topology),
			"source", path,
		)
	}

	// Re-register active versioned designs over the file-loaded definitions:
	// a deployed design (process_designs) is the source of truth for the
	// engine; files only seed the initial/baseline definitions.
	if actives, err := designStore.ListActiveDesigns(context.Background()); err == nil {
		for _, dv := range actives {
			if def, perr := repository.DesignOf(dv.Design); perr == nil {
				if rerr := store.RegisterDefinition(context.Background(), def); rerr == nil {
					logger.Info("applied active design at boot",
						"process_id", dv.ProcessID, "version", dv.Version)
				}
			}
		}
	} else {
		logger.Warn("list active designs at boot", "error", err)
	}

	// --- LLM Client (Dependency Injection) ---
	apiKey := os.Getenv("OPENAI_API_KEY")
	baseURL := envOr("OPENAI_BASE_URL", "")
	if apiKey == "" && baseURL == "" {
		logger.Warn("OPENAI_API_KEY not set and no custom OPENAI_BASE_URL — LLM calls will fail at runtime")
	} else if apiKey == "" {
		logger.Info("running without API key (local model mode)", "base_url", baseURL)
	}

	llmClient := ai.NewOpenAIClient(ai.OpenAIConfig{
		APIKey:    apiKey,
		BaseURL:   baseURL,
		Model:     envOr("OPENAI_MODEL", "gpt-4o"),
		KeepAlive: envOr("LLM_KEEP_ALIVE", "5m"),
		Options: ai.LLMOptions{
			NumCtx:      envInt("LLM_NUM_CTX", 4096),
			NumBatch:    envInt("LLM_NUM_BATCH", 512),
			NumPredict:  envInt("LLM_NUM_PREDICT", 256),
			Temperature: envFloat("LLM_TEMPERATURE", 0.7),
		},
	})

	// Designer-mode LLM — the design chat (/api/management/design/ask) may
	// run on a different endpoint/model than the process-mode agent. When
	// the DESIGNER_OPENAI_* vars are unset it falls back to the process LLM.
	designURL := os.Getenv("DESIGNER_OPENAI_BASE_URL")
	designKey := os.Getenv("DESIGNER_OPENAI_API_KEY")
	var designClient *ai.OpenAIClient
	if strings.TrimSpace(designURL) != "" || strings.TrimSpace(designKey) != "" {
		designClient = ai.NewOpenAIClient(ai.OpenAIConfig{
			APIKey:    designKey,
			BaseURL:   strings.TrimSpace(designURL),
			Model:     envOr("DESIGNER_OPENAI_MODEL", envOr("OPENAI_MODEL", "gpt-4o")),
			KeepAlive: envOr("LLM_KEEP_ALIVE", "5m"),
			Options: ai.LLMOptions{
				NumCtx:      envInt("LLM_NUM_CTX", 4096),
				NumBatch:    envInt("LLM_NUM_BATCH", 512),
				NumPredict:  envInt("LLM_NUM_PREDICT", 256),
				Temperature: envFloat("LLM_TEMPERATURE", 0.7),
			},
		})
		if strings.TrimSpace(designURL) != "" {
			ctxDP, cancelD := context.WithTimeout(context.Background(), 6*time.Second)
			if err := designClient.Ping(ctxDP); err != nil {
				logger.Warn("designer LLM server unreachable — design chat will fail until it is reachable", "base_url", designURL, "error", err)
			}
			cancelD()
		}
		logger.Info("designer LLM configured", "base_url", designURL, "model", envOr("DESIGNER_OPENAI_MODEL", "(process model)"))
	} else {
		designClient = llmClient
	}

	// Check LLM availability on startup when configured.
	if apiKey != "" || baseURL != "" {
		ctxPing, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		if err := llmClient.Ping(ctxPing); err != nil {
			logger.Error("LLM server unreachable", "error", err)
			os.Exit(1)
		}
		logger.Info("LLM server reachable")
	}

	// --- Engine ---
	eng := engine.New(store, llmClient, logger)
	eng.SetDesignStore(designStore)
	eng.SetLogStore(logStore)
	eng.SetCheckpointStore(cpStore)
	eng.SetCommentStore(commentStore)
	eng.SetAttachmentStore(attachmentStore)
	eng.SetTrackStore(trackStore)
	llmClient.SetLogStore(logStore)
	designClient.SetLogStore(logStore) // token attribution for the designer LLM

	// Restore processes that were running or paused when the engine last shut down.
	if err := eng.RestoreProcesses(context.Background()); err != nil {
		logger.Error("failed to restore processes", "error", err)
	}

	// Watchdog: fail any RUNNING process whose execution goroutine died
	// (orphaned instances would otherwise stay RUNNING forever).
	eng.StartReaper(time.Duration(envInt("REAPER_INTERVAL_SEC", 30)) * time.Second)

	// --- Kafka event dispatcher ---
	var dispatcher events.Dispatcher
	if brokerList := os.Getenv("KAFKA_BROKERS"); brokerList != "" {
		brokers := strings.Split(brokerList, ",")
		dispatcher = events.NewKafkaDispatcher(brokers, logger)
		logger.Info("kafka dispatcher enabled",
			"brokers", brokers,
			"topics", []string{events.TopicProcessStream, events.TopicTaskStream},
		)
	} else {
		dispatcher = events.NewLogDispatcher(logger)
		logger.Info("KAFKA_BROKERS not set — using log-only event dispatcher")
	}
	defer func() {
		if err := dispatcher.Close(); err != nil {
			logger.Error("error closing event dispatcher", "error", err)
		}
	}()
	eng.SetEventDispatcher(dispatcher)

	// Data-service tools for the LLM (/ask): the DMS doc service and the
	// operational data index run in-process, authorized via the caller's
	// claims from the /ask request context.
	if docsSvc != nil && formsSvc != nil {
		eng.SetServiceTools(engine.NewServiceTools(docsSvc, formsSvc))
		logger.Info("in-process data-service tools enabled")
	}

	// --- HTTP Server ---
	mux := http.NewServeMux()
	h := handler.New(eng)
	h.RegisterRoutes(mux)

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Single-port surface: proxy the Mailpit web inbox under /mailpit/ so the
	// whole platform is reachable on ONE external port (8095). Enabled via
	// MAILPIT_WEB_URL (docker compose sets it to http://mailpit:8025).
	if mailpitURL := strings.TrimSpace(os.Getenv("MAILPIT_WEB_URL")); mailpitURL != "" {
		target, err := url.Parse(mailpitURL)
		if err != nil {
			logger.Error("invalid MAILPIT_WEB_URL", "error", err)
		} else {
			proxy := httputil.NewSingleHostReverseProxy(target)
			mux.Handle("/mailpit/", http.StripPrefix("/mailpit", proxy))
			logger.Info("mailpit web inbox proxied", "path", "/mailpit/")
		}
	}

	// In-process data-service routes (Go ports of the Java sidecars).
	if docsSvc != nil {
		docsSvc.RegisterRoutes(mux, "/dms-api")
		formsSvc.RegisterRoutes(mux, "/oper-api")
	}

	// --- In-process auth (Go port of brief-auth-backend, /api/auth) ---
	authSvc := auth.New(auth.Config{
		BaseURL:           envOr("KEYCLOAK_URL", "http://localhost:8080"),
		Realm:             envOr("KEYCLOAK_REALM", "brief-test-realm"),
		ClientID:          envOr("KEYCLOAK_CLIENT_ID", "briefuserapp"),
		ClientSecret:      envOr("KEYCLOAK_CLIENT_SECRET", "2E0BcpPjuhEHP51MoYrA3zGMrPLOZbUw"),
		AdminClientID:     envOr("KEYCLOAK_ADMIN_CLIENT_ID", "oumma-admin"),
		AdminClientSecret: envOr("KEYCLOAK_ADMIN_CLIENT_SECRET", "aJ0KMvdhUOyAdKFUAG4ThhGW2koFfRzR"),
	}, logger)
	h.SetMailer(authSvc.SendMail)
	if act, ok := store.(repository.ActivationStore); ok {
		authSvc.SetActivationStore(act)
	}
	authSvc.RegisterRoutes(mux)
	logger.Info("in-process auth enabled (keycloak)", "url", envOr("KEYCLOAK_URL", "http://localhost:8080"), "realm", envOr("KEYCLOAK_REALM", "brief-test-realm"))

	// --- Process/role config (YAML) + user-processes / management routes ---
	pcStore, err := processconfig.NewStore(processDir, logger)
	if err != nil {
		logger.Error("failed to load process config", "error", err)
		os.Exit(1)
	}

	h.SetRoleStore(pcStore) // enforce designer-configured role permissions

	// Decision-form trace: after a task completion, if the completed task
	// has a form-step definition, record the submitted result in the oper
	// (forms) store as a trace before the engine proceeds.
	if formsSvc != nil {
		h.SetFormTracer(func(ctx context.Context, processID, taskName string, version int, input map[string]any) {
			// The step lookup uses the instance's pinned design version so
			// old instances trace against the form structure they launched
			// with; version 0 falls back to the live config.
			step := pcStore.StepForVersion(processID, taskName, version)
			if step == nil || len(step.Fields) == 0 {
				return
			}
			fields := make([]*forms.FormFieldRequest, 0, len(step.Fields))
			for _, f := range step.Fields {
				fr := &forms.FormFieldRequest{
					FieldName: strPtr(f.FieldName),
					FieldType: strPtr(f.FieldType),
				}
				if f.FieldLabel != "" {
					fr.FieldLabel = strPtr(f.FieldLabel)
				}
				if v, ok2 := input[f.FieldName]; ok2 {
					fr.FieldValue = v
				}
				fields = append(fields, fr)
			}
			req := &forms.ProcessFormRequest{
				ProcessID:         processID,
				ProcessInstanceID: []string{processID},
				Steps: []*forms.StepRequest{{
					StepID:            taskName,
					ProcessInstanceID: []string{processID},
					FormFields:        fields,
					FormInput:         input,
				}},
			}
			if _, err := formsSvc.SaveFormTrace(ctx, req); err != nil {
				logger.Warn("decision form trace failed", "process", processID, "task", taskName, "error", err)
			}
		})
	}
	go pcStore.StartReloadPoll(context.Background(), 5*time.Second)
	pcStore.RegisterRoutes(mux)
	pcStore.RegisterManagementRoutes(mux, authSvc)
	designSvc := processconfig.NewDesignService(pcStore, designStore, store, designClient, logger)
	designSvc.SetStateLister(store) // delete guard: no executed instances
	if rs, ok := any(logStore).(repository.DesignReviewStore); ok {
		designSvc.SetReviewStore(rs) // AI design-review history (design history)
	}
	designSvc.SetQuotaGuard(h.EnforceQuota) // designer monthly quota on the design chat
	designSvc.RegisterRoutes(mux)
	// Every design write (UI or AI chat) refreshes the single pending design
	// version; the designer deploys or discards it from the versions tab.
	pcStore.SetOnChanged(func(processID string) {
		// Synchronous on purpose: the snapshot sequence number is taken at
		// mutation time; SnapshotPending itself does the work in a goroutine.
		designSvc.SnapshotPending(context.Background(), processID, "designer")
	})
	// Version-aware lookups: running instances resolve permissions and form
	// steps against the design version they were launched with (the design
	// repository holds every deployed version's full document).
	if designStore != nil {
		pcStore.SetVersionDocFunc(func(processID string, version int) ([]byte, bool) {
			dv, err := designStore.GetDesign(context.Background(), processID, version)
			if err != nil || dv == nil || len(dv.Design) == 0 {
				return nil, false
			}
			return dv.Design, true
		})
	}

	// Launch form_id: step DTOs expose the oper-data form_id of the saved
	// step, and launches are rejected without a form_id that belongs to the
	// process's saved forms.
	if formsSvc != nil {
		pcStore.SetFormIDFunc(func(processID, stepID string) string {
			fid, ok := formsSvc.FormIDForStep(context.Background(), processID, stepID)
			if !ok {
				return ""
			}
			return fid
		})
		if h != nil {
			h.SetFormIDValidator(func(processID, formID string) bool {
				return formsSvc.ValidFormID(context.Background(), processID, formID)
			})
		}
	}

	// In-process token validation (replaces the former HTTP hop to
	// :8082/api/auth/session/active). Claims keep the same shape as before:
	// groups are stripped of the leading "/" Keycloak prefix.
	validateToken := func(ctx context.Context, token string) (*handler.AuthClaims, bool) {
		st, ok := authSvc.ValidateSession(ctx, token)
		if !ok || st.UserInfo == nil {
			return nil, false
		}
		ui := st.UserInfo
		groups := make([]string, 0, len(ui.Groups))
		for _, g := range ui.Groups {
			groups = append(groups, strings.TrimPrefix(g, "/"))
		}
		return &handler.AuthClaims{Active: true, Username: ui.PreferredUser, Groups: groups, Departments: ui.Department}, true
	}

	addr := envOr("ADDR", ":8080")
	srv := &http.Server{
		Addr:         addr,
		Handler:      handler.CORS(handler.RequireAuthFunc(validateToken)(mux)),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		logger.Info("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	fmt.Fprintf(os.Stderr, `
╔═══════════════════════════════════════════════════════════════════════╗
║                  Workflow Engine — %s                                 ║
╠═══════════════════════════════════════════════════════════════════════╣
║  GET    /processes/{process_id}/search  search instances          ║
║  GET    /definitions                                                  ║
║  ---- /dms-api (in-process DMS document service) ------------------- ║
║  POST   /dms-api/documents/upload   multipart document upload        ║
║  GET    /dms-api/documents  list      GET /dms-api/documents/search  ║
║  GET    /dms-api/documents/{id}     PUT/DELETE /dms-api/documents/{id}║
║  GET    /dms-api/documents/{id}/versions  .../{id}/download          ║
║  POST   /dms-api/tags  GET/PUT/DELETE /dms-api/tags/{id}             ║
║  GET    /dms-api/tags/ (results, ?search=)  PATCH /dms-api/tags/{id}/║
║  GET    /dms-api/tags/search  GET /dms-api/tags/all                  ║
║  ---- /oper-api (in-process operational data index) -----------------║
║  GET    /oper-api/user/forms  list saved forms (filters)             ║
║  GET    /oper-api/user/forms/searchSavedForms | searchform           ║
║  GET    /oper-api/user/forms/getform  GET .../formversions           ║
║  POST/PUT/GET/DELETE /oper-api/management/processform                ║
║  DELETE /oper-api/management/processform/steps  GET .../auth-test    ║
║  GET    /oper-api/health [/live /ready /started]                     ║
║  POST   /processes/{process_id}        start instance                 ║
║  POST   /processes/{process_id}/stream start + SSE stream             ║
║  GET    /processes/{process_id}        list instances                 ║
║  GET    /processes/{process_id}/{id}   get instance                   ║
║  DELETE /processes/{process_id}/{id}   abort instance                 ║
║  POST   /processes/{process_id}/{id}/{signal}  send signal            ║
║  GET    /processes/{process_id}/{id}/tasks      active HUMAN tasks.   ║
║  GET    /processes/{process_id}/{id}/{task}/{tid}  get task           ║
║  POST   /processes/{process_id}/{id}/{task}/{tid}  complete           ║
║  POST   /processes/{process_id}/{id}/{task}/{tid}/stream  complete           ║ 
║  POST   /processes/{process_id}/{id}/{task}/{tid}/comments            ║
║  GET    /processes/{process_id}/{id}/{task}/{tid}/comments            ║
║  PUT    /processes/{process_id}/{id}/{task}/{tid}/comments/{cid}.     ║
║  POST   /processes/{process_id}/{id}/{task}/{tid}/attachments         ║
║  GET    /processes/{process_id}/{id}/{task}/{tid}/attachments         ║
║  DELETE /processes/{process_id}/{id}/{task}/{tid}/attachments/{at_id} ║
║  GET    /processes/{process_id}/{id}/track  execution history         ║
║  ---- /api/auth (in-process auth, port of brief-auth-backend) ------- ║
║  POST   /api/auth/signin | signout | refresh                          ║
║  GET    /api/auth/session/active  GET /api/auth/health                ║
║  ---- /api/user-processes (role-filtered config from YAML) ---------- ║
║  GET    /my-accessible-processes  /task-groups[/{pid}]                ║
║  GET    /forms/{pid}[/{step}]  /ai-params/{pid}                       ║
║  GET    /search-items /authorised-search /view-access-roles           ║
║  GET    /templates/{pid}  POST /templates  GET /extract-all           ║
║  ---- /api/management (Designer, YAML-backed CRUD) -------------- ║
║  POST   /insert-process /add-role /add-forms /ai-params /add-template ║
║  POST   /create-user  POST /confirm-email/{userId}  ...               ║
║  GET    /health                                                       ║
╚═══════════════════════════════════════════════════════════════════════╝
`, addr)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

// redactURL hides the password portion of a database URL for logging.
func redactURL(raw string) string {
	// postgres://user:password@host:port/db → postgres://user:***@host:port/db
	at := -1
	colon2 := -1
	for i, c := range raw {
		if c == '@' {
			at = i
			break
		}
		if c == ':' {
			colon2 = i // keeps updating, last one before @ is the password separator
		}
	}
	if at > 0 && colon2 > 0 && colon2 < at {
		return raw[:colon2+1] + "***" + raw[at:]
	}
	return raw
}

func strPtr(v string) *string { return &v }
