package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xuri/excelize/v2"
)

//go:embed web/*
var webFiles embed.FS

type loginPageData struct{}

type server struct {
	db            *pgxpool.Pool
	sessionMu     sync.Mutex
	sessions      map[string]time.Time
	importSheetMu sync.RWMutex
	importSheet   string
}

type policy struct {
	ID             int64  `json:"id"`
	PolicyName     string `json:"policy_name"`
	Criticality    string `json:"criticality"`
	Category       string `json:"category"`
	Active         string `json:"active"`
	Tenant         string `json:"tenant"`
	DeviceCategory string `json:"device_category"`
	PlaybookUsage  string `json:"playbook_usage"`
}

func main() {
	dotEnvPath, err := resolveDotEnvPath()
	if err != nil {
		log.Fatalf("locate .env: %v", err)
	}
	if err := loadDotEnv(dotEnvPath); err != nil {
		log.Fatalf("load .env: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := connectDatabase(ctx)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `ALTER TABLE policy_list_filtered ADD COLUMN IF NOT EXISTS playbook_usage text`); err != nil {
		log.Fatalf("ensure playbook_usage column: %v", err)
	}
	if err := validateAuthConfig(); err != nil {
		log.Fatalf("authentication config: %v", err)
	}
	s := &server{db: pool, sessions: make(map[string]time.Time)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.home)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("GET /api/login", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /api/policies", s.listPolicies)
	mux.HandleFunc("POST /api/policies/import", s.importPolicies)
	mux.HandleFunc("POST /api/policies/import-report.xlsx", s.downloadImportReportXLSX)
	mux.HandleFunc("GET /api/policies/export.xlsx", s.exportPoliciesXLSX)
	mux.HandleFunc("POST /api/policies", s.createPolicy)
	mux.HandleFunc("GET /api/policies/{id}", s.getPolicy)
	mux.HandleFunc("PUT /api/policies/{id}", s.updatePolicy)
	mux.HandleFunc("DELETE /api/policies/{id}", s.deletePolicy)
	mux.HandleFunc("POST /api/policies/bulk-delete", s.bulkDeletePolicies)
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	addr := env("ADDR", ":9000")
	httpServer := &http.Server{Addr: addr, Handler: s.authMiddleware(mux), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("web service listening on http://localhost%s", addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("server shutdown: %v", err)
	}
}

func connectDatabase(ctx context.Context) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL())
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 10
	cfg.MinConns = 1
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	var pool *pgxpool.Pool
	for attempt := 1; attempt <= 10; attempt++ {
		pool, err = pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			err = pool.Ping(ctx)
		}
		if err == nil {
			break
		}
		if pool != nil {
			pool.Close()
			pool = nil
		}
		log.Printf("waiting for PostgreSQL (attempt %d/10): %v", attempt, err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if err != nil {
		return nil, err
	}
	return pool, nil
}

func (s *server) exportPoliciesXLSX(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), `SELECT id,COALESCE(policy_name,''),COALESCE(criticality,''),COALESCE(category,''),CASE WHEN lower(COALESCE(active,'')) IN ('active','enabled','true','yes','1') THEN 'Enabled' ELSE 'Disabled' END,COALESCE("Tenant",''),COALESCE("Device_Category",''),COALESCE(playbook_usage,'') FROM policy_list_filtered ORDER BY id`)
	if err != nil {
		apiError(w, 500, err)
		return
	}
	defer rows.Close()
	f := excelize.NewFile()
	defer f.Close()
	sheet := s.lastImportSheet()
	defaultSheet := f.GetSheetName(0)
	if err := f.SetSheetName(defaultSheet, sheet); err != nil {
		apiError(w, 500, err)
		return
	}
	headers := []any{"No.", "Policy Name", "Criticality", "Category", "Enabled", "Tenant", "Device Category", "Playbook Usage"}
	for col, v := range headers {
		cell, _ := excelize.CoordinatesToCellName(col+1, 1)
		if err := f.SetCellValue(sheet, cell, v); err != nil {
			apiError(w, 500, err)
			return
		}
	}
	rowNum := 2
	for rows.Next() {
		var p policy
		var enabled string
		if err := rows.Scan(&p.ID, &p.PolicyName, &p.Criticality, &p.Category, &enabled, &p.Tenant, &p.DeviceCategory, &p.PlaybookUsage); err != nil {
			apiError(w, 500, err)
			return
		}
		values := []any{p.ID, p.PolicyName, p.Criticality, p.Category, enabled, p.Tenant, p.DeviceCategory, p.PlaybookUsage}
		for col, v := range values {
			cell, _ := excelize.CoordinatesToCellName(col+1, rowNum)
			if err := f.SetCellValue(sheet, cell, v); err != nil {
				apiError(w, 500, err)
				return
			}
		}
		rowNum++
	}
	if err := rows.Err(); err != nil {
		apiError(w, 500, err)
		return
	}
	_ = f.SetColWidth(sheet, "A", "A", 10)
	_ = f.SetColWidth(sheet, "B", "B", 58)
	_ = f.SetColWidth(sheet, "C", "C", 16)
	_ = f.SetColWidth(sheet, "D", "D", 34)
	_ = f.SetColWidth(sheet, "E", "E", 14)
	_ = f.SetColWidth(sheet, "F", "F", 35)
	_ = f.SetColWidth(sheet, "G", "G", 35)
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="policy-list.xlsx"`)
	w.Header().Set("Cache-Control", "no-store")
	if _, err := f.WriteTo(w); err != nil {
		log.Printf("write policy workbook: %v", err)
	}
}
func (s *server) downloadImportReportXLSX(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 5<<20)
	var report importResult
	if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
		apiError(w, 400, fmt.Errorf("invalid import report: %w", err))
		return
	}
	if len(report.Details) > 10000 {
		apiError(w, 400, fmt.Errorf("report has too many rows"))
		return
	}
	f := excelize.NewFile()
	defer f.Close()
	sheet := f.GetSheetName(0)
	headers := []any{"No.", "Policy Name", "Action", "Tenant", "Details"}
	for col, v := range headers {
		cell, _ := excelize.CoordinatesToCellName(col+1, 1)
		if err := f.SetCellValue(sheet, cell, v); err != nil {
			apiError(w, 500, err)
			return
		}
	}
	for i, item := range report.Details {
		values := []any{item.No, item.PolicyName, item.Action, item.Tenant, item.Details}
		for col, v := range values {
			cell, _ := excelize.CoordinatesToCellName(col+1, i+2)
			if err := f.SetCellValue(sheet, cell, v); err != nil {
				apiError(w, 500, err)
				return
			}
		}
	}
	_ = f.SetColWidth(sheet, "A", "A", 10)
	_ = f.SetColWidth(sheet, "B", "B", 54)
	_ = f.SetColWidth(sheet, "C", "C", 16)
	_ = f.SetColWidth(sheet, "D", "D", 30)
	_ = f.SetColWidth(sheet, "E", "E", 64)
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="policy-import-report.xlsx"`)
	w.Header().Set("Cache-Control", "no-store")
	if _, err := f.WriteTo(w); err != nil {
		log.Printf("write import report workbook: %v", err)
	}
}

func validateAuthConfig() error {
	if strings.TrimSpace(os.Getenv("ADMIN_USER")) == "" {
		return fmt.Errorf("ADMIN_USER must be set in .env")
	}
	raw := strings.TrimSpace(os.Getenv("ADMIN_PASSWORD_SHA256"))
	if len(raw) != 64 {
		return fmt.Errorf("ADMIN_PASSWORD_SHA256 must be a 64-character SHA-256 hex digest")
	}
	if _, err := hex.DecodeString(raw); err != nil {
		return fmt.Errorf("ADMIN_PASSWORD_SHA256 must be valid hex")
	}
	return nil
}

func (s *server) loginPage(w http.ResponseWriter, r *http.Request) {
	data, err := webFiles.ReadFile("web/login.html")
	if err != nil {
		http.Error(w, "login page unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, 400, fmt.Errorf("invalid login request"))
		return
	}
	expectedUser := os.Getenv("ADMIN_USER")
	userOK := subtle.ConstantTimeCompare([]byte(req.Username), []byte(expectedUser))
	sum := sha256.Sum256([]byte(req.Password))
	expected, _ := hex.DecodeString(os.Getenv("ADMIN_PASSWORD_SHA256"))
	passOK := subtle.ConstantTimeCompare(sum[:], expected)
	if userOK != 1 || passOK != 1 {
		apiError(w, 401, fmt.Errorf("ชื่อผู้ใช้หรือรหัสผ่านไม่ถูกต้อง"))
		return
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		apiError(w, 500, fmt.Errorf("could not create session"))
		return
	}
	session := hex.EncodeToString(token)
	expires := time.Now().Add(12 * time.Hour)
	s.sessionMu.Lock()
	s.sessions[session] = expires
	s.sessionMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "policy_session", Value: session, Path: "/", HttpOnly: true, Secure: strings.EqualFold(os.Getenv("COOKIE_SECURE"), "true"), SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: 43200})
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("policy_session"); err == nil {
		s.sessionMu.Lock()
		delete(s.sessions, c.Value)
		s.sessionMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "policy_session", Value: "", Path: "/", HttpOnly: true, Secure: strings.EqualFold(os.Getenv("COOKIE_SECURE"), "true"), SameSite: http.SameSiteStrictMode, MaxAge: -1})
	writeJSON(w, 200, map[string]string{"status": "logged_out"})
}
func (s *server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" || r.URL.Path == "/api/login" || r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie("policy_session")
		valid := false
		if err == nil {
			s.sessionMu.Lock()
			expiry, ok := s.sessions[cookie.Value]
			if ok && time.Now().Before(expiry) {
				valid = true
			} else if ok {
				delete(s.sessions, cookie.Value)
			}
			s.sessionMu.Unlock()
		}
		if !valid {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				apiError(w, 401, fmt.Errorf("login required"))
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (s *server) home(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	tmpl, err := template.ParseFS(webFiles, "web/index.html")
	if err != nil {
		http.Error(w, "template error", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	if err := tmpl.Execute(w, nil); err != nil {
		log.Printf("render page: %v", err)
	}
}

type importAction struct {
	No         int64  `json:"no"`
	PolicyName string `json:"policy_name"`
	Tenant     string `json:"tenant"`
	Action     string `json:"action"`
	Details    string `json:"details,omitempty"`
}
type importResult struct {
	File      string         `json:"file"`
	TotalRows int            `json:"total_rows"`
	Added     int            `json:"added"`
	Updated   int            `json:"updated"`
	Removed   int            `json:"removed"`
	Unchanged int            `json:"unchanged"`
	Details   []importAction `json:"details"`
}

func (s *server) importPolicies(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	if err := r.ParseMultipartForm(20 << 20); err != nil {
		apiError(w, 400, fmt.Errorf("upload must be a valid XLSX file (max 20 MB)"))
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		apiError(w, 400, fmt.Errorf("missing XLSX file field 'file'"))
		return
	}
	defer file.Close()
	if !strings.EqualFold(filepath.Ext(header.Filename), ".xlsx") {
		apiError(w, 400, fmt.Errorf("only .xlsx files are supported"))
		return
	}
	input, sheet, err := readPolicyXLSX(file)
	if err != nil {
		apiError(w, 400, err)
		return
	}
	s.setLastImportSheet(sheet)
	tx, err := s.db.Begin(r.Context())
	if err != nil {
		apiError(w, 500, err)
		return
	}
	defer tx.Rollback(r.Context())
	dbrows, err := tx.Query(r.Context(), `SELECT id,COALESCE(policy_name,''),COALESCE(criticality,''),COALESCE(category,''),COALESCE(active,''),COALESCE("Tenant",''),COALESCE("Device_Category",''),COALESCE(playbook_usage,'') FROM policy_list_filtered`)
	if err != nil {
		apiError(w, 500, err)
		return
	}
	existing := map[int64]policy{}
	for dbrows.Next() {
		var p policy
		if err := dbrows.Scan(&p.ID, &p.PolicyName, &p.Criticality, &p.Category, &p.Active, &p.Tenant, &p.DeviceCategory, &p.PlaybookUsage); err != nil {
			dbrows.Close()
			apiError(w, 500, err)
			return
		}
		existing[p.ID] = p
	}
	if err := dbrows.Err(); err != nil {
		dbrows.Close()
		apiError(w, 500, err)
		return
	}
	dbrows.Close()
	maxNo := int64(0)
	present := make(map[int64]bool, len(input))
	for _, p := range input {
		present[p.ID] = true
		if p.ID > maxNo {
			maxNo = p.ID
		}
	}
	result := importResult{File: header.Filename, TotalRows: len(input), Details: []importAction{}}
	for _, candidate := range input {
		old, found := existing[candidate.ID]
		if !found {
			// Re-key old generated-ID rows once, so a first sync updates rather than duplicates them.
			legacyID := int64(0)
			for id, row := range existing {
				if id > maxNo && sameValue(row.PolicyName, candidate.PolicyName) {
					if legacyID == 0 || policyCoreEqual(row, candidate) {
						legacyID = id
						if policyCoreEqual(row, candidate) {
							break
						}
					}
				}
			}
			if legacyID > 0 {
				old = existing[legacyID]
				if _, err := tx.Exec(r.Context(), `UPDATE policy_list_filtered SET id=$1,policy_name=$2,criticality=$3,category=$4,active=$5,"Tenant"=$6,"Device_Category"=$7,playbook_usage=$8 WHERE id=$9`, candidate.ID, candidate.PolicyName, candidate.Criticality, candidate.Category, candidate.Active, candidate.Tenant, candidate.DeviceCategory, candidate.PlaybookUsage, legacyID); err != nil {
					apiError(w, 500, fmt.Errorf("migrate legacy No. %d: %w", legacyID, err))
					return
				}
				delete(existing, legacyID)
				existing[candidate.ID] = candidate
				result.Updated++
				result.Details = append(result.Details, importAction{No: candidate.ID, PolicyName: candidate.PolicyName, Tenant: candidate.Tenant, Action: "updated", Details: fmt.Sprintf("ID changed from %d to workbook No.; fields synchronized", legacyID)})
				continue
			}
			if _, err := tx.Exec(r.Context(), `INSERT INTO policy_list_filtered (id,policy_name,criticality,category,active,"Tenant","Device_Category",playbook_usage) OVERRIDING SYSTEM VALUE VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, candidate.ID, candidate.PolicyName, candidate.Criticality, candidate.Category, candidate.Active, candidate.Tenant, candidate.DeviceCategory, candidate.PlaybookUsage); err != nil {
				apiError(w, 500, fmt.Errorf("insert No. %d: %w", candidate.ID, err))
				return
			}
			existing[candidate.ID] = candidate
			result.Added++
			result.Details = append(result.Details, importAction{No: candidate.ID, PolicyName: candidate.PolicyName, Tenant: candidate.Tenant, Action: "added", Details: "No. not found; added from workbook"})
			continue
		}
		changed := []string{}
		if !sameValue(old.PolicyName, candidate.PolicyName) {
			changed = append(changed, "Policy Name")
		}
		if !sameValue(old.Criticality, candidate.Criticality) {
			changed = append(changed, "Criticality")
		}
		if !sameValue(old.Category, candidate.Category) {
			changed = append(changed, "Category")
		}
		if !sameValue(old.Active, candidate.Active) {
			changed = append(changed, "Active")
		}
		if !sameValue(old.Tenant, candidate.Tenant) {
			changed = append(changed, "Tenant")
		}
		if !sameValue(old.DeviceCategory, candidate.DeviceCategory) {
			changed = append(changed, "Device Category")
		}
		if !sameValue(old.PlaybookUsage, candidate.PlaybookUsage) {
			changed = append(changed, "Playbook Usage")
		}
		if len(changed) == 0 {
			result.Unchanged++
			continue
		}
		if _, err := tx.Exec(r.Context(), `UPDATE policy_list_filtered SET policy_name=$1,criticality=$2,category=$3,active=$4,"Tenant"=$5,"Device_Category"=$6,playbook_usage=$7 WHERE id=$8`, candidate.PolicyName, candidate.Criticality, candidate.Category, candidate.Active, candidate.Tenant, candidate.DeviceCategory, candidate.PlaybookUsage, candidate.ID); err != nil {
			apiError(w, 500, fmt.Errorf("update No. %d: %w", candidate.ID, err))
			return
		}
		existing[candidate.ID] = candidate
		result.Updated++
		result.Details = append(result.Details, importAction{No: candidate.ID, PolicyName: candidate.PolicyName, Tenant: candidate.Tenant, Action: "updated", Details: "Changed: " + strings.Join(changed, ", ")})
	}
	for id, old := range existing {
		if present[id] {
			continue
		}
		if _, err := tx.Exec(r.Context(), `DELETE FROM policy_list_filtered WHERE id=$1`, id); err != nil {
			apiError(w, 500, fmt.Errorf("remove No. %d: %w", id, err))
			return
		}
		result.Removed++
		result.Details = append(result.Details, importAction{No: id, PolicyName: old.PolicyName, Tenant: old.Tenant, Action: "removed", Details: "No. is not present in workbook"})
	}
	if _, err := tx.Exec(r.Context(), `SELECT setval(pg_get_serial_sequence('policy_list_filtered','id'),GREATEST(COALESCE((SELECT max(id) FROM policy_list_filtered),1),1),true)`); err != nil {
		apiError(w, 500, fmt.Errorf("sync id sequence: %w", err))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		apiError(w, 500, err)
		return
	}
	writeJSON(w, 200, result)
}
func sameValue(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}
func policyCoreEqual(a, b policy) bool {
	return sameValue(a.PolicyName, b.PolicyName) && sameValue(a.Criticality, b.Criticality) && sameValue(a.Category, b.Category) && sameValue(a.Active, b.Active) && sameValue(a.DeviceCategory, b.DeviceCategory)
}
func mergeTenants(oldValue, newValue string) (string, []string) {
	known := map[string]string{}
	for _, t := range strings.Split(normalizeTenant(oldValue), ",") {
		if t != "" {
			known[strings.ToLower(t)] = t
		}
	}
	added := []string{}
	for _, t := range strings.Split(normalizeTenant(newValue), ",") {
		if t != "" {
			k := strings.ToLower(t)
			if _, ok := known[k]; !ok {
				known[k] = t
				added = append(added, t)
			}
		}
	}
	all := make([]string, 0, len(known))
	for _, t := range known {
		all = append(all, t)
	}
	sort.SliceStable(all, func(i, j int) bool { return strings.ToLower(all[i]) < strings.ToLower(all[j]) })
	sort.SliceStable(added, func(i, j int) bool { return strings.ToLower(added[i]) < strings.ToLower(added[j]) })
	return strings.Join(all, ","), added
}
func policyCoreDifferences(a, b policy) []string {
	d := []string{}
	if !sameValue(a.Criticality, b.Criticality) {
		d = append(d, "Criticality")
	}
	if !sameValue(a.Category, b.Category) {
		d = append(d, "Category")
	}
	if !sameValue(a.Active, b.Active) {
		d = append(d, "Enabled/Active")
	}
	if !sameValue(a.DeviceCategory, b.DeviceCategory) {
		d = append(d, "Device Category")
	}
	return d
}
func readPolicyXLSX(src multipart.File) ([]policy, string, error) {
	f, err := excelize.OpenReader(src)
	if err != nil {
		return nil, "", fmt.Errorf("cannot read XLSX: %w", err)
	}
	defer f.Close()
	sheet := ""
	for _, name := range f.GetSheetList() {
		if strings.EqualFold(strings.TrimSpace(name), "In Scope") {
			sheet = name
			break
		}
	}
	if sheet == "" {
		return nil, "", fmt.Errorf("worksheet 'In Scope' was not found")
	}
	data, err := f.GetRows(sheet)
	if err != nil {
		return nil, "", err
	}
	if len(data) < 2 {
		return nil, "", fmt.Errorf("worksheet has no policy rows")
	}
	indexes := map[string]int{}
	for i, h := range data[0] {
		indexes[normalizeHeader(h)] = i
	}
	required := []string{"no.", "policy name", "criticality", "category", "enabled", "tenant", "device category", "playbook usage"}
	for _, name := range required {
		if _, ok := indexes[name]; !ok {
			return nil, "", fmt.Errorf("required column '%s' was not found", name)
		}
	}
	cell := func(row []string, key string) string {
		idx := indexes[key]
		if idx >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[idx])
	}
	result := make([]policy, 0, len(data)-1)
	seenNo := map[int64]bool{}
	for line, row := range data[1:] {
		name := cell(row, "policy name")
		if name == "" {
			continue
		}
		no, parseErr := strconv.ParseInt(cell(row, "no."), 10, 64)
		if parseErr != nil || no < 1 {
			return nil, "", fmt.Errorf("row %d: invalid No. %q", line+2, cell(row, "no."))
		}
		if seenNo[no] {
			return nil, "", fmt.Errorf("row %d: duplicate No. %d", line+2, no)
		}
		seenNo[no] = true
		enabled := strings.ToLower(cell(row, "enabled"))
		var active string
		switch enabled {
		case "enabled", "active", "true", "yes", "1":
			active = "active"
		case "disabled", "inactive", "in-active", "false", "no", "0":
			active = "in-active"
		default:
			return nil, "", fmt.Errorf("row %d: unsupported Enabled value %q", line+2, cell(row, "enabled"))
		}
		tenant := normalizeTenant(cell(row, "tenant"))
		if tenant == "" {
			return nil, "", fmt.Errorf("row %d: Tenant is empty", line+2)
		}
		result = append(result, policy{ID: no, PolicyName: name, Criticality: strings.ToUpper(cell(row, "criticality")), Category: cell(row, "category"), Active: active, Tenant: tenant, DeviceCategory: cell(row, "device category"), PlaybookUsage: cell(row, "playbook usage")})
	}
	if len(result) == 0 {
		return nil, "", fmt.Errorf("no valid policy records found")
	}
	return result, sheet, nil
}

func (s *server) lastImportSheet() string {
	s.importSheetMu.RLock()
	defer s.importSheetMu.RUnlock()
	if s.importSheet == "" {
		return "In Scope"
	}
	return s.importSheet
}

func (s *server) setLastImportSheet(sheet string) {
	s.importSheetMu.Lock()
	s.importSheet = sheet
	s.importSheetMu.Unlock()
}
func normalizeHeader(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(s)), " "))
}
func normalizeTenant(s string) string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == '|' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
	seen := map[string]bool{}
	out := []string{}
	for _, value := range fields {
		value = strings.TrimSpace(value)
		key := strings.ToLower(value)
		if value != "" && !seen[key] {
			seen[key] = true
			out = append(out, value)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return strings.Join(out, ",")
}
func (s *server) listPolicies(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	rows, err := s.db.Query(r.Context(), `SELECT id,COALESCE(policy_name,''),COALESCE(criticality,''),COALESCE(category,''),COALESCE(active,''),COALESCE("Tenant",''),COALESCE("Device_Category",''),COALESCE(playbook_usage,'') FROM policy_list_filtered WHERE $1='' OR policy_name ILIKE '%'||$1||'%' OR criticality ILIKE '%'||$1||'%' OR category ILIKE '%'||$1||'%' OR active ILIKE '%'||$1||'%' OR "Tenant" ILIKE '%'||$1||'%' OR "Device_Category" ILIKE '%'||$1||'%' OR playbook_usage ILIKE '%'||$1||'%' ORDER BY id LIMIT 5000`, q)
	if err != nil {
		apiError(w, 500, err)
		return
	}
	defer rows.Close()
	items := []policy{}
	for rows.Next() {
		var p policy
		if err := rows.Scan(&p.ID, &p.PolicyName, &p.Criticality, &p.Category, &p.Active, &p.Tenant, &p.DeviceCategory, &p.PlaybookUsage); err != nil {
			apiError(w, 500, err)
			return
		}
		items = append(items, p)
	}
	if err := rows.Err(); err != nil {
		apiError(w, 500, err)
		return
	}
	writeJSON(w, 200, items)
}
func (s *server) getPolicy(w http.ResponseWriter, r *http.Request) {
	p, err := s.findPolicy(r)
	if err != nil {
		apiError(w, policyErrorStatus(err), err)
		return
	}
	writeJSON(w, 200, p)
}
func (s *server) createPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := decodePolicy(w, r)
	if !ok {
		return
	}
	err := s.db.QueryRow(r.Context(), `INSERT INTO policy_list_filtered (policy_name,criticality,category,active,"Tenant","Device_Category",playbook_usage) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`, p.PolicyName, p.Criticality, p.Category, p.Active, p.Tenant, p.DeviceCategory, p.PlaybookUsage).Scan(&p.ID)
	if err != nil {
		apiError(w, 500, err)
		return
	}
	writeJSON(w, 201, p)
}
func (s *server) updatePolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := decodePolicy(w, r)
	if !ok {
		return
	}
	id, err := parseID(r)
	if err != nil {
		apiError(w, 400, err)
		return
	}
	tag, err := s.db.Exec(r.Context(), `UPDATE policy_list_filtered SET policy_name=$1,criticality=$2,category=$3,active=$4,"Tenant"=$5,"Device_Category"=$6,playbook_usage=$7 WHERE id=$8`, p.PolicyName, p.Criticality, p.Category, p.Active, p.Tenant, p.DeviceCategory, p.PlaybookUsage, id)
	if err != nil {
		apiError(w, 500, err)
		return
	}
	if tag.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	p.ID = id
	writeJSON(w, 200, p)
}
func (s *server) deletePolicy(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		apiError(w, 400, err)
		return
	}
	tag, err := s.db.Exec(r.Context(), `DELETE FROM policy_list_filtered WHERE id=$1`, id)
	if err != nil {
		apiError(w, 500, err)
		return
	}
	if tag.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type bulkDeleteRequest struct {
	IDs []int64 `json:"ids"`
	All bool    `json:"all"`
}

func (s *server) bulkDeletePolicies(w http.ResponseWriter, r *http.Request) {
	var req bulkDeleteRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, 400, fmt.Errorf("invalid JSON: %w", err))
		return
	}
	var tag pgconn.CommandTag
	var err error
	if req.All {
		tag, err = s.db.Exec(r.Context(), `DELETE FROM policy_list_filtered`)
	} else {
		if len(req.IDs) == 0 || len(req.IDs) > 5000 {
			apiError(w, 400, fmt.Errorf("select between 1 and 5000 policies"))
			return
		}
		seen := map[int64]bool{}
		ids := make([]int64, 0, len(req.IDs))
		for _, id := range req.IDs {
			if id < 1 {
				apiError(w, 400, fmt.Errorf("invalid policy id"))
				return
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		tag, err = s.db.Exec(r.Context(), `DELETE FROM policy_list_filtered WHERE id=ANY($1)`, ids)
	}
	if err != nil {
		apiError(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]int64{"deleted": tag.RowsAffected()})
}

func (s *server) findPolicy(r *http.Request) (policy, error) {
	id, err := parseID(r)
	if err != nil {
		return policy{}, err
	}
	var p policy
	err = s.db.QueryRow(r.Context(), `SELECT id,COALESCE(policy_name,''),COALESCE(criticality,''),COALESCE(category,''),COALESCE(active,''),COALESCE("Tenant",''),COALESCE("Device_Category",''),COALESCE(playbook_usage,'') FROM policy_list_filtered WHERE id=$1`, id).Scan(&p.ID, &p.PolicyName, &p.Criticality, &p.Category, &p.Active, &p.Tenant, &p.DeviceCategory, &p.PlaybookUsage)
	return p, err
}
func parseID(r *http.Request) (int64, error) {
	var id int64
	if _, err := fmt.Sscan(r.PathValue("id"), &id); err != nil || id < 1 {
		return 0, fmt.Errorf("invalid policy id")
	}
	return id, nil
}
func decodePolicy(w http.ResponseWriter, r *http.Request) (policy, bool) {
	var p policy
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		apiError(w, 400, fmt.Errorf("invalid JSON: %w", err))
		return p, false
	}
	p.PolicyName = strings.TrimSpace(p.PolicyName)
	if p.PolicyName == "" {
		apiError(w, 400, fmt.Errorf("policy_name is required"))
		return p, false
	}
	p.Criticality = strings.TrimSpace(p.Criticality)
	p.Category = strings.TrimSpace(p.Category)
	p.Active = strings.TrimSpace(p.Active)
	p.Tenant = strings.TrimSpace(p.Tenant)
	p.DeviceCategory = strings.TrimSpace(p.DeviceCategory)
	p.PlaybookUsage = strings.TrimSpace(p.PlaybookUsage)
	return p, true
}
func policyErrorStatus(err error) int {
	if errors.Is(err, os.ErrNotExist) {
		return 404
	}
	return 400
}
func apiError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write JSON: %v", err)
	}
}
func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}
func (s *server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Ping(r.Context()); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ready\n"))
}
func databaseURL() string {
	if url := strings.TrimSpace(os.Getenv("DATABASE_URL")); url != "" {
		return url
	}
	return "postgres://" + env("POSTGRES_USER", "gpn") + ":" + env("POSTGRES_PASSWORD", "gpn") + "@" + env("POSTGRES_HOST", "localhost") + ":" + env("POSTGRES_PORT", "5432") + "/" + env("POSTGRES_DB", "gpn") + "?sslmode=" + env("POSTGRES_SSLMODE", "disable")
}
func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// resolveDotEnvPath prefers configuration shipped beside the executable. This
// avoids accidentally loading an unrelated .env from CMD's working directory.
// For `go run`, the temporary executable has no adjacent .env, so use the
// current working directory as a fallback.
func resolveDotEnvPath() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	executableEnv := filepath.Join(filepath.Dir(executable), ".env")
	if _, err := os.Stat(executableEnv); err == nil {
		return executableEnv, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if _, err := os.Stat(".env"); err == nil {
		return ".env", nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return executableEnv, nil
}

func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" {
			return fmt.Errorf("invalid entry at line %d", lineNumber)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		} else if comment := strings.Index(value, " #"); comment >= 0 {
			value = strings.TrimSpace(value[:comment])
		}
		// Authentication credentials are managed in the project .env file. This
		// prevents stale ADMIN_* variables inherited by a CMD session from
		// silently overriding the credentials the operator just configured.
		if key == "ADMIN_USER" || key == "ADMIN_PASSWORD_SHA256" {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}
