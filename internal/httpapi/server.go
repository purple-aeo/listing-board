package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"listingboard/internal/board"
)

const (
	sessionCookie   = "lb_session"
	maxBrochureSize = 1 << 20
)

type Server struct {
	store   board.Store
	dataDir string
	webDir  string
	handler http.Handler
}

type account struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

func New(store board.Store, dataDir, webDir string) *Server {
	s := &Server{store: store, dataDir: dataDir, webDir: webDir}
	s.handler = withCORS(s.routes())
	return s
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("POST /api/register", s.register)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("GET /api/jobs", s.listJobs)
	mux.HandleFunc("POST /api/jobs", s.createJob)
	mux.HandleFunc("GET /api/jobs/{id}", s.getJob)
	mux.HandleFunc("POST /api/jobs/{id}/file", s.upload)
	mux.HandleFunc("POST /api/jobs/{id}/start", s.start)
	mux.HandleFunc("GET /api/jobs/{id}/file", s.download)
	if s.webDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(s.webDir)))
	}
	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "Request body is not valid JSON")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if !strings.Contains(email, "@") || len(email) > 200 {
		writeErr(w, http.StatusBadRequest, "Enter an email address")
		return
	}
	if len(body.Password) < 8 || len(body.Password) > 200 {
		writeErr(w, http.StatusBadRequest, "Password must be at least 8 characters")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create the account")
		return
	}
	user, err := s.store.CreateUser(r.Context(), email, string(hash))
	if errors.Is(err, board.ErrDuplicate) {
		writeErr(w, http.StatusConflict, "An account with that email already exists")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create the account")
		return
	}
	if err := s.openSession(w, r, user); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not start the session")
		return
	}
	writeJSON(w, http.StatusCreated, account{ID: user.ID, Email: user.Email})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "Request body is not valid JSON")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	user, err := s.store.UserByEmail(r.Context(), email)
	if errors.Is(err, board.ErrNotFound) {
		writeErr(w, http.StatusUnauthorized, "That email or password is incorrect")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not sign in")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(body.Password)) != nil {
		writeErr(w, http.StatusUnauthorized, "That email or password is incorrect")
		return
	}
	if err := s.openSession(w, r, user); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not start the session")
		return
	}
	writeJSON(w, http.StatusOK, account{ID: user.ID, Email: user.Email})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.DeleteSession(r.Context(), cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, account{ID: user.ID, Email: user.Email})
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	jobs, err := s.store.ListJobs(r.Context(), user.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not load sheets")
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	var body struct {
		Title   string `json:"title"`
		Address string `json:"address"`
	}
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "Request body is not valid JSON")
		return
	}
	title := strings.TrimSpace(body.Title)
	address := strings.TrimSpace(body.Address)
	if title == "" || address == "" || len(title) > 200 || len(address) > 200 {
		writeErr(w, http.StatusBadRequest, "Enter a title and an address")
		return
	}
	now := time.Now().UTC()
	job := board.Job{
		ID:        board.NewID(),
		UserID:    user.ID,
		Title:     title,
		Address:   address,
		Status:    "draft",
		Checklist: board.NewChecklist(),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.store.CreateJob(r.Context(), job); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create the sheet")
		return
	}
	writeJSON(w, http.StatusCreated, job)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	job, ok := s.load(w, r, user.ID)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	job, ok := s.load(w, r, user.ID)
	if !ok {
		return
	}

	if strings.TrimSpace(job.Filename) != "" {
		writeErr(w, http.StatusBadRequest, "A brochure is already attached")
		return
	}

	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Brochure upload was incomplete")
		return
	}

	var (
		foundFile bool
		rawName   string
		data      []byte
	)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, "Brochure upload was incomplete")
			return
		}
		if part.FormName() == "file" {
			foundFile = true
			rawName = filenameFromHeader(part.Header.Get("Content-Disposition"), part.FileName())
			data, err = io.ReadAll(io.LimitReader(part, maxBrochureSize+1))
			if err != nil {
				writeErr(w, http.StatusBadRequest, "Brochure upload was incomplete")
				return
			}
			if len(data) > maxBrochureSize {
				writeErr(w, http.StatusBadRequest, "File too large")
				return
			}
			break
		}
	}
	if !foundFile {
		writeErr(w, http.StatusBadRequest, "Choose a brochure file")
		return
	}

	if len(data) == 0 {
		writeErr(w, http.StatusBadRequest, "File is empty")
		return
	}

	isPDF := bytes.HasPrefix(data, []byte("%PDF-"))
	isText := utf8.Valid(data) && !bytes.Contains(data, []byte{0})
	if !isPDF && !isText {
		writeErr(w, http.StatusBadRequest, "Brochure must be a PDF or a text file")
		return
	}

	rawName = strings.TrimSpace(rawName)
	if rawName == "" {
		writeErr(w, http.StatusBadRequest, "Choose a brochure file")
		return
	}

	dir := filepath.Join(s.dataDir, user.ID, job.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not store the brochure")
		return
	}
	dst := filepath.Join(dir, "body")
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not store the brochure")
		return
	}

	job.Filename = rawName
	job.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateJob(r.Context(), job); err != nil {
		_ = os.Remove(dst)
		writeErr(w, http.StatusInternalServerError, "Could not store the brochure")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	job, ok := s.load(w, r, user.ID)
	if !ok {
		return
	}
	if strings.TrimSpace(job.Filename) == "" {
		writeErr(w, http.StatusBadRequest, "Attach a brochure first")
		return
	}
	if job.Status != board.StatusDraft {
		writeJSON(w, http.StatusOK, job)
		return
	}

	jobs, err := s.store.ListJobs(r.Context(), user.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not check sheets")
		return
	}
	for _, other := range jobs {
		if other.ID != job.ID && other.Status == board.StatusRunning {
			writeErr(w, http.StatusConflict, "Another job is already running")
			return
		}
	}

	job.Status = board.StatusRunning
	job.RunID = board.NewID()
	job.Error = ""
	for i := range job.Checklist {
		job.Checklist[i].State = board.StepStatePending
	}
	job.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateJob(r.Context(), job); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not start the sheet")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	user, ok := s.current(w, r)
	if !ok {
		return
	}
	job, ok := s.load(w, r, user.ID)
	if !ok {
		return
	}
	if strings.TrimSpace(job.Filename) == "" {
		writeErr(w, http.StatusNotFound, "Not found")
		return
	}
	f, err := os.Open(filepath.Join(s.dataDir, user.ID, job.ID, "body"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "Not found")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Disposition", "attachment; filename="+strconvQuote(filepath.Base(job.Filename)))
	http.ServeContent(w, r, filepath.Base(job.Filename), job.UpdatedAt, f)
}

func (s *Server) load(w http.ResponseWriter, r *http.Request, userID string) (board.Job, bool) {
	job, err := s.store.Job(r.Context(), r.PathValue("id"))
	if errors.Is(err, board.ErrNotFound) || (err == nil && (job.UserID != userID || userID == "")) {
		writeErr(w, http.StatusNotFound, "Not found")
		return board.Job{}, false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not load the sheet")
		return board.Job{}, false
	}
	return job, true
}

func (s *Server) current(w http.ResponseWriter, r *http.Request) (board.User, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		writeErr(w, http.StatusUnauthorized, "Sign in")
		return board.User{}, false
	}
	user, err := s.store.UserByToken(r.Context(), cookie.Value)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "Sign in")
		return board.User{}, false
	}
	return user, true
}

func (s *Server) openSession(w http.ResponseWriter, r *http.Request, user board.User) error {
	token := board.NewToken()
	if err := s.store.CreateSession(r.Context(), user.ID, token, time.Now().Add(24*time.Hour)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400,
	})
	return nil
}

func filenameFromHeader(disposition, fallback string) string {
	_, params, err := mime.ParseMediaType(disposition)
	if err == nil {
		if name := strings.TrimSpace(params["filename"]); name != "" {
			return name
		}
	}
	return fallback
}

func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	return dec.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func strconvQuote(value string) string {
	b, _ := json.Marshal(value)
	return string(b)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin == "http://localhost:5173" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
